package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/breakpoints"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// seedRun creates a session + run the tenant gate will accept.
func seedRun(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "agent-x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-x", UserID: "alice"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
}

func armedFrom(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		RunID string   `json:"run_id"`
		Armed []string `json:"armed"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out.Armed
}

// TestHandleBreakpoints_ArmsAWalkThatIsAlreadyRunning is the whole point: the
// walk registered with no breakpoints, and an operator arms one afterwards.
func TestHandleBreakpoints_ArmsAWalkThatIsAlreadyRunning(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)

	// A walk starts with NOTHING armed — the ordinary case.
	set, release, err := srv.breakpointReg.Open(runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Fatal("a walk with no run argument started armed")
	}

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave:before_dispatch"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := armedFrom(t, rec.Body.String()); len(got) != 1 || got[0] != "wave:before_dispatch" {
		t.Errorf("armed = %v", got)
	}
	// The LIVE set the walk is consulting changed — not a copy.
	if !set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Error("the walk's own set did not see the arming")
	}
	if set.Armed("wave", breakpoints.AfterCollection) {
		t.Error("a phase-qualified spec armed the other phase too")
	}
}

// TestHandleBreakpoints_GetReadsBackCanonically: a caller sees what the walk
// will do, not an echo of its shorthand.
func TestHandleBreakpoints_GetReadsBackCanonically(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	_, release, _ := srv.breakpointReg.Open(runID, []string{"wave"})
	defer release()

	rec := doJSON(t, srv, "GET", "/v1/runs/"+runID+"/breakpoints", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := armedFrom(t, rec.Body.String())
	if len(got) != 2 || got[0] != "wave:after_collection" || got[1] != "wave:before_dispatch" {
		t.Errorf("armed = %v, want both phases, canonical and sorted", got)
	}
}

// TestHandleBreakpoints_EmptyListTurnsDebugOff — the canvas's off switch.
func TestHandleBreakpoints_EmptyListTurnsDebugOff(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, _ := srv.breakpointReg.Open(runID, []string{"wave"})
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if len(armedFrom(t, rec.Body.String())) != 0 || set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Error("the empty list did not disarm")
	}
}

// TestHandleBreakpoints_RejectedPutKeepsThePreviousArming: an operator fixing a
// typo must not discover they have also disarmed what was working.
func TestHandleBreakpoints_RejectedPutKeepsThePreviousArming(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, _ := srv.breakpointReg.Open(runID, []string{"wave:before_dispatch"})
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["review","wave:typo"]}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Error("a rejected PUT disarmed the previous set")
	}
	if set.Armed("review", breakpoints.BeforeDispatch) {
		t.Error("a rejected PUT partially applied")
	}
}

// TestHandleBreakpoints_NoLiveWalkIs404: an arming that appeared to succeed and
// then never paused anything is the worst outcome for a debugger, so a run with
// no walk in flight fails loudly. A cross-tenant run folds into the SAME 404 —
// run_ids are not secret, so the gate must not become an existence oracle.
func TestHandleBreakpoints_NoLiveWalkIs404(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv) // a real run, but no walk registered

	for _, method := range []string{"GET", "PUT"} {
		rec := doJSON(t, srv, method, "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave"]}`)
		if rec.Code != 404 {
			t.Errorf("%s with no live walk = %d, want 404; body=%s", method, rec.Code, rec.Body.String())
		}
	}
	// An unknown run id is the same 404, not a different code that would say
	// "this run does not exist".
	rec := doJSON(t, srv, "GET", "/v1/runs/run-nope/breakpoints", "")
	if rec.Code != 404 {
		t.Errorf("unknown run = %d, want the same opaque 404", rec.Code)
	}
}

// TestBreakpointPhases_MatchTheRegistrys pins the two spellings of the phase
// against each other. The registry is a leaf and cannot import teamrun, so the
// constants are declared twice; if they ever diverge, every arming would
// silently stop matching and no other test would notice.
func TestBreakpointPhases_MatchTheRegistrys(t *testing.T) {
	if string(teamrun.BeforeDispatch) != breakpoints.BeforeDispatch {
		t.Errorf("before_dispatch spelled %q in teamrun and %q in breakpoints",
			teamrun.BeforeDispatch, breakpoints.BeforeDispatch)
	}
	if string(teamrun.AfterCollection) != breakpoints.AfterCollection {
		t.Errorf("after_collection spelled %q in teamrun and %q in breakpoints",
			teamrun.AfterCollection, breakpoints.AfterCollection)
	}
	// And the adapter actually bridges them.
	set, _ := breakpoints.NewSet([]string{"wave:after_collection"})
	var src teamrun.BreakpointSource = liveBreakpoints{set}
	if !src.Armed("wave", teamrun.AfterCollection) {
		t.Error("the adapter did not translate the phase")
	}
	if src.Armed("wave", teamrun.BeforeDispatch) {
		t.Error("the adapter armed the wrong phase")
	}
}
