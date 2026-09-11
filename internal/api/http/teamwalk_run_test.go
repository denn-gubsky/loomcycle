package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestOpenTeamWalkRun_GrantsWhatThePauseMachineryNeeds is the regression for
// the defect this whole change exists to fix.
//
// loomboard drives loomcycle over POST /v1/_teamdef, which dispatches under
// substrateAdminCtx — a plane with NO run id and NO Interruption policy. A
// breakpoint armed there could not be answered, and a pause that cannot be
// answered fails safe to ABORT, so arming one killed the walk instead of
// pausing it. Both halves are asserted because either one alone still aborts.
func TestOpenTeamWalkRun_GrantsWhatThePauseMachineryNeeds(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	// The plane the canvas actually arrives on.
	base := substrateAdminCtx(context.Background())
	if tools.RunID(base) != "" {
		t.Fatal("the substrate plane already had a run id — this test no longer covers the gap")
	}
	if tools.InterruptionPolicy(base).Enabled {
		t.Fatal("the substrate plane already granted Interruption — this test no longer covers the gap")
	}

	walkCtx, runID, finish, err := srv.openTeamWalkRun(base, "triage", false)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	if runID == "" {
		t.Fatal("no run id — the breakpoint set and the ask are both keyed on it")
	}
	if got := tools.RunID(walkCtx); got != runID {
		t.Errorf("ctx run id = %q, want %q", got, runID)
	}
	pol := tools.InterruptionPolicy(walkCtx)
	if !pol.Enabled {
		t.Error("Interruption is not enabled on the walk ctx — every pause would error, and an unanswerable pause ABORTS the walk")
	}
	if len(pol.Kinds) != 1 || pol.Kinds[0] != "question" {
		t.Errorf("Interruption kinds = %v, want just [question] — a breakpoint asks nothing else", pol.Kinds)
	}

	// The run is real and addressable, filed under the team.
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("the walk's run row is not readable: %v", err)
	}
	if run.AgentID != teamWalkAgentPrefix+"triage" {
		t.Errorf("run agent = %q, want %q", run.AgentID, teamWalkAgentPrefix+"triage")
	}
	if run.Status != store.RunRunning {
		t.Errorf("run status = %q, want running", run.Status)
	}

	// And finishing records the outcome, so a failed walk is never left looking
	// like one still going.
	finish(context.DeadlineExceeded)
	run, err = srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun after finish: %v", err)
	}
	if run.Status != store.RunFailed {
		t.Errorf("status after a failed walk = %q, want failed", run.Status)
	}
	// The message the walk failed with reaches the row, so a failed walk is
	// never indistinguishable from one still going.
	if run.ErrorMsg == "" {
		t.Error("the walk's error was not recorded on the run")
	}
}

// TestOpenTeamWalkRun_DetachSurvivesTheRequestCtx: a detached walk must keep
// the request's ctx VALUES (auth principal, tenant, admission) while no longer
// dying when the handler returns — the same trade an interactive run makes.
func TestOpenTeamWalkRun_DetachSurvivesTheRequestCtx(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	reqCtx, cancel := context.WithCancel(substrateAdminCtx(context.Background()))
	walkCtx, _, _, err := srv.openTeamWalkRun(reqCtx, "triage", true)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	cancel() // the handler returns
	if walkCtx.Err() != nil {
		t.Fatal("a detached walk ctx died with the request — the walk would be killed the moment the caller got its run id")
	}
	// Values survive: the identity the walk spawns its members under.
	if tools.RunIdentity(walkCtx).AgentID == "" {
		t.Error("the detached ctx lost its run identity")
	}

	// A NON-detached walk keeps the request's lifetime, so a disconnect still
	// tears it down.
	reqCtx2, cancel2 := context.WithCancel(substrateAdminCtx(context.Background()))
	walkCtx2, _, _, err := srv.openTeamWalkRun(reqCtx2, "triage", false)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	cancel2()
	if walkCtx2.Err() == nil {
		t.Error("a synchronous walk outlived its request ctx")
	}
}
