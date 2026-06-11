package pause

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestManager_PauseWaitsForRunToPark asserts Pause()'s Stage-2 barrier blocks
// until a registered in-flight run reaches a boundary and parks — so
// paused_runs_count is accurate on return (the RFC X fix). A simulated
// PauseGate parks the run (store 'paused' → MarkParked) only after pause is
// declared, exactly as the real loop gate does.
func TestManager_PauseWaitsForRunToPark(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r1"})

	m.RegisterRun(run.ID)
	defer m.DeregisterRun(run.ID)

	// Simulated PauseGate: once pause is declared, persist 'paused' then mark
	// parked (the BeginPark→store→MarkParked ordering the real gate uses), and
	// stay parked until resume.
	parked := make(chan struct{})
	go func() {
		for m.State() == StateRunning {
			time.Sleep(2 * time.Millisecond)
		}
		resume, should := m.BeginPark(run.ID)
		if !should {
			return
		}
		_ = s.SetRunPauseState(ctx, run.ID, store.PauseStatePaused)
		m.MarkParked(run.ID)
		close(parked)
		<-resume
		_ = s.SetRunPauseState(ctx, run.ID, store.PauseStateRunning)
		m.EndPark(run.ID)
	}()

	res, err := m.Pause(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(time.Second):
		t.Fatal("gate never parked the run")
	}
	if res.PausedRunsCount != 1 {
		t.Errorf("PausedRunsCount = %d, want 1 (Pause must wait for the run to park)", res.PausedRunsCount)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}

	// Resume wakes the parked gate goroutine + flips the row back.
	rr, err := m.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if rr.ResumedRunsCount != 1 {
		t.Errorf("ResumedRunsCount = %d, want 1", rr.ResumedRunsCount)
	}
}

// TestManager_PauseTimesOutOnUnparkedRun asserts a registered run that never
// reaches a boundary (e.g. blocked in a long tool / provider turn) does not
// hang Pause forever: Pause returns at the timeout with a warning and
// paused_runs_count=0 (the run is still executing).
func TestManager_PauseTimesOutOnUnparkedRun(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r1"})

	m.RegisterRun(run.ID) // registered but never parks
	defer m.DeregisterRun(run.ID)

	start := time.Now()
	res, err := m.Pause(ctx, 150*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("Pause returned in %v, want ≥ timeout (should wait the full window for the run to park)", elapsed)
	}
	if res.PausedRunsCount != 0 {
		t.Errorf("PausedRunsCount = %d, want 0 (the run never parked)", res.PausedRunsCount)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("want a warning naming the run(s) that did not reach a boundary in time")
	}
	// The warning must NAME the unparked run (review finding #2 — actionable so
	// an operator knows a fan-out parent / long turn held back the quiesce).
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, run.ID) {
		t.Errorf("warning %q does not name the unparked run %q", joined, run.ID)
	}
	if got := m.State(); got != StatePaused {
		t.Errorf("State after timeout = %s, want paused (state still transitions)", got)
	}
}

// TestManager_PauseQuiescesImmediatelyWhenNoRuns pins the idle case: no
// registered runs → the barrier is satisfied at once.
func TestManager_PauseQuiescesImmediatelyWhenNoRuns(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	start := time.Now()
	res, err := m.Pause(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("Pause with no in-flight runs took %v, want near-immediate", time.Since(start))
	}
	if res.PausedRunsCount != 0 {
		t.Errorf("PausedRunsCount = %d, want 0", res.PausedRunsCount)
	}
}
