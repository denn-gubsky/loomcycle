package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// returnsWithin runs fn on its own goroutine and reports whether it returned
// within d. A call that never returns is left behind: the test has failed by
// then, and the goroutine ends with the test binary.
func returnsWithin(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// A run the boot restart queues again while the runtime is paused already
// holds its place, so the restart returns instead of waiting for the resume —
// boot runs it synchronously — and the run is cancellable while it waits out
// the pause: the cancel ends it cancelled.
func TestRestartQueuedRuns_ReturnsWhileAPauseLastsAndTheRunIsCancellable(t *testing.T) {
	s, _ := newGatedBatchServer(t, 1)
	pm := pause.NewManager(s.store, 50*time.Millisecond)
	s.SetPauseManager(pm)
	if _, err := pm.Pause(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pm.Resume(context.Background()) })
	run := queuedFixture(t, s, "a_qpausedsync", "", runner.RunInput{Agent: "r", Segments: oneUserSeg("x")}, "")

	if !returnsWithin(3*time.Second, func() { _, _, _ = s.RestartQueuedRuns(context.Background(), time.Now().Add(time.Second)) }) {
		t.Fatal("RestartQueuedRuns did not return while the pause lasts: it waits on a run that is only waiting out the pause")
	}
	if got, _ := s.store.GetRun(context.Background(), run.ID); got.Status != store.RunQueued {
		t.Fatalf("restarted run during the pause = %q, want still queued", got.Status)
	}
	if _, ok := s.cancelReg.Cancel("a_qpausedsync", "cancelled during the pause"); !ok {
		t.Fatal("a restarted run waiting out the pause is in no cancel registry")
	}
	waitRunStatus(t, s.store, run.ID, store.RunCancelled)
}

// Restoring a snapshot into a paused runtime returns: the restored queued
// runs wait out the pause on their own, and run once the runtime resumes.
func TestSnapshot_RestoreIntoAPausedRuntimeReturnsAndRunsOnResume(t *testing.T) {
	src, _ := newGatedBatchServer(t, 1)
	queued := queuedFixture(t, src, "a_snappaused", "", runner.RunInput{Agent: "r", Segments: oneUserSeg("restored while paused")}, "")
	_, raw, err := snapshot.Capture(context.Background(), src.store, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dst, gate := newGatedBatchServer(t, 1)
	pm := pause.NewManager(dst.store, 50*time.Millisecond)
	dst.SetPauseManager(pm)
	if _, err := pm.Pause(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pm.Resume(context.Background()) })
	result, err := snapshot.Restore(context.Background(), dst.store, raw, dst.snapshotRestoreOptions(context.Background(), false))
	if err != nil {
		t.Fatal(err)
	}
	if !returnsWithin(3*time.Second, func() { dst.finishRestore(context.Background(), &result) }) {
		t.Fatal("restoring into a paused runtime did not return until the runtime resumed")
	}
	if result.QueuedRunsRequeued != 1 {
		t.Fatalf("requeued %d, want 1 (warnings %v)", result.QueuedRunsRequeued, result.Warnings)
	}
	if got, _ := dst.store.GetRun(context.Background(), queued.ID); got.Status != store.RunQueued {
		t.Fatalf("restored run during the pause = %q, want still queued", got.Status)
	}
	if _, err := pm.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, dst.store, queued.ID, store.RunRunning)
	gate <- struct{}{}
	waitRunStatus(t, dst.store, queued.ID, store.RunCompleted)
}

// A queued run that ends without starting — here its caller went away while
// it waited — gives its idempotency key back, as a run a restart cannot start
// does: the caller's retry with the same key runs the work instead of being
// answered with the run that never ran.
func TestRunOnce_QueuedRunEndedBeforeStartReleasesItsIdempotencyKey(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	callerCtx, callerGone := context.WithCancel(context.Background())
	first := startRunOnce(s, callerCtx, runner.RunInput{Agent: "r", ClientIdempotencyKey: "retry-me", Segments: oneUserSeg("the work")})
	_, firstRun := first.ids(t)
	callerGone()
	if err := first.wait(t); err == nil {
		t.Fatal("a queued run whose caller went away returned nil")
	}
	waitRunStatus(t, s.store, firstRun, store.RunCancelled)

	retry := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", ClientIdempotencyKey: "retry-me", Segments: oneUserSeg("the work")})
	var retryRun string
	select {
	case ids := <-retry.registered:
		retryRun = ids[1]
	case err := <-retry.done:
		var dup *runner.DuplicateRunError
		if errors.As(err, &dup) {
			t.Fatalf("the retry was answered with run %s, which ended cancelled before it started (first run %s)", dup.RunID, firstRun)
		}
		t.Fatalf("the retry ended before it was registered: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("the retry was never registered")
	}
	if retryRun == firstRun {
		t.Fatalf("the retry reused run %s", firstRun)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	_ = holder.wait(t)
	if err := retry.wait(t); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitRunStatus(t, s.store, retryRun, store.RunCompleted)
}

// nextRunState returns the next run-state event for runID, skipping other
// runs' events, or fails the test when none arrives.
func nextRunState(t *testing.T, sub *runstate.Subscription, runID string) runstate.RunStateEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-sub.C:
			if ev.RunID == runID {
				return ev
			}
		case <-deadline:
			t.Fatalf("no run-state event for run %s", runID)
			return runstate.RunStateEvent{}
		}
	}
}

// A run cancelled while it is queued reports its end on the run-state stream,
// with its identity: a consumer holding the run's id from its queued answer
// waits for a terminal frame.
func TestRunOnce_CancelledWhileQueuedPublishesItsEnd(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	bus := runstate.NewBus()
	s.SetRunStateBus(bus)
	sub := bus.Subscribe("qa")
	defer sub.Close()
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	queued := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", UserID: "qa", Segments: oneUserSeg("waits")})
	agentID, runID := queued.ids(t)
	if _, ok := s.cancelReg.Cancel(agentID, "test"); !ok {
		t.Fatal("cancel registry has no entry for the queued run")
	}
	_ = queued.wait(t)
	waitRunStatus(t, s.store, runID, store.RunCancelled)
	ev := nextRunState(t, sub, runID)
	if ev.Status != string(store.RunCancelled) || ev.AgentID != agentID || ev.Agent != "r" || ev.UserID != "qa" {
		t.Errorf("run-state event = %+v, want the run's cancelled end with its identity", ev)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
}

// A queued run a restart cannot start again is failed, and that end is
// reported on the run-state stream too.
func TestRestartQueuedRuns_PublishesTheEndOfARunItCannotStart(t *testing.T) {
	s, _ := newGatedBatchServer(t, 1)
	bus := runstate.NewBus()
	s.SetRunStateBus(bus)
	sub := bus.Subscribe("alice")
	defer sub.Close()
	noTrace := queuedFixture(t, s, "a_qlostpub", "", runner.RunInput{Agent: "r", Segments: oneUserSeg("x")}, retentionmode.None)

	if _, failed, err := s.RestartQueuedRuns(context.Background(), time.Now().Add(time.Second)); err != nil || failed != 1 {
		t.Fatalf("RestartQueuedRuns failed %d (%v), want 1", failed, err)
	}
	ev := nextRunState(t, sub, noTrace.ID)
	if ev.Status != string(store.RunFailed) || ev.AgentID != "a_qlostpub" || ev.TenantID != "acme" {
		t.Errorf("run-state event = %+v, want the run's failed end in acme", ev)
	}
}
