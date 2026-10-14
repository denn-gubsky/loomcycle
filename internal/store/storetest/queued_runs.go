package storetest

import (
	"context"
	"encoding/json"
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
// alone. FailQueuedRun ends it with the reason given and releases its
// idempotency keys, so a retry with the same key runs the work; it refuses a
// run that is no longer queued.
func testQueuedRunFailedOnRestartNotSwept(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "tq", "default", "dee")
	if err != nil {
		t.Fatal(err)
	}
	keyed := store.RunIdentity{AgentID: "a_qfail", UserID: "dee", TenantID: "tq",
		IdempotencyKey: "qkey-fail", DeliveryAltKey: "qalt-fail"}
	queued, err := s.CreateQueuedRun(ctx, sess.ID, keyed)
	if err != nil {
		t.Fatal(err)
	}
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
	// Started after the sweep, which would fail a just-started run as stale.
	running = newQueuedRun(t, s, "tq", "dee", "a_qrun2")
	if _, err := s.StartQueuedRun(ctx, running.ID, store.RunIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailQueuedRun(ctx, queued.ID, "queued when loomcycle restarted"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, queued.ID)
	if got.Status != store.RunFailed || got.ErrorMsg != "queued when loomcycle restarted" || got.StopReason != "restarted" {
		t.Errorf("queued run after FailQueuedRun = %q %q %q, want failed, the reason, restarted", got.Status, got.ErrorMsg, got.StopReason)
	}
	if err := s.FailQueuedRun(ctx, running.ID, "x"); !errors.Is(err, store.ErrRunNotQueued) {
		t.Errorf("FailQueuedRun of a running run = %v, want ErrRunNotQueued", err)
	}
	if got, _ := s.GetRun(ctx, running.ID); got.Status != store.RunRunning {
		t.Errorf("FailQueuedRun changed a running run to %q", got.Status)
	}
	// Both keys are free again: a new run may claim them.
	if _, err := s.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_qretry", UserID: "dee", TenantID: "tq",
		IdempotencyKey: "qkey-fail", DeliveryAltKey: "qalt-fail"}); err != nil {
		t.Errorf("a retry with the failed run's keys = %v, want accepted", err)
	}
}

// The request a queued run will start from is recorded while it is queued,
// listed for a restart oldest first with it, and cleared when the run starts
// or ends. A run queued after the restart's boot is not listed.
func testQueuedRunsListedWithTheirInput(t *testing.T, s store.Store) {
	ctx := context.Background()
	first := newQueuedRun(t, s, "tql", "eve", "a_ql1")
	time.Sleep(2 * time.Millisecond)
	second := newQueuedRun(t, s, "tql", "eve", "a_ql2")
	bare := newQueuedRun(t, s, "tql", "eve", "a_ql3")
	if err := s.SetQueuedInput(ctx, first.ID, []byte(`{"agent":"one"}`)); err != nil {
		t.Fatalf("SetQueuedInput: %v", err)
	}
	if err := s.SetQueuedInput(ctx, second.ID, []byte(`{"agent":"two"}`)); err != nil {
		t.Fatal(err)
	}
	boot := time.Now()
	time.Sleep(2 * time.Millisecond)
	late := newQueuedRun(t, s, "tql", "eve", "a_ql4")

	list, err := s.ListQueuedRuns(ctx, "", boot)
	if err != nil {
		t.Fatalf("ListQueuedRuns: %v", err)
	}
	pos := map[string]int{}
	inputs := map[string]string{}
	for i, q := range list {
		pos[q.Run.ID] = i
		inputs[q.Run.ID] = string(q.Input)
	}
	if _, ok := pos[late.ID]; ok {
		t.Error("a run queued after boot was listed")
	}
	if _, ok := pos[bare.ID]; !ok {
		t.Fatal("a queued run with no input was not listed")
	}
	if inputs[bare.ID] != "" {
		t.Errorf("input of a run that recorded none = %q, want empty", inputs[bare.ID])
	}
	if pos[first.ID] > pos[second.ID] {
		t.Error("queued runs were not listed oldest first")
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(inputs[first.ID]), &v); err != nil || v["agent"] != "one" {
		t.Errorf("listed input = %q (%v), want the recorded request", inputs[first.ID], err)
	}

	if _, err := s.StartQueuedRun(ctx, first.ID, store.RunIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, second.ID, store.RunCancelled, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQueuedInput(ctx, first.ID, []byte(`{}`)); !errors.Is(err, store.ErrRunNotQueued) {
		t.Errorf("SetQueuedInput on a started run = %v, want ErrRunNotQueued", err)
	}
	list, err = s.ListQueuedRuns(ctx, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range list {
		if q.Run.ID == first.ID || q.Run.ID == second.ID {
			t.Errorf("run %s was listed after it started or ended", q.Run.ID)
		}
	}
	// That the column itself is cleared at start and end is checked per
	// backend (TestQueuedInput_ClearedWhenTheRunStartsOrEnds): no store call
	// reads it off a row that is not queued.
}
