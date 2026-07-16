package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/turncancel"
)

// turnCancelFixture reuses the channel fan fixture's store + auth token and wires
// the steer + turn-cancel registries the handler needs.
func turnCancelFixture(t *testing.T) (*Server, func()) {
	t.Helper()
	srv, cleanup := channelFanFixture(t)
	srv.SetSteerRegistry(steer.NewRegistry(4))
	srv.turnCancelReg = turncancel.NewRegistry()
	// The fixture replaces the NewServer-built registry, so re-wire the cause
	// builder it needs to fire (NewServer does this in production).
	srv.turnCancelReg.SetCauseFor(loop.TurnCancelCause)
	return srv, cleanup
}

// POST /v1/runs/{id}/cancel fires the run's armed per-turn token (delivering the
// operator reason via the cancel cause) and reports stopped/parked.
func TestHandleCancelTurn_FiresArmedTokenAndParks(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: "run-1"}) // no SessionID → ownership gate skipped
	defer dereg()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	srv.turnCancelReg.Arm("run-1", cancel)

	rec := doJSON(t, srv, "POST", "/v1/runs/run-1/cancel", `{"reason":"too slow"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		RunID   string `json:"run_id"`
		Stopped bool   `json:"stopped"`
		Parked  bool   `json:"parked"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Stopped || !out.Parked || out.RunID != "run-1" {
		t.Errorf("response = %+v, want {run-1, stopped, parked}", out)
	}

	// The armed turn ctx was cancelled with the turn-cancel cause + the reason.
	if ctx.Err() == nil {
		t.Fatal("armed turn ctx was not cancelled")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, loop.ErrTurnCancelled) {
		t.Errorf("cause = %v, want ErrTurnCancelled", cause)
	}
	if !strings.Contains(cause.Error(), "too slow") {
		t.Errorf("cause %q did not carry the operator reason", cause.Error())
	}
	// Token consumed → no longer armed.
	if srv.turnCancelReg.IsArmed("run-1") {
		t.Error("token still armed after a successful cancel")
	}
}

// A second cancel once the turn has been stopped is a no-op 409 (idempotent).
func TestHandleCancelTurn_SecondCallIs409(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: "run-1"})
	defer dereg()
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	srv.turnCancelReg.Arm("run-1", cancel)

	if rec := doJSON(t, srv, "POST", "/v1/runs/run-1/cancel", `{}`); rec.Code != 200 {
		t.Fatalf("first cancel status = %d, want 200", rec.Code)
	}
	rec := doJSON(t, srv, "POST", "/v1/runs/run-1/cancel", `{}`)
	if rec.Code != 409 {
		t.Fatalf("second cancel status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "not_mid_turn" {
		t.Errorf("code = %q, want not_mid_turn", code)
	}
}

// A live but NOT-mid-turn run (registered, unarmed, no run row) → 409 not_mid_turn.
func TestHandleCancelTurn_409WhenNotMidTurn(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: "run-1"})
	defer dereg()

	rec := doJSON(t, srv, "POST", "/v1/runs/run-1/cancel", `{}`)
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "not_mid_turn" {
		t.Errorf("code = %q, want not_mid_turn", code)
	}
}

// A cancel on a non-existent live run → opaque 404 (no existence oracle).
func TestHandleCancelTurn_404WhenNoLiveRun(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()

	rec := doJSON(t, srv, "POST", "/v1/runs/ghost/cancel", `{}`)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404 (no in-flight run); body=%s", rec.Code, rec.Body.String())
	}
}

// A turn-cancel on a NON-interactive run → 409 not_interactive (stopping its only
// turn would terminate it — use whole-run cancel instead).
func TestHandleCancelTurn_409NonInteractive(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "agent-x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Default RunIdentity → Interactive:false.
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-x", UserID: "alice"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: run.ID}) // registered but never armed
	defer dereg()

	rec := doJSON(t, srv, "POST", "/v1/runs/"+run.ID+"/cancel", `{}`)
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "not_interactive" {
		t.Errorf("code = %q, want not_interactive", code)
	}
}

// The turn-cancel route requires runs:create (same as steer/resolve/compact), and
// the whole-run cancel route is unchanged.
func TestRequiredScopeFor_TurnCancel(t *testing.T) {
	if got := requiredScopeFor(http.MethodPost, "/v1/runs/run-1/cancel"); got != auth.ScopeRunsCreate {
		t.Errorf("turn-cancel scope = %q, want %q", got, auth.ScopeRunsCreate)
	}
	// Whole-run cancel (a distinct path) still maps to runs:create — unchanged.
	if got := requiredScopeFor(http.MethodPost, "/v1/agents/ag-1/cancel"); got != auth.ScopeRunsCreate {
		t.Errorf("whole-run cancel scope = %q, want %q", got, auth.ScopeRunsCreate)
	}
}

// stubClusterCanceller records the cross-replica delegation from the handler's
// fallback path.
type stubClusterCanceller struct {
	calls  int
	runID  string
	reason string
	found  bool
	err    error
}

func (s *stubClusterCanceller) CancelRemote(_ context.Context, runID, reason string) (bool, error) {
	s.calls++
	s.runID, s.reason = runID, reason
	return s.found, s.err
}

// A cross-replica turn-cancel: the run is NOT live on this replica (steer
// registry miss) but exists in the shared store — the handler gates via the store
// and routes to the owning replica via the cluster canceller, returning 200.
func TestHandleCancelTurn_RoutesCrossReplicaAndParks(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "agent-x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-x", UserID: "alice", Interactive: true})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Deliberately NOT registered in the steer registry → the handler takes the
	// cross-replica fallback and delegates to the cluster canceller.
	stub := &stubClusterCanceller{found: true}
	srv.turnCancelReg.SetClusterCanceller(stub)

	rec := doJSON(t, srv, "POST", "/v1/runs/"+run.ID+"/cancel", `{"reason":"remote stop"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.calls != 1 || stub.runID != run.ID || stub.reason != "remote stop" {
		t.Fatalf("CancelRemote calls=%d runID=%q reason=%q, want 1/%s/remote stop", stub.calls, stub.runID, stub.reason, run.ID)
	}
}

// A cross-replica cancel that the owning replica did not fire (parked / gone) →
// 404 (no in-flight turn to cancel), and a cross-tenant/unknown run never reaches
// the cluster canceller (opaque 404 at the store gate — no existence oracle).
func TestHandleCancelTurn_CrossReplicaNotFired_404(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "agent-x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-x", UserID: "alice", Interactive: true})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stub := &stubClusterCanceller{found: false}
	srv.turnCancelReg.SetClusterCanceller(stub)

	if rec := doJSON(t, srv, "POST", "/v1/runs/"+run.ID+"/cancel", `{}`); rec.Code != 404 {
		t.Fatalf("not-fired remote status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// Unknown run → opaque 404 at the store gate, cluster canceller untouched.
	if rec := doJSON(t, srv, "POST", "/v1/runs/ghost/cancel", `{}`); rec.Code != 404 {
		t.Fatalf("unknown run status = %d, want 404", rec.Code)
	}
	if stub.calls != 1 {
		t.Fatalf("cluster canceller calls=%d, want 1 (only the existing run reaches it)", stub.calls)
	}
}

// Single-process (no coordinator wired): an existing run that isn't live on this
// replica → 404, byte-identical to P1's steer-miss response.
func TestHandleCancelTurn_SingleProcessExistingRunNotLive_404(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "agent-x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-x", UserID: "alice", Interactive: true})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// No steer registration, no cluster canceller → fallback finds nothing → 404.
	if rec := doJSON(t, srv, "POST", "/v1/runs/"+run.ID+"/cancel", `{}`); rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (no in-flight run); body=%s", rec.Code, rec.Body.String())
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Code
}
