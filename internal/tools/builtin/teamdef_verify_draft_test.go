package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// verifyDraftReport is what verify answers for an overlay.
type verifyDraftReport struct {
	Valid         bool             `json:"valid"`
	Runnable      bool             `json:"runnable"`
	CheckedAs     string           `json:"checked_as"`
	ParentDefID   string           `json:"parent_def_id"`
	ContentSHA256 string           `json:"content_sha256"`
	Matches       bool             `json:"matches"`
	Deployed      bool             `json:"deployed"`
	Issues        []map[string]any `json:"issues"`
}

func verifyDraft(t *testing.T, tool *TeamDef, ctx context.Context, name, overlay, extra string) verifyDraftReport {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"`+name+`","overlay":`+overlay+extra+`}`))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.IsError {
		t.Fatalf("verify of a draft must report, not refuse; got error %s", res.Text)
	}
	var out verifyDraftReport
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %s: %v", res.Text, err)
	}
	return out
}

// issueOf returns the first issue of kind, or fails.
func issueOf(t *testing.T, r verifyDraftReport, kind string) map[string]any {
	t.Helper()
	for _, i := range r.Issues {
		if i["kind"] == kind {
			return i
		}
	}
	t.Fatalf("no %s issue in %v", kind, r.Issues)
	return nil
}

// versionCount is how many versions the store holds under name — the
// before/after a check must leave unchanged.
func versionCount(t *testing.T, tool *TeamDef, ctx context.Context, name string) int {
	t.Helper()
	rows, err := tool.Store.TeamDefListByName(ctx, name)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(rows)
}

// G19 acceptance 1: an undeclared "./channel" names its state and field.
func TestTeamDefVerifyDraft_UndeclaredLocalChannelNamesItsStateAndField(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	overlay := `{"entry":"post","states":[{"state":"post","handler":{"kind":"channel","channel":"./events"}},
	  {"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"post","to":"done","on":"success"}]}`
	r := verifyDraft(t, tool, ctx, "notifier", overlay, "")
	if r.Valid || r.Runnable {
		t.Fatalf("want valid:false runnable:false, got %+v", r)
	}
	i := issueOf(t, r, teamIssueLocalChannelMissing)
	if i["severity"] != severityRefused || i["state"] != "post" || i["path"] != "states[0].handler.channel" {
		t.Errorf("issue should be a refusal at states[0].handler.channel in state post; got %v", i)
	}
	// Listed once: Validate refuses it and the sweep would report it too.
	n := 0
	for _, i := range r.Issues {
		if i["kind"] == teamIssueLocalChannelMissing {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the undeclared channel is reported %d times, want once: %v", n, r.Issues)
	}
}

// G19 acceptance 2: an ACL entry outside the author's own allowlist, with the
// create refusal's own words.
func TestTeamDefVerifyDraft_ACLBeyondTheAuthorIsRefusedWithTheCreateText(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	overlay := strings.TrimSuffix(starterGraph, "}") + `,"channels":{"publish":["verdicts","audit"],"subscribe":["pr-events"]}}`
	r := verifyDraft(t, tool, ctx, "triage", overlay, "")
	if r.Valid {
		t.Fatalf("want valid:false, got %+v", r)
	}
	i := issueOf(t, r, teamIssueChannelAuthority)
	if i["path"] != "channels.publish[1]" || i["channel"] != "audit" {
		t.Errorf("issue should point at channels.publish[1] (audit); got %v", i)
	}
	res := teamOp(t, tool, ctx, "create", "triage", overlay)
	if !res.IsError || res.Text != "create: "+i["detail"].(string) {
		t.Errorf("create should refuse with exactly the issue's detail:\n create: %s\n detail: %v", res.Text, i["detail"])
	}
}

// G19 acceptance 3: a local agent body the agent gates refuse names local.agents.<name>.
func TestTeamDefVerifyDraft_RefusedLocalAgentBodyNamesItsPath(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	r := verifyDraft(t, tool, ctx, "sdlc", localTeam(`{"tier":"middle","tools":["Bash"]}`), "")
	if r.Valid {
		t.Fatalf("an agent wider than its author must be refused; got %+v", r)
	}
	i := issueOf(t, r, teamIssueLocalAgentInvalid)
	if i["path"] != "local.agents.reviewer" || i["agent"] != "./reviewer" {
		t.Errorf("issue should name local.agents.reviewer; got %v", i)
	}
}

// G19 acceptance 4: a team that saves but names a member that does not
// resolve is valid and not runnable.
func TestTeamDefVerifyDraft_MissingMemberIsValidButNotRunnable(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.AgentExists = func(_ context.Context, name string) bool { return name != "reviewer" }
	r := verifyDraft(t, tool, ctx, "solo", validTeamGraph, "")
	if !r.Valid || r.Runnable {
		t.Fatalf("want valid:true runnable:false, got %+v", r)
	}
	i := issueOf(t, r, teamIssueAgentMissing)
	if i["severity"] != severityUnrunnable || i["path"] != "states[0].handler.agent" {
		t.Errorf("want an unrunnable issue at states[0].handler.agent; got %v", i)
	}
}

// G19 acceptance 5 + 6: several problems in one draft are all reported, in the
// order a save checks them, and nothing is written.
func TestTeamDefVerifyDraft_ReportsEveryProblemInSaveOrderAndWritesNothing(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{Publish: []string{"verdicts"}})
	tool.AgentExists = func(context.Context, string) bool { return false }
	// A graph error (a dead-end state), an ACL entry the author does not hold,
	// a local agent wider than its author, and a global member that does not
	// resolve.
	overlay := `{"entry":"review","channels":{"publish":["audit"]},
	  "local":{"agents":{"reviewer":{"tier":"middle","tools":["Bash"]}}},
	  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},
	            {"state":"judge","handler":{"kind":"agent","agent":"judge"}},
	            {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"review","to":"judge","on":"success"},{"from":"review","to":"done","on":"pushback:skip"}]}`
	before := versionCount(t, tool, ctx, "sdlc")
	r := verifyDraft(t, tool, ctx, "sdlc", overlay, "")
	if after := versionCount(t, tool, ctx, "sdlc"); after != before {
		t.Fatalf("verify wrote a version: %d → %d", before, after)
	}
	var got []string
	for _, i := range r.Issues {
		got = append(got, i["kind"].(string))
	}
	want := []string{teamIssueGraphInvalid, teamIssueChannelAuthority, teamIssueLocalAgentInvalid, teamIssueAgentMissing}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("issues = %v, want %v (in that order)", got, want)
	}
	if r.Valid || r.Runnable || r.CheckedAs != "create" {
		t.Errorf("want valid:false runnable:false checked_as:create; got %+v", r)
	}
	// The save refuses with the first of them, word for word.
	res := teamOp(t, tool, ctx, "create", "sdlc", overlay)
	if !res.IsError || res.Text != "create: "+r.Issues[0]["detail"].(string) {
		t.Errorf("create should refuse with the first issue:\n create: %s\n first:  %v", res.Text, r.Issues[0]["detail"])
	}
}

// A save of a name that already has a version is a fork, so verify checks a
// draft as one by default — merged over the active version — and reports
// whether the draft is what is deployed.
func TestTeamDefVerifyDraft_ChecksAsAForkOfTheActiveVersionByDefault(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	created := createTeam(t, tool, ctx, "triage", validTeamGraph)

	// An overlay a fork would merge: only colours, which the hash excludes.
	r := verifyDraft(t, tool, ctx, "triage", `{"colors":{"states":{"review":"#eee"}}}`, "")
	if r.CheckedAs != "fork" || r.ParentDefID != created["def_id"] || !r.Valid {
		t.Fatalf("want a valid fork of %v; got %+v", created["def_id"], r)
	}
	if !r.Matches || r.ContentSHA256 != created["content_sha256"] {
		t.Errorf("a colours-only draft is the deployed team: want matches with %v; got %+v", created["content_sha256"], r)
	}

	// as:create checks the overlay alone — which, without the parent's graph,
	// has no entry.
	r = verifyDraft(t, tool, ctx, "triage", `{"colors":{"states":{"review":"#eee"}}}`, `,"as":"create"`)
	if r.CheckedAs != "create" || r.Valid {
		t.Errorf("as:create must not merge over the parent; got %+v", r)
	}
}

// A draft is reported, never refused, even when it cannot be built.
func TestTeamDefVerifyDraft_UnbuildableDraftIsAReport(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	r := verifyDraft(t, tool, ctx, "triage", `{"states":"not-a-list"}`, "")
	if r.Valid || len(r.Issues) != 1 || r.Issues[0]["kind"] != teamIssueOverlayInvalid {
		t.Errorf("want one overlay_invalid issue; got %+v", r)
	}
	r = verifyDraft(t, tool, ctx, "triage", validTeamGraph, `,"as":"fork"`)
	if r.Valid || r.CheckedAs != "fork" || r.Issues[0]["kind"] != teamIssueParentNotFound {
		t.Errorf("a fork of a name with no version: want parent_not_found; got %+v", r)
	}
}

// The inputs that only make sense for a draft are refused without one, and a
// hash with an overlay is ambiguous.
func TestTeamDefVerify_RefusesDraftInputsWithoutAnOverlay(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	for _, in := range []string{
		`{"op":"verify","name":"x","as":"fork"}`,
		`{"op":"verify","name":"x","overlay":{},"content_sha256":"sha256:x"}`,
		`{"op":"verify","name":"x","overlay":{"entry":"a"},"as":"replace"}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		if !res.IsError {
			t.Errorf("%s: accepted, want a refusal; got %s", in, res.Text)
		}
	}
}

// Equivalence: over every refusal shape this file builds, a save is refused
// exactly when verify says valid:false, with the first refusal's text.
func TestTeamDefVerifyDraft_ValidIffTheSaveIsAccepted(t *testing.T) {
	for name, tc := range map[string]struct {
		overlay string
		team    string
	}{
		"valid":                {validTeamGraph, "v"},
		"no entry":             {`{"states":[{"state":"a","handler":{"kind":"terminal"}}]}`, "e"},
		"unknown kind":         {strings.Replace(validTeamGraph, `"kind": "agent"`, `"kind": "robot"`, 1), "k"},
		"undeclared ./channel": {`{"entry":"p","states":[{"state":"p","handler":{"kind":"channel","channel":"./x"}},{"state":"d","handler":{"kind":"terminal"}}],"transitions":[{"from":"p","to":"d","on":"success"}]}`, "c"},
		"reserved channel":     {`{"entry":"p","states":[{"state":"p","handler":{"kind":"channel","channel":"_team/other/x"}},{"state":"d","handler":{"kind":"terminal"}}],"transitions":[{"from":"p","to":"d","on":"success"}]}`, "r"},
		"starter without ACL":  {starterGraph, "s"},
	} {
		t.Run(name, func(t *testing.T) {
			tool, ctx, cleanup := teamDefFixture(t)
			defer cleanup()
			r := verifyDraft(t, tool, ctx, tc.team, tc.overlay, "")
			res := teamOp(t, tool, ctx, "create", tc.team, tc.overlay)
			if r.Valid == res.IsError {
				t.Fatalf("verify valid=%v but create error=%v (%s)", r.Valid, res.IsError, res.Text)
			}
			if res.IsError {
				if first := r.Issues[0]; res.Text != "create: "+first["detail"].(string) {
					t.Errorf("create refused with %q, verify's first refusal is %q", res.Text, first["detail"])
				}
			}
		})
	}
}

// Stored-mode verify keeps its shape, and its issues gain a severity and a path.
func TestTeamDefVerify_StoredIssuesCarrySeverityAndPath(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	createTeam(t, tool, ctx, "solo", validTeamGraph)
	tool.AgentExists = func(context.Context, string) bool { return false }
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"solo"}`))
	out := decodeResult(t, res.Text)
	if out["runnable"] != false {
		t.Fatalf("want runnable:false; got %v", out)
	}
	if _, has := out["valid"]; has {
		t.Errorf("stored verify must keep its shape — no valid field; got %v", out)
	}
	issue := out["issues"].([]any)[0].(map[string]any)
	if issue["severity"] != severityUnrunnable || issue["path"] != "states[0].handler.agent" || issue["state"] != "review" {
		t.Errorf("want severity+path on a stored issue; got %v", issue)
	}
}
