package pause

import (
	"context"
	"errors"
	"slices"
	"sort"
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

// PauseWatch reports the pause from the moment it is declared, and each
// channel it hands out closes at the next change — the pause, then the resume.
func TestManager_PauseWatchSignalsThePauseAndTheResume(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	closed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	paused, changed := m.PauseWatch()
	if paused || closed(changed) {
		t.Fatalf("running: paused=%v changed closed=%v, want neither", paused, closed(changed))
	}
	if _, err := m.Pause(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !closed(changed) {
		t.Fatal("the running watch was not woken by the pause")
	}
	paused, changed = m.PauseWatch()
	if !paused || closed(changed) {
		t.Fatalf("paused: paused=%v changed closed=%v, want paused and an open channel", paused, closed(changed))
	}
	if _, err := m.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !closed(changed) {
		t.Fatal("the paused watch was not woken by the resume")
	}
	if paused, _ = m.PauseWatch(); paused {
		t.Error("still paused after the resume")
	}

	var nilM *Manager
	if paused, changed := nilM.PauseWatch(); paused || changed != nil {
		t.Errorf("nil Manager.PauseWatch() = %v, %v; want never paused, never changing", paused, changed)
	}
}

// PausedSince is when the operator paused — before the barrier wait — and is
// cleared by the resume.
func TestManager_PausedSinceIsWhenThePauseBegan(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	if !m.PausedSince().IsZero() {
		t.Fatal("a running manager reports a pause start")
	}
	before := time.Now()
	if _, err := m.Pause(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	after := time.Now()
	if since := m.PausedSince(); since.Before(before) || since.After(after) {
		t.Errorf("PausedSince = %v, want within the Pause call [%v, %v]", since, before, after)
	}
	if _, err := m.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !m.PausedSince().IsZero() {
		t.Error("the pause start outlived the resume")
	}
	m.applyRemotePause()
	if m.PausedSince().IsZero() {
		t.Error("a cluster pause applied here records no start")
	}
	m.applyRemoteResume()
	if !m.PausedSince().IsZero() {
		t.Error("the cluster pause's start outlived its resume")
	}
}

// pauseWrites records a RunPauseRecorder's calls.
type pauseWrites struct {
	mu     sync.Mutex
	writes []string // "open <run> <since>" or "end <run> <since>"
	ends   map[string]time.Time
}

func (w *pauseWrites) record(_ context.Context, runID string, since, until time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if until.IsZero() {
		w.writes = append(w.writes, "open "+runID+" "+since.Format(time.RFC3339Nano))
		return nil
	}
	w.writes = append(w.writes, "end "+runID+" "+since.Format(time.RFC3339Nano))
	if w.ends == nil {
		w.ends = map[string]time.Time{}
	}
	w.ends[runID] = until
	return nil
}

func (w *pauseWrites) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.writes
	w.writes = nil
	sort.Strings(out)
	return out
}

// Every run live when the runtime pauses has the pause recorded — not only
// the runs that park for it: one inside a tool call for the whole pause never
// parks. Each is opened before Pause returns, so a snapshot taken then carries
// it, and ended, as the same pause, before Resume returns. A cluster pause
// applied here records it the same way.
func TestManager_APauseIsRecordedOnEveryLiveRun(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	w := &pauseWrites{}
	m.SetRunPauseRecorder(w.record)
	m.RegisterRun("r_busy")
	m.RegisterRun("r_other")

	if _, err := m.Pause(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	since := m.PausedSince().Format(time.RFC3339Nano)
	if got, want := w.take(), []string{"open r_busy " + since, "open r_other " + since}; !slices.Equal(got, want) {
		t.Fatalf("writes once Pause returned = %v, want %v", got, want)
	}
	before := time.Now()
	if _, err := m.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, want := w.take(), []string{"end r_busy " + since, "end r_other " + since}; !slices.Equal(got, want) {
		t.Fatalf("writes once Resume returned = %v, want %v", got, want)
	}
	if end := w.ends["r_busy"]; end.Before(before) || end.After(time.Now()) {
		t.Errorf("the pause ended at %v, want at the resume", end)
	}

	m.DeregisterRun("r_other")
	m.applyRemotePause()
	since = m.PausedSince().Format(time.RFC3339Nano)
	if got, want := w.take(), []string{"open r_busy " + since}; !slices.Equal(got, want) {
		t.Fatalf("writes once a cluster pause was applied = %v, want %v", got, want)
	}
	m.applyRemoteResume()
	if got, want := w.take(), []string{"end r_busy " + since}; !slices.Equal(got, want) {
		t.Fatalf("writes once a cluster resume was applied = %v, want %v", got, want)
	}
}
