package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newRunForFinishTest seeds a running run row and returns the server + run id.
func newRunForFinishTest(t *testing.T) (*Server, string) {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := &Server{store: st}
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "agent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a1", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, run.ID
}

// TestFinishRunWithCancel_ClientDisconnectRecordsCancelled is the regression for
// the JobEmber symptom: a non-interactive run whose CLIENT disconnected (runCtx
// cancelled, NO ErrCancelledByAPI cause, loop returned context.Canceled) must be
// recorded as a clean `cancelled`, NOT `failed: "context canceled"`. The
// terminal row must persist even though runCtx is already cancelled
// (finishRunCancelled detaches to a background ctx). Fail-before: the old code
// fell through to finishRun → status=failed.
func TestFinishRunWithCancel_ClientDisconnectRecordsCancelled(t *testing.T) {
	srv, runID := newRunForFinishTest(t)

	// A client disconnect: runCtx cancelled with the default cause (plain
	// context.Canceled), NOT cancel.ErrCancelledByAPI. The loop returns
	// context.Canceled.
	runCtx, cancelFn := context.WithCancelCause(context.Background())
	cancelFn(nil) // nil cause → context.Canceled (mirrors request-ctx propagation)

	srv.finishRunWithCancel(context.Background(), runCtx, runID, loop.RunResult{}, context.Canceled, runStateMeta{RunID: runID, IsTopLevel: true})

	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunCancelled {
		t.Errorf("status = %q, want %q (a client disconnect must record a clean cancel, not a failure)", run.Status, store.RunCancelled)
	}
	if run.ErrorMsg != "" {
		t.Errorf("a cancelled run must carry no error text, got %q", run.ErrorMsg)
	}
	if run.StopReason != "client disconnected" {
		t.Errorf("stop_reason = %q, want %q", run.StopReason, "client disconnected")
	}
}

// TestFinishRunWithCancel_RealErrorStillFails locks the inverse: a GENUINE run
// error (runCtx NOT cancelled) is still recorded as failed — the disconnect
// reclassification must not swallow real failures.
func TestFinishRunWithCancel_RealErrorStillFails(t *testing.T) {
	srv, runID := newRunForFinishTest(t)

	// runCtx alive (never cancelled); the loop returned a real provider error.
	runCtx, cancelFn := context.WithCancelCause(context.Background())
	defer cancelFn(nil)

	srv.finishRunWithCancel(context.Background(), runCtx, runID, loop.RunResult{}, errors.New("provider 500: boom"), runStateMeta{RunID: runID, IsTopLevel: true})

	run, _ := srv.store.GetRun(context.Background(), runID)
	if run.Status != store.RunFailed {
		t.Errorf("status = %q, want %q (a real error must stay failed)", run.Status, store.RunFailed)
	}
	if run.ErrorMsg == "" {
		t.Errorf("a failed run must carry its error text")
	}
}

// TestFinishRunWithCancel_APICancelUnchanged confirms the pre-existing API-cancel
// path still wins (cancelled with the API reason), unaffected by the new branch.
func TestFinishRunWithCancel_APICancelUnchanged(t *testing.T) {
	srv, runID := newRunForFinishTest(t)

	runCtx, cancelFn := context.WithCancelCause(context.Background())
	cancelFn(cancel.ErrCancelledByAPI)

	srv.finishRunWithCancel(context.Background(), runCtx, runID, loop.RunResult{}, context.Canceled, runStateMeta{RunID: runID, IsTopLevel: true})

	run, _ := srv.store.GetRun(context.Background(), runID)
	if run.Status != store.RunCancelled {
		t.Errorf("status = %q, want %q (API cancel)", run.Status, store.RunCancelled)
	}
}
