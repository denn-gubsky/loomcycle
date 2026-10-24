package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.2 — a team walk's ops ask the team they act on: run asks run
// team:<name>, poll asks read, cancel asks cancel. Each is judged by the
// run's ACL (the originating principal's).

// walkACLFixture is pollFixture with a second team, "other", and the def_id
// of "rev".
func walkACLFixture(t *testing.T) (*TeamDef, context.Context, *tools.Background, *gatedWalk, string, func()) {
	t.Helper()
	tool, ctx, bg, g, _, done := pollFixture(t)
	createTeam(t, tool, ctx, "other", validTeamGraph)
	got := runTD(t, tool, ctx, `{"op":"list","name":"rev"}`)
	revID := firstDefID(t, got)
	return tool, ctx, bg, g, revID, func() {
		close(g.release)
		done()
	}
}

func firstDefID(t *testing.T, res tools.Result) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	rows, _ := decodeResult(t, res.Text)["versions"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no versions in %s", res.Text)
	}
	id, _ := rows[0].(map[string]any)["def_id"].(string)
	if id == "" {
		t.Fatalf("no def_id in %s", res.Text)
	}
	return id
}

func TestTeamDefTool_InRunRunDenyRefusesThatTeamOnly(t *testing.T) {
	tool, ctx, bg, g, revID, done := walkACLFixture(t)
	defer done()
	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"run"}, Resources: []string{"team:rev"}}))

	for _, in := range []string{
		`{"op":"run","name":"rev","input":"x","mode":"poll"}`,
		// The op runs the id, so it is asked of the team the id is.
		`{"op":"run","def_id":"` + revID + `","input":"x","mode":"poll"}`,
		`{"op":"run","name":"other","def_id":"` + revID + `","input":"x","mode":"poll"}`,
		// A waited-for run starts the walk too.
		`{"op":"run","name":"rev","input":"x"}`,
	} {
		// Bounded: a run the check lets through waits on its gated member.
		call, cancel := context.WithTimeout(run, 2*time.Second)
		res := runTD(t, tool, call, in)
		cancel()
		if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "team:rev") {
			t.Errorf("%s: want acl_denied naming team:rev, got %s", in, res.Text)
		}
	}
	if s, _ := g.counts(); s != 0 {
		t.Fatalf("%d members started under a refused run", s)
	}
	if views, _ := bg.Select(nil, ""); len(views) != 0 {
		t.Fatalf("a refused run left %d walks in the table", len(views))
	}
	res := runTD(t, tool, run, `{"op":"run","name":"other","input":"x","mode":"poll"}`)
	if res.IsError {
		t.Fatalf("run of a team the deny does not name: %s", res.Text)
	}
	// A deny on run is not a deny on the definition.
	if res := runTD(t, tool, run, `{"op":"get","def_id":"`+revID+`"}`); res.IsError {
		t.Errorf("a deny on run team:rev refused reading its definition: %s", res.Text)
	}
}

func TestTeamDefTool_InRunCancelDenyLeavesThatTeamsWalkRunning(t *testing.T) {
	tool, ctx, bg, _, _, done := walkACLFixture(t)
	defer done()
	rev := startPollWalk(t, tool, ctx, "")
	other := mustPollWalk(t, tool, ctx, "other")
	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"cancel"}, Resources: []string{"team:rev"}}))

	for _, ids := range []string{`"` + rev + `"`, `"` + other + `","` + rev + `"`} {
		res := runTD(t, tool, run, `{"op":"cancel","run_ids":[`+ids+`]}`)
		if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "team:rev") {
			t.Errorf("cancel [%s]: want acl_denied naming team:rev, got %s", ids, res.Text)
		}
	}
	for _, id := range []string{rev, other} {
		if v, _ := bg.Lookup(id); v.Ended() {
			t.Fatalf("walk %s ended under a refused cancel: %s", id, v.State)
		}
	}
	res := runTD(t, tool, run, `{"op":"cancel","run_ids":["`+other+`"]}`)
	if res.IsError {
		t.Fatalf("cancel of a team the deny does not name: %s", res.Text)
	}
	if v, _ := bg.Lookup(other); v.State != tools.ChildCancelled {
		t.Errorf("other's walk after its cancel = %s, want cancelled", v.State)
	}
	// A deny on cancel is not a deny on reading the walk.
	if res := runTD(t, tool, run, `{"op":"poll","run_ids":["`+rev+`"]}`); res.IsError {
		t.Errorf("a deny on cancel refused a poll: %s", res.Text)
	}
}

func TestTeamDefTool_InRunPollNeedsReadOfEachWalksTeam(t *testing.T) {
	tool, ctx, _, _, _, done := walkACLFixture(t)
	defer done()
	rev := startPollWalk(t, tool, ctx, "")
	other := mustPollWalk(t, tool, ctx, "other")
	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"team:rev"}}))

	// Named, and unnamed: a poll of every unread walk reads rev's too.
	for _, in := range []string{`{"op":"poll","run_ids":["` + rev + `"]}`, `{"op":"poll"}`, `{"op":"poll","wait":"all","wait_ms":1}`} {
		res := runTD(t, tool, run, in)
		if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "read team:rev") {
			t.Errorf("%s: want acl_denied naming read team:rev, got %s", in, res.Text)
		}
	}
	if p := decodeWalkPoll(t, runTD(t, tool, run, `{"op":"poll","run_ids":["`+other+`"]}`)); len(p.Walks) != 1 {
		t.Errorf("poll of a team the deny does not name = %+v", p)
	}
	// A deny on read is not a deny on running the team.
	if res := runTD(t, tool, run, `{"op":"run","name":"rev","input":"x","mode":"poll"}`); res.IsError {
		t.Errorf("a deny on read refused a run: %s", res.Text)
	}
}

// mustPollWalk runs team name in poll mode and returns its run id.
func mustPollWalk(t *testing.T, tool *TeamDef, ctx context.Context, name string) string {
	t.Helper()
	res := runTD(t, tool, ctx, `{"op":"run","name":"`+name+`","input":"the diff","mode":"poll"}`)
	if res.IsError {
		t.Fatalf("poll-mode run of %s: %s", name, res.Text)
	}
	id, _ := decodeResult(t, res.Text)["run_id"].(string)
	if id == "" {
		t.Fatalf("no run_id in %s", res.Text)
	}
	return id
}

// Agent poll and cancel read and end a walk like any background child, so
// they ask its team what TeamDef poll and cancel ask; a sub-agent child asks
// nothing of team:.
func TestAgentTool_PollAndCancelOfAWalkAskItsTeam(t *testing.T) {
	tool, ctx, bg, _, _, done := walkACLFixture(t)
	defer done()
	rev := startPollWalk(t, tool, ctx, "")
	other := mustPollWalk(t, tool, ctx, "other")
	a := pollTool(newGated())
	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read", "cancel"}, Resources: []string{"team:rev"}}))

	for _, in := range []string{
		`{"op":"poll","child_run_ids":["` + rev + `"]}`,
		`{"op":"poll"}`,
		`{"op":"cancel","child_run_ids":["` + rev + `"]}`,
		`{"op":"cancel","child_run_ids":["` + other + `","` + rev + `"]}`,
	} {
		res := execJSON(t, a, run, in)
		if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "team:rev") {
			t.Errorf("Agent %s: want acl_denied naming team:rev, got %s", in, res.Text)
		}
	}
	for _, id := range []string{rev, other} {
		if v, _ := bg.Lookup(id); v.Ended() {
			t.Fatalf("walk %s ended under a refused cancel: %s", id, v.State)
		}
	}
	if p := decodePoll(t, execJSON(t, a, run, `{"op":"poll","child_run_ids":["`+other+`"]}`)); len(p.Children) != 1 {
		t.Errorf("Agent poll of a team the deny does not name = %+v", p)
	}
	if res := execJSON(t, a, run, `{"op":"cancel","child_run_ids":["`+other+`"]}`); res.IsError {
		t.Errorf("Agent cancel of a team the deny does not name: %s", res.Text)
	}
}
