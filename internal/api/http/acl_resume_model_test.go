package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The review's probe (RFC FY Part E): a tenant-wide `deny use model:*` bound
// while a run is paused refuses its resume before any model call. The resume
// resolved the run's model before it had compiled the caller's current
// rules, so nothing asked; and the restored starting model was never asked.
func TestACLResume_AModelDenyBoundDuringThePauseRefusesTheResume(t *testing.T) {
	e := keyspaceWriteFixture(t, authz.ModeCompat, `[{"effect":"deny","actions":["invoke"],"resources":["tool:Nope"]}]`,
		[][]providers.Event{endTurn(), endTurn()})
	run := e.run(t, "a_keeper_model_resume")
	bindTenantWide(t, e.st, "no-models", `[{"effect":"deny","actions":["use"],"resources":["model:*"]}]`)
	parkForResume(t, e.srv, run.ID)
	modelEvents := func() int {
		n := 0
		for _, typ := range runTranscriptEvents(t, e, run.ID) {
			if typ == "text" || typ == "usage" || typ == "tool_call" || typ == "done" {
				n++
			}
		}
		return n
	}
	before := modelEvents()
	n, warnings := e.srv.ResumePausedRuns(context.Background())
	if n != 0 || !strings.Contains(strings.Join(warnings, "\n"), "acl_denied") {
		t.Fatalf("resumed %d (warnings %v), want the resume refused with acl_denied", n, warnings)
	}
	if after := modelEvents(); after != before {
		t.Errorf("the refused resume wrote %d model events (%v): a model call was made", after-before, runTranscriptEvents(t, e, run.ID))
	}
}

func runTranscriptEvents(t *testing.T, e *keyspaceWriteEnv, runID string) []string {
	t.Helper()
	run, err := e.st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := e.st.GetTranscript(context.Background(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.RunID == runID {
			out = append(out, ev.Type)
		}
	}
	return out
}
