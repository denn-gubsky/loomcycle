package pause

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
)

// stubBackplane is the in-process backplane fake for Phase 4 unit
// tests. Records every Publish + lets tests inject events via a
// per-topic Subscribe channel.
type stubBackplane struct {
	published atomic.Int32
	mu        chan coord.Event
}

func newStubBackplane() *stubBackplane {
	return &stubBackplane{mu: make(chan coord.Event, 16)}
}

func (s *stubBackplane) Publish(_ context.Context, _ string, payload []byte) error {
	s.published.Add(1)
	return nil
}

func (s *stubBackplane) Subscribe(ctx context.Context, _ string) (<-chan coord.Event, error) {
	// Return the stub channel; tests inject events by writing to s.mu.
	return s.mu, nil
}

func (s *stubBackplane) Close() error { return nil }

func TestManager_ApplyRemotePause_TransitionsLocalState(t *testing.T) {
	m := NewManager(nil, 0)
	if m.State() != StateRunning {
		t.Fatalf("initial state = %s, want running", m.State())
	}

	m.applyRemotePause()

	// applyRemotePause skips StatePausing and goes straight to
	// StatePaused (the originating replica already drained tools).
	if m.State() != StatePaused {
		t.Errorf("after applyRemotePause: state = %s, want paused", m.State())
	}
	// pauseCh should be closed so a select observer fires immediately.
	select {
	case <-m.PauseCh():
	case <-time.After(100 * time.Millisecond):
		t.Error("pauseCh not closed by applyRemotePause")
	}
}

func TestManager_ApplyRemotePause_IdempotentWhenAlreadyPaused(t *testing.T) {
	m := NewManager(nil, 0)
	m.applyRemotePause()
	// Second call — should not panic (close-of-closed-chan would).
	m.applyRemotePause()
	if m.State() != StatePaused {
		t.Errorf("state after double pause = %s, want paused", m.State())
	}
}

// TestManager_ApplyRemotePause_NoOpDuringLocalPausing pins review-1
// finding #3: applyRemotePause's guard is `!= StateRunning`, so it
// also bails when this replica's own Pause() is mid-flight in
// StatePausing. Without the guard, the second close(pauseCh) inside
// applyRemotePause would panic.
func TestManager_ApplyRemotePause_NoOpDuringLocalPausing(t *testing.T) {
	m := NewManager(nil, 0)
	// Simulate "local Pause() is in flight" by setting state to
	// StatePausing + closing pauseCh, as Pause() does under m.mu.
	m.state.Store(int32(StatePausing))
	close(m.pauseCh)
	// Now applyRemotePause must NOT close pauseCh again (panic) AND
	// must NOT change state away from StatePausing.
	m.applyRemotePause()
	if m.State() != StatePausing {
		t.Errorf("state was clobbered: %s, want still pausing", m.State())
	}
}

func TestManager_ApplyRemoteResume_ReopensPauseCh(t *testing.T) {
	m := NewManager(nil, 0)
	m.applyRemotePause()
	if m.State() != StatePaused {
		t.Fatal("setup: should be paused")
	}

	prevCh := m.PauseCh()
	m.applyRemoteResume()
	if m.State() != StateRunning {
		t.Errorf("after applyRemoteResume: state = %s, want running", m.State())
	}
	newCh := m.PauseCh()
	if prevCh == newCh {
		t.Error("applyRemoteResume should allocate a fresh pauseCh; same channel returned")
	}
}

func TestManager_SubscribeBackplane_DispatchesPauseEvent(t *testing.T) {
	m := NewManager(nil, 0)
	bp := newStubBackplane()
	if err := m.SubscribeBackplane(context.Background(), bp); err != nil {
		t.Fatalf("SubscribeBackplane: %v", err)
	}

	// Inject a pause event.
	payload, _ := json.Marshal(pauseBackplaneEvent{Op: "pause"})
	bp.mu <- coord.Event{Topic: "loomcycle.pause", Payload: payload}

	// Goroutine processes asynchronously; poll.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.State() == StatePaused {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.State() != StatePaused {
		t.Errorf("backplane pause event not dispatched; state = %s", m.State())
	}
}

func TestManager_SubscribeBackplane_MalformedEventIgnored(t *testing.T) {
	m := NewManager(nil, 0)
	bp := newStubBackplane()
	if err := m.SubscribeBackplane(context.Background(), bp); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Malformed JSON — logged + skipped.
	bp.mu <- coord.Event{Topic: "loomcycle.pause", Payload: []byte("not json")}
	// Unknown op — logged + skipped.
	payload, _ := json.Marshal(pauseBackplaneEvent{Op: "neither"})
	bp.mu <- coord.Event{Topic: "loomcycle.pause", Payload: payload}
	time.Sleep(100 * time.Millisecond)
	if m.State() != StateRunning {
		t.Errorf("malformed events should not transition state; got %s", m.State())
	}
}

func TestManager_State_SingleReplicaPathUnchanged(t *testing.T) {
	// With no SetRuntimeStateStore call, the Manager must hit the
	// in-process atomic path — no DB, no cache. This pins the
	// back-compat invariant for v0.11.x deployments.
	m := NewManager(nil, 0)
	// State() should return atomically without touching m.rss (nil).
	got := m.State()
	if got != StateRunning {
		t.Errorf("single-replica State() = %s, want running", got)
	}
	// Force into pausing via atomic store directly and verify State()
	// reads from atomic.
	m.state.Store(int32(StatePausing))
	if got := m.State(); got != StatePausing {
		t.Errorf("single-replica State() = %s, want pausing", got)
	}
}

func TestParseRuntimeState(t *testing.T) {
	cases := map[string]RuntimeState{
		"running": StateRunning,
		"pausing": StatePausing,
		"paused":  StatePaused,
		"":        StateRunning, // defensive
		"garbage": StateRunning, // defensive
	}
	for input, want := range cases {
		if got := parseRuntimeState(input); got != want {
			t.Errorf("parseRuntimeState(%q) = %s, want %s", input, got, want)
		}
	}
}

// Sentinel: applyRemoteResume from StateRunning is a no-op.
func TestManager_ApplyRemoteResume_NoOpWhenAlreadyRunning(t *testing.T) {
	m := NewManager(nil, 0)
	prevCh := m.PauseCh()
	m.applyRemoteResume() // already running
	if m.PauseCh() != prevCh {
		t.Error("applyRemoteResume on already-running should NOT replace pauseCh")
	}
	if m.State() != StateRunning {
		t.Errorf("state = %s, want running", m.State())
	}
}

// Sentinel: SubscribeBackplane returns the subscribe error.
type errBackplane struct{ err error }

func (e *errBackplane) Publish(context.Context, string, []byte) error { return nil }
func (e *errBackplane) Subscribe(context.Context, string) (<-chan coord.Event, error) {
	return nil, e.err
}
func (e *errBackplane) Close() error { return nil }

func TestManager_SubscribeBackplane_PropagatesSubscribeError(t *testing.T) {
	m := NewManager(nil, 0)
	want := errors.New("simulated")
	if err := m.SubscribeBackplane(context.Background(), &errBackplane{err: want}); err == nil {
		t.Fatal("expected subscribe error to propagate")
	}
}
