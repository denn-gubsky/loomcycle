package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TeamDef cancel ends a walk run in poll mode as Agent cancel does: the walk
// ends cancelled with its parent's reason, its member is cancelled with it,
// its run is finished with an error, and the answer reports that final
// state. A walk that had already finished is left as it ended.
func TestTeamDefCancel_EndsAPollWalkAndItsMemberAndReportsItsFinalState(t *testing.T) {
	tool, ctx, bg, g, rec, done := pollFixture(t)
	defer done()

	id := startPollWalk(t, tool, ctx, "")
	waitUntil(t, func() bool { s, _ := g.counts(); return s == 1 })
	c := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"cancel","run_ids":["`+id+`"]}`))
	if len(c.Walks) != 1 {
		t.Fatalf("cancel answered %+v, want one walk", c)
	}
	if w := c.Walks[0]; w["run_id"] != id || w["name"] != "rev" || w["state"] != "cancelled" ||
		!strings.Contains(w["error"].(string), "cancelled by its parent") {
		t.Errorf("cancel row = %v, want rev cancelled by its parent", w)
	}
	if _, ended := g.counts(); ended != 1 {
		t.Error("the walk's member did not end with it")
	}
	if _, err := rec.ended(); err == nil {
		t.Error("the cancelled walk's run was finished without an error")
	}
	if v, _ := bg.Lookup(id); v.State != tools.ChildCancelled {
		t.Errorf("table row = %s, want cancelled", v.State)
	}

	// A walk that completed before the cancel keeps its ending and its answer.
	g2 := newGatedWalk()
	tool.Spawn = g2.spawn()
	finished := startPollWalk(t, tool, ctx, "")
	close(g2.release)
	waitUntil(t, func() bool { v, _ := bg.Lookup(finished); return v.Ended() })
	c = decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"cancel","run_ids":["`+finished+`"]}`))
	if w := c.Walks[0]; w["state"] != "completed" || w["error"] != nil {
		t.Errorf("cancel of a finished walk = %v, want completed", w)
	}
	p := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+finished+`"]}`))
	if p.Walks[0]["final_output"] != "reviewer reviewed" {
		t.Errorf("poll after cancelling a finished walk = %v", p.Walks[0])
	}
}

// Only this run's poll-mode walks can be cancelled. A sub-agent of this run,
// a detached walk, another run's walk and an id nobody started are all
// answered the same way, and naming one beside a real walk cancels nothing.
func TestTeamDefCancel_RefusesEveryIdThatIsNotThisRunsWalkAndCancelsNothing(t *testing.T) {
	tool, ctx, bg, g, _, done := pollFixture(t)
	defer done()
	gated := newGated("sub")
	a := pollTool(gated)
	var sub pollStartRow
	if err := json.Unmarshal([]byte(execJSON(t, a, ctx, `{"op":"spawn","name":"w","prompt":"sub","mode":"poll"}`).Text), &sub); err != nil {
		t.Fatal(err)
	}
	det := decodeResult(t, runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"x","mode":"detach"}`).Text)["run_id"].(string)
	other, _ := withParentRun(ctx, context.Background())
	otherWalk := startPollWalk(t, tool, other, "")
	mine := startPollWalk(t, tool, ctx, "")

	for _, id := range []string{sub.ChildRunID, det, otherWalk, "r_nobody"} {
		res := runTD(t, tool, ctx, `{"op":"cancel","run_ids":["`+mine+`","`+id+`"]}`)
		if !res.IsError || !strings.Contains(res.Text, "not a team walk of this run: "+id) || strings.Contains(res.Text, mine) {
			t.Errorf("cancel naming %s = %+v, want refused naming only it", id, res)
		}
	}
	if v, _ := bg.Lookup(mine); v.Ended() {
		t.Errorf("this run's walk was cancelled by a refused call: %s", v.State)
	}
	if v, _ := bg.Lookup(sub.ChildRunID); v.Ended() {
		t.Errorf("the sub-agent was cancelled: %s", v.State)
	}
	if res := runTD(t, tool, ctx, `{"op":"cancel"}`); !res.IsError || !strings.Contains(res.Text, "run_ids") {
		t.Errorf("cancel naming nothing = %+v, want refused", res)
	}
	if res := runTD(t, tool, tools.WithBackground(ctx, nil), `{"op":"cancel","run_ids":["`+mine+`"]}`); !res.IsError ||
		!strings.Contains(res.Text, "not a team walk of this run") {
		t.Errorf("cancel outside a run = %+v, want the unknown-id answer", res)
	}
	close(g.release)
	gated.open("sub")
	for _, id := range []string{mine, sub.ChildRunID} {
		waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
	}
	waitUntil(t, func() bool { _, e := g.counts(); return e == 3 })
}
