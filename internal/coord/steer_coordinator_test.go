package coord

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memBackplane is an in-process Backplane for unit-testing the coordinator
// round-trip without a Postgres fixture. Publish fans a copy to every current
// subscriber of the topic (non-blocking; the 64-deep buffers never fill in a
// test).
type memBackplane struct {
	mu   sync.Mutex
	subs map[string][]chan Event
}

func newMemBackplane() *memBackplane { return &memBackplane{subs: map[string][]chan Event{}} }

func (b *memBackplane) Publish(_ context.Context, topic string, payload []byte) error {
	b.mu.Lock()
	chans := append([]chan Event(nil), b.subs[topic]...)
	b.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- Event{Topic: topic, Payload: payload}:
		default:
		}
	}
	return nil
}

func (b *memBackplane) Subscribe(_ context.Context, topic string) (<-chan Event, error) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch, nil
}

func (b *memBackplane) Close() error { return nil }

type stubSteerRunStore struct{ runs map[string]store.Run } // keyed by run_id

func (s *stubSteerRunStore) GetRun(_ context.Context, runID string) (store.Run, error) {
	r, ok := s.runs[runID]
	if !ok {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	return r, nil
}

type stubLiveness struct {
	alive bool
	err   error
}

func (s stubLiveness) IsReplicaAlive(context.Context, string, time.Duration) (bool, error) {
	return s.alive, s.err
}

func newSteerCoord(t *testing.T, bp Backplane, replicaID string, runs map[string]store.Run, alive bool) *SteerCoordinator {
	t.Helper()
	c, err := NewSteerCoordinator(SteerCoordinatorConfig{
		Backplane:    bp,
		ReplicaID:    replicaID,
		Store:        &stubSteerRunStore{runs: runs},
		ReplicaStore: stubLiveness{alive: alive},
		AckTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSteerCoordinator: %v", err)
	}
	return c
}

func TestSteerCoordinator_NotFound_ReturnsMiss(t *testing.T) {
	c := newSteerCoord(t, newMemBackplane(), "replica-A", map[string]store.Run{}, true)
	delivered, found, err := c.PushRemote(context.Background(), "ghost", steer.Message{Text: "x"})
	if err != nil || delivered || found {
		t.Errorf("PushRemote(unknown) = (%v,%v,%v), want (false,false,nil)", delivered, found, err)
	}
}

func TestSteerCoordinator_SelfOwned_ReturnsMiss(t *testing.T) {
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: "replica-A"}}
	c := newSteerCoord(t, newMemBackplane(), "replica-A", runs, true)
	delivered, found, _ := c.PushRemote(context.Background(), "run-1", steer.Message{Text: "x"})
	if delivered || found {
		t.Errorf("self-owned run = (%v,%v), want (false,false) — local registry missed ⇒ run ended", delivered, found)
	}
}

func TestSteerCoordinator_Terminal_ReturnsMiss(t *testing.T) {
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunCompleted, ReplicaID: "replica-B"}}
	c := newSteerCoord(t, newMemBackplane(), "replica-A", runs, true)
	delivered, found, _ := c.PushRemote(context.Background(), "run-1", steer.Message{Text: "x"})
	if delivered || found {
		t.Errorf("terminal run = (%v,%v), want (false,false)", delivered, found)
	}
}

func TestSteerCoordinator_DeadOwner_ReturnsMiss(t *testing.T) {
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: "replica-B"}}
	c := newSteerCoord(t, newMemBackplane(), "replica-A", runs, false /* dead */)
	delivered, found, _ := c.PushRemote(context.Background(), "run-1", steer.Message{Text: "x"})
	if delivered || found {
		t.Errorf("dead-owner run = (%v,%v), want (false,false)", delivered, found)
	}
}

// The full cross-replica round-trip: originator (A) PushRemote → owner (B)
// RunSteerSubscriber delivers to B's local registry + acks → A's ack subscriber
// routes the ack back → PushRemote returns delivered.
func TestSteerCoordinator_DeliversToRemoteOwner(t *testing.T) {
	bp := newMemBackplane()
	const owner = "replica-B"
	runs := map[string]store.Run{"run-1": {ID: "run-1", Status: store.RunRunning, ReplicaID: owner}}

	a := newSteerCoord(t, bp, "replica-A", runs, true) // originator
	b := newSteerCoord(t, bp, owner, runs, true)       // owner

	ownerReg := steer.NewRegistry(4)
	q, dereg := ownerReg.Register(steer.Entry{RunID: "run-1"})
	defer dereg()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunSteerSubscriber(ctx, ownerReg) // owner side
	go a.RunSteerAckSubscriber(ctx)        // originator side
	time.Sleep(30 * time.Millisecond)      // let both Subscribe calls register

	delivered, found, err := a.PushRemote(context.Background(), "run-1", steer.Message{Text: "focus"})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	if !found || !delivered {
		t.Fatalf("PushRemote = (delivered=%v, found=%v), want (true,true)", delivered, found)
	}
	select {
	case m := <-q:
		if m.Text != "focus" {
			t.Errorf("owner queue got %q, want focus", m.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("owner registry never received the steered message")
	}
}

func TestSteerCoordinator_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  SteerCoordinatorConfig
		want string
	}{
		{"nil backplane", SteerCoordinatorConfig{}, "backplane"},
		{"nil store", SteerCoordinatorConfig{Backplane: newMemBackplane()}, "store"},
		{"nil replica store", SteerCoordinatorConfig{Backplane: newMemBackplane(), Store: &stubSteerRunStore{}}, "replica store"},
	}
	for _, tc := range cases {
		if _, err := NewSteerCoordinator(tc.cfg); err == nil {
			t.Errorf("%s: want error mentioning %q, got nil", tc.name, tc.want)
		}
	}
}
