package pause

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// parkWhenPaused simulates a run's pause gate: once a pause is declared it
// persists 'paused' and takes barrier credit, as the real gate does, then stays
// parked until resume or stop is closed.
func parkWhenPaused(t *testing.T, m *Manager, s store.Store, runID string, stop <-chan struct{}) <-chan struct{} {
	t.Helper()
	parked := make(chan struct{})
	go func() {
		for m.State() == StateRunning {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		resume, should := m.BeginPark(runID)
		if !should {
			return
		}
		_ = s.SetRunPauseState(context.Background(), runID, store.PauseStatePaused)
		m.MarkParked(runID)
		close(parked)
		select {
		case <-resume:
		case <-stop:
		}
	}()
	return parked
}

func newShutdownRun(t *testing.T, s store.Store, agentID string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "a", "u")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A shutdown pause parks the runs that reach a boundary, and interrupts the
// tool calls of the runs that do not — only those: a parked run's tools are
// left alone.
func TestManager_ShutdownPauseInterruptsOnlyUnparkedRuns(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	stop := make(chan struct{})
	defer close(stop)

	parker := newShutdownRun(t, s, "parker")
	busy := newShutdownRun(t, s, "busy")
	m.RegisterRun(parker.ID)
	defer m.DeregisterRun(parker.ID)
	m.RegisterRun(busy.ID)
	defer m.DeregisterRun(busy.ID)
	parked := parkWhenPaused(t, m, s, parker.ID, stop)

	start := time.Now()
	res := m.PauseForShutdown(400 * time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("PauseForShutdown took %s with a 400ms bound", took)
	}
	select {
	case <-parked:
	default:
		t.Fatal("the parking run never parked")
	}
	if err := m.ToolInterrupt(parker.ID).Err(); err != nil {
		t.Errorf("the parked run's tools were interrupted: %v", err)
	}
	if err := m.ToolInterrupt(busy.ID).Err(); err == nil {
		t.Error("the run still working at the interrupt point kept its tools running")
	}
	if res.ParkedRuns != 1 || !slices.Equal(res.InterruptedRuns, []string{busy.ID}) || !slices.Equal(res.UnparkedRuns, []string{busy.ID}) {
		t.Errorf("result = %+v, want 1 parked and %s interrupted and unparked", res, busy.ID)
	}
	if m.State() != StatePaused {
		t.Errorf("state after the shutdown pause = %s, want paused", m.State())
	}
}

// A run interrupted at the interrupt point that then parks — its interrupted
// tool returned and it reached its boundary — is counted parked: the shutdown
// waits for it within the bound.
func TestManager_ShutdownPauseWaitsForInterruptedRunToPark(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	stop := make(chan struct{})
	defer close(stop)

	run := newShutdownRun(t, s, "tool")
	m.RegisterRun(run.ID)
	defer m.DeregisterRun(run.ID)
	// The run is inside a tool call: it reaches its boundary only once the
	// call is interrupted.
	go func() {
		select {
		case <-m.ToolInterrupt(run.ID).Done():
		case <-stop:
			return
		}
		resume, should := m.BeginPark(run.ID)
		if !should {
			return
		}
		_ = s.SetRunPauseState(context.Background(), run.ID, store.PauseStatePaused)
		m.MarkParked(run.ID)
		select {
		case <-resume:
		case <-stop:
		}
	}()

	res := m.PauseForShutdown(800 * time.Millisecond)
	if res.ParkedRuns != 1 || len(res.UnparkedRuns) != 0 || !slices.Equal(res.InterruptedRuns, []string{run.ID}) {
		t.Fatalf("result = %+v, want the interrupted run parked", res)
	}
	got, err := s.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PauseState != store.PauseStatePaused {
		t.Errorf("pause_state = %q, want paused", got.PauseState)
	}
}

// A shutdown pause is this replica's alone: nothing is published to the
// others, a cluster resume arriving meanwhile does not wake its runs, and an
// operator's resume is refused.
func TestManager_ShutdownPauseIsLocal(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	bp := newStubBackplane()
	m.SetBackplane(bp)

	run := newShutdownRun(t, s, "r")
	m.RegisterRun(run.ID)
	defer m.DeregisterRun(run.ID)
	stop := make(chan struct{})
	defer close(stop)
	parkWhenPaused(t, m, s, run.ID, stop)

	_ = m.PauseForShutdown(300 * time.Millisecond)
	if n := bp.published.Load(); n != 0 {
		t.Errorf("a shutdown pause published %d backplane event(s); want none", n)
	}
	resume, should := m.BeginPark("late")
	if !should {
		t.Fatal("BeginPark after a shutdown pause says the runtime is running")
	}

	m.applyRemoteResume()
	if m.State() == StateRunning {
		t.Error("a cluster resume un-paused a replica that is shutting down")
	}
	select {
	case <-resume:
		t.Error("a cluster resume woke a run parked for the shutdown")
	default:
	}

	if _, err := m.Resume(context.Background()); !errors.Is(err, ErrNotPaused) {
		t.Errorf("Resume during shutdown = %v, want ErrNotPaused", err)
	}
	if m.State() == StateRunning {
		t.Error("an operator resume un-paused a replica that is shutting down")
	}
}

// A run registering after the interrupt point (none should start, but a
// sub-run of an interrupted parent can) has its tools interrupted from the
// start.
func TestManager_RunRegisteredAfterShutdownInterruptStartsInterrupted(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	run := newShutdownRun(t, s, "busy")
	m.RegisterRun(run.ID)
	defer m.DeregisterRun(run.ID)
	_ = m.PauseForShutdown(100 * time.Millisecond)

	m.RegisterRun("late")
	defer m.DeregisterRun("late")
	if m.ToolInterrupt("late").Err() == nil {
		t.Error("a run registered after the interrupt point can still run tools")
	}
}

func TestManager_ToolInterruptUnregisteredIsNil(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	if m.ToolInterrupt("nope") != nil {
		t.Error("ToolInterrupt of an unregistered run is not nil")
	}
	var nilM *Manager
	if nilM.ToolInterrupt("x") != nil {
		t.Error("ToolInterrupt on a nil manager is not nil")
	}
}
