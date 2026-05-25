package runstate

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
)

type stubBackplane struct {
	publishCount atomic.Int32
	lastTopic    atomic.Value // string
	feedCh       chan coord.Event
}

func newStubBackplane() *stubBackplane {
	return &stubBackplane{feedCh: make(chan coord.Event, 16)}
}

func (s *stubBackplane) Publish(_ context.Context, topic string, _ []byte) error {
	s.publishCount.Add(1)
	s.lastTopic.Store(topic)
	return nil
}

func (s *stubBackplane) Subscribe(_ context.Context, _ string) (<-chan coord.Event, error) {
	return s.feedCh, nil
}

func (s *stubBackplane) Close() error { return nil }

func TestBus_Publish_FanoutsToBackplane_WhenBPSet(t *testing.T) {
	b := NewBus()
	bp := newStubBackplane()
	b.SetBackplane(bp)
	sub := b.Subscribe("alice")
	defer sub.Close()

	b.Publish(RunStateEvent{RunID: "r1", UserID: "alice", Status: "completed"})
	// Local delivery
	select {
	case evt := <-sub.C:
		if evt.RunID != "r1" {
			t.Errorf("local delivery: RunID = %q, want r1", evt.RunID)
		}
	case <-time.After(time.Second):
		t.Fatal("no local delivery")
	}
	// Backplane publish (async w.r.t the local delivery but happens
	// before Publish returns).
	if bp.publishCount.Load() != 1 {
		t.Errorf("backplane publishes = %d, want 1", bp.publishCount.Load())
	}
	if got := bp.lastTopic.Load(); got != "loomcycle.runstate" {
		t.Errorf("topic = %v, want loomcycle.runstate", got)
	}
}

func TestBus_Publish_NoBackplane_LocalOnly(t *testing.T) {
	// Single-replica: bp nil → no backplane Publish. v0.11.x invariant.
	b := NewBus()
	sub := b.Subscribe("alice")
	defer sub.Close()

	b.Publish(RunStateEvent{RunID: "r2", UserID: "alice", Status: "completed"})
	select {
	case <-sub.C:
	case <-time.After(time.Second):
		t.Fatal("no local delivery")
	}
	// No backplane => no publish to count, and no panic from a nil
	// dereference.
}

func TestBus_SubscribeBackplane_DeliversRemoteEvents(t *testing.T) {
	b := NewBus()
	bp := newStubBackplane()
	b.SetBackplane(bp)
	if err := b.SubscribeBackplane(context.Background(), bp); err != nil {
		t.Fatalf("SubscribeBackplane: %v", err)
	}
	sub := b.Subscribe("alice")
	defer sub.Close()

	// Inject a backplane event (simulating a remote replica publish).
	remoteEvt := RunStateEvent{RunID: "r3", UserID: "alice", Status: "completed"}
	payload, _ := json.Marshal(remoteEvt)
	bp.feedCh <- coord.Event{Topic: "loomcycle.runstate", Payload: payload}

	select {
	case evt := <-sub.C:
		if evt.RunID != "r3" {
			t.Errorf("got RunID %q, want r3", evt.RunID)
		}
	case <-time.After(time.Second):
		t.Fatal("backplane event did not wake local subscriber")
	}
}

func TestBus_SubscribeBackplane_DoesNotRePublish(t *testing.T) {
	// Critical invariant: events received on backplane MUST NOT be
	// re-published on backplane (would loop). publishLocal is the
	// in-package no-republish path.
	b := NewBus()
	bp := newStubBackplane()
	b.SetBackplane(bp)
	if err := b.SubscribeBackplane(context.Background(), bp); err != nil {
		t.Fatalf("SubscribeBackplane: %v", err)
	}

	remoteEvt := RunStateEvent{RunID: "r4", UserID: "alice", Status: "completed"}
	payload, _ := json.Marshal(remoteEvt)
	bp.feedCh <- coord.Event{Topic: "loomcycle.runstate", Payload: payload}
	time.Sleep(200 * time.Millisecond)

	// publishCount must stay 0 — the backplane subscriber calls
	// publishLocal, not Publish.
	if bp.publishCount.Load() != 0 {
		t.Errorf("backplane re-publish detected (count=%d) — publishLocal contract broken", bp.publishCount.Load())
	}
}
