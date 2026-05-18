package pause

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newTestManager builds a Manager backed by an in-memory SQLite store.
// Returns a cleanup func the test caller must defer.
func newTestManager(t *testing.T) (*Manager, store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	m := NewManager(s, 100*time.Millisecond)
	return m, s, func() { _ = s.Close() }
}

// TestManager_InitialStateIsRunning pins the default + the nil
// receiver behaviour the loop relies on.
func TestManager_InitialStateIsRunning(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	if got := m.State(); got != StateRunning {
		t.Errorf("State() = %s, want running", got)
	}
	var nilM *Manager
	if got := nilM.State(); got != StateRunning {
		t.Errorf("nil Manager.State() = %s, want running (nil-safe)", got)
	}
	if ch := nilM.PauseCh(); ch != nil {
		t.Errorf("nil Manager.PauseCh() = %v, want nil channel (blocks forever)", ch)
	}
}

// TestManager_PauseTransitionsRunningToPaused walks the happy path:
// StateRunning → Pause() → StatePaused. Closed channel observable
// pre-pause is also verified.
func TestManager_PauseTransitionsRunningToPaused(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()

	chBefore := m.PauseCh()
	select {
	case <-chBefore:
		t.Fatal("pauseCh closed before Pause was called")
	default:
		// expected
	}

	res, err := m.Pause(context.Background(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if res.State != "paused" {
		t.Errorf("result.State = %q, want paused", res.State)
	}
	if got := m.State(); got != StatePaused {
		t.Errorf("State() after Pause = %s, want paused", got)
	}
	// The channel from before Pause must now be closed.
	select {
	case <-chBefore:
		// expected — closed channel returns immediately
	default:
		t.Error("pauseCh not closed after Pause")
	}
}

// TestManager_PauseTwiceReturnsAlreadyPausing pins idempotency: the
// second Pause call returns ErrAlreadyPausing so the HTTP handler can
// surface 409 rather than 200 with a misleading result.
func TestManager_PauseTwiceReturnsAlreadyPausing(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	if _, err := m.Pause(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("first Pause: %v", err)
	}
	_, err := m.Pause(context.Background(), 50*time.Millisecond)
	if !errors.Is(err, ErrAlreadyPausing) {
		t.Errorf("second Pause err = %v, want ErrAlreadyPausing", err)
	}
}

// TestManager_ResumeRequiresPaused pins that Resume from StateRunning
// returns ErrNotPaused — the HTTP handler maps to 409.
func TestManager_ResumeRequiresPaused(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	_, err := m.Resume(context.Background())
	if !errors.Is(err, ErrNotPaused) {
		t.Errorf("Resume from running: err = %v, want ErrNotPaused", err)
	}
}

// TestManager_ResumeRestoresRunningAndFreshChannel walks Pause →
// Resume. The new PauseCh after Resume must be DIFFERENT from the
// old one (so future Pause calls start clean) and unclosed.
func TestManager_ResumeRestoresRunningAndFreshChannel(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	oldCh := m.PauseCh()
	if _, err := m.Pause(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	res, err := m.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.State != "running" {
		t.Errorf("result.State = %q, want running", res.State)
	}
	if got := m.State(); got != StateRunning {
		t.Errorf("State() after Resume = %s, want running", got)
	}
	newCh := m.PauseCh()
	if newCh == oldCh {
		t.Error("PauseCh after Resume is the same channel as before Pause; expected a fresh allocation")
	}
	select {
	case <-newCh:
		t.Error("fresh PauseCh is closed; expected open")
	default:
		// expected
	}
}

// TestManager_PauseResumeCycleSurvivesMultipleRounds — the pause
// channel must be reusable across rounds. A second Pause/Resume on
// the same manager must succeed and produce a fresh channel each
// time.
func TestManager_PauseResumeCycleSurvivesMultipleRounds(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	for round := 0; round < 3; round++ {
		if _, err := m.Pause(context.Background(), 50*time.Millisecond); err != nil {
			t.Fatalf("round %d Pause: %v", round, err)
		}
		if _, err := m.Resume(context.Background()); err != nil {
			t.Fatalf("round %d Resume: %v", round, err)
		}
		if got := m.State(); got != StateRunning {
			t.Errorf("round %d: State() = %s, want running", round, got)
		}
	}
}

// TestManager_ToolCtx_RunningStateIsPassthrough — under StateRunning,
// ToolCtx returns a ctx that's NOT cancelled and a cleanup that's
// idempotent. Tracking lands in activeTools and cleanup removes it.
func TestManager_ToolCtx_RunningStateIsPassthrough(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	ctx, cleanupTool := m.ToolCtx(context.Background(), "tool-id-1", "Read", []byte(`{}`))
	defer cleanupTool()

	select {
	case <-ctx.Done():
		t.Error("ctx done before any pause; expected open under StateRunning")
	default:
		// expected
	}

	// Confirm registry has the entry.
	count := 0
	m.activeTools.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 1 {
		t.Errorf("activeTools count = %d, want 1", count)
	}

	cleanupTool()
	count = 0
	m.activeTools.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("activeTools count after cleanup = %d, want 0", count)
	}
}

// TestManager_PauseCancelsIdempotentToolsImmediately walks the
// in-flight policy: an idempotent tool registered before Pause, then
// Pause arrives — the tool's ctx must be cancelled IMMEDIATELY
// (without waiting for the deadline). The non-idempotent peer
// registered alongside must NOT be cancelled.
func TestManager_PauseCancelsIdempotentToolsImmediately(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()

	idemCtx, idemCleanup := m.ToolCtx(context.Background(), "t-idem", "Read", []byte(`{}`))
	nonIdemCtx, nonIdemCleanup := m.ToolCtx(context.Background(), "t-nonidem", "Write", []byte(`{"path":"/tmp/x"}`))
	defer idemCleanup()
	defer nonIdemCleanup()

	// Pause in a background goroutine so we can observe the ctx
	// transitions on the running goroutine.
	pauseDone := make(chan struct{})
	go func() {
		_, _ = m.Pause(context.Background(), 200*time.Millisecond)
		close(pauseDone)
	}()

	// Idempotent ctx should be cancelled within a short wait.
	select {
	case <-idemCtx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Error("idempotent tool's ctx not cancelled within 100ms of pause")
	}

	// Non-idempotent ctx should NOT be cancelled (waiting for it
	// to finish naturally OR hit the per-pause timeout).
	select {
	case <-nonIdemCtx.Done():
		t.Error("non-idempotent ctx cancelled immediately; expected wait")
	case <-time.After(50 * time.Millisecond):
		// expected — still running
	}

	// Let the pause finish naturally (the non-idem tool's entry
	// is still in activeTools; Pause's deadline will sweep it).
	nonIdemCleanup()
	<-pauseDone
}

// TestManager_PauseCountsForceCancelled — when Pause's deadline
// fires with non-idempotent tools still active, those get force-
// cancelled and counted in the returned ForceCancelledCount.
func TestManager_PauseCountsForceCancelled(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()

	// Register two non-idempotent tools and DON'T clean them up.
	_, _ = m.ToolCtx(context.Background(), "t-1", "Write", []byte(`{"path":"/tmp/a"}`))
	_, _ = m.ToolCtx(context.Background(), "t-2", "Bash", []byte(`{"command":"sleep 10"}`))

	res, err := m.Pause(context.Background(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if res.ForceCancelledCount != 2 {
		t.Errorf("ForceCancelledCount = %d, want 2", res.ForceCancelledCount)
	}
}

// TestManager_ConcurrentPauseCh — the PauseCh is observable from many
// goroutines concurrently, and they all wake when Pause is called.
// Race detector catches data races here.
func TestManager_ConcurrentPauseCh(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()

	var wg sync.WaitGroup
	woke := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-m.PauseCh()
			woke <- struct{}{}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let goroutines park
	if _, err := m.Pause(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	wg.Wait()
	if len(woke) != 10 {
		t.Errorf("woke count = %d, want 10 (all goroutines should see the close)", len(woke))
	}
}

// TestManager_SnapshotReflectsPausedCount — Snapshot includes the
// count of paused runs. We seed a few runs, transition them, and
// verify the snapshot.
func TestManager_SnapshotReflectsPausedCount(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	r1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r1"})
	r2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r2"})
	_ = s.SetRunPauseState(ctx, r1.ID, store.PauseStatePaused)
	_ = s.SetRunPauseState(ctx, r2.ID, store.PauseStatePaused)

	snap, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.PausedRunsCount != 2 {
		t.Errorf("PausedRunsCount = %d, want 2", snap.PausedRunsCount)
	}
}

// TestManager_ResumeFlipsPausedRunsToRunning — Resume calls
// SetRunPauseState(running) on every previously-paused run.
func TestManager_ResumeFlipsPausedRunsToRunning(t *testing.T) {
	m, s, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	r1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r1"})
	r2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r2"})
	_ = s.SetRunPauseState(ctx, r1.ID, store.PauseStatePaused)
	_ = s.SetRunPauseState(ctx, r2.ID, store.PauseStatePaused)
	// Manager doesn't know about these (they were paused outside
	// its lifecycle in this test). Force state to paused so Resume
	// proceeds.
	_, _ = m.Pause(ctx, 50*time.Millisecond)

	res, err := m.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.ResumedRunsCount != 2 {
		t.Errorf("ResumedRunsCount = %d, want 2", res.ResumedRunsCount)
	}

	// Confirm both rows back to running.
	for _, agentID := range []string{"r1", "r2"} {
		got, _ := s.GetRunByAgentID(ctx, agentID)
		if got.PauseState != store.PauseStateRunning {
			t.Errorf("%s.PauseState = %q, want running", agentID, got.PauseState)
		}
	}
}
