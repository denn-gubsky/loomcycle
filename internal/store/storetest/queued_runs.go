package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EI P3 — queued runs: accepted, waiting for a run slot.

func newQueuedRun(t *testing.T, s store.Store, tenant, user, agentID string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, tenant, "default", user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: tenant})
	if err != nil {
		t.Fatalf("CreateQueuedRun: %v", err)
	}
	return run
}

// A queued run is created queued, starts as an ordinary running run with what
// start resolved, and a second start of it is refused.
func testQueuedRunLifecycle(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newQueuedRun(t, s, "tq", "ann", "a_queued")
	if run.Status != store.RunQueued {
		t.Fatalf("created status = %q, want queued", run.Status)
	}
	started, err := s.StartQueuedRun(ctx, run.ID, store.RunIdentity{Model: "m1", UserTier: "pro"})
	if err != nil {
		t.Fatalf("StartQueuedRun: %v", err)
	}
	if started.Status != store.RunRunning || started.Model != "m1" || started.UserTier != "pro" {
		t.Errorf("started = status %q model %q tier %q, want running m1 pro", started.Status, started.Model, started.UserTier)
	}
	if _, err := s.StartQueuedRun(ctx, run.ID, store.RunIdentity{}); !errors.Is(err, store.ErrRunNotQueued) {
		t.Errorf("second start = %v, want ErrRunNotQueued", err)
	}
	if _, err := s.StartQueuedRun(ctx, "no-such-run", store.RunIdentity{}); err == nil {
		t.Error("start of a missing run succeeded")
	}
}

// A queued run that is cancelled while it waits ends through FinishRun, and
// its start is then refused: the cancel wins.
func testQueuedRunCancelledBeforeStart(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newQueuedRun(t, s, "tq", "bea", "a_qcancel")
	if err := s.FinishRun(ctx, run.ID, store.RunCancelled, "", store.Usage{}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if got, _ := s.GetRun(ctx, run.ID); got.Status != store.RunCancelled {
		t.Fatalf("status after cancel = %q, want cancelled", got.Status)
	}
	if _, err := s.StartQueuedRun(ctx, run.ID, store.RunIdentity{}); !errors.Is(err, store.ErrRunNotQueued) {
		t.Errorf("start after cancel = %v, want ErrRunNotQueued", err)
	}
}

// A draft whose start has to wait becomes queued — guarded on its draft —
// and can no longer be edited, then starts through StartQueuedRun.
func testConfiguredRunQueuesThenStarts(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newDraft(t, s, "tq", "cal", "a_dq")
	draft, err := s.GetRunDraft(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueConfiguredRun(ctx, run.ID, []byte(`{"segments":[]}`)); !errors.Is(err, store.ErrDraftChanged) {
		t.Fatalf("queue with a stale draft = %v, want ErrDraftChanged", err)
	}
	if err := s.QueueConfiguredRun(ctx, run.ID, draft); err != nil {
		t.Fatalf("QueueConfiguredRun: %v", err)
	}
	if got, _ := s.GetRun(ctx, run.ID); got.Status != store.RunQueued {
		t.Fatalf("status = %q, want queued", got.Status)
	}
	if err := s.UpdateRunDraft(ctx, run.ID, []byte(`{"segments":[]}`)); !errors.Is(err, store.ErrRunNotConfigured) {
		t.Errorf("edit of a queued draft = %v, want ErrRunNotConfigured", err)
	}
	if err := s.DeleteConfiguredRun(ctx, run.ID); !errors.Is(err, store.ErrRunNotConfigured) {
		t.Errorf("delete of a queued draft = %v, want ErrRunNotConfigured", err)
	}
	if started, err := s.StartQueuedRun(ctx, run.ID, store.RunIdentity{}); err != nil || started.Status != store.RunRunning {
		t.Fatalf("start = %+v (%v), want running", started.Status, err)
	}
	if d, err := s.GetRunDraft(ctx, run.ID); err == nil || d != nil {
		t.Errorf("draft after start = %s (%v), want cleared", d, err)
	}
}

// A queued run is not a crashed running run: the stale sweeper leaves it
// alone. FailQueuedRuns ends it with the reason given, and leaves running
// rows alone.
func testQueuedRunFailedOnRestartNotSwept(t *testing.T, s store.Store) {
	ctx := context.Background()
	queued := newQueuedRun(t, s, "tq", "dee", "a_qfail")
	running := newQueuedRun(t, s, "tq", "dee", "a_qrun")
	if _, err := s.StartQueuedRun(ctx, running.ID, store.RunIdentity{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := s.SweepStaleRuns(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, queued.ID); got.Status != store.RunQueued {
		t.Fatalf("stale sweep changed a queued run to %q", got.Status)
	}
	other := newQueuedRun(t, s, "tq", "dee", "a_qrun2")
	if _, err := s.StartQueuedRun(ctx, other.ID, store.RunIdentity{}); err != nil {
		t.Fatal(err)
	}
	n, err := s.FailQueuedRuns(ctx, "", time.Now(), "queued when loomcycle restarted")
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("FailQueuedRuns = %d, want at least the one queued run", n)
	}
	got, _ := s.GetRun(ctx, queued.ID)
	if got.Status != store.RunFailed || got.ErrorMsg != "queued when loomcycle restarted" {
		t.Errorf("queued run after FailQueuedRuns = %q %q, want failed with the reason", got.Status, got.ErrorMsg)
	}
	if got, _ := s.GetRun(ctx, other.ID); got.Status != store.RunRunning {
		t.Errorf("FailQueuedRuns changed a running run to %q", got.Status)
	}
}
