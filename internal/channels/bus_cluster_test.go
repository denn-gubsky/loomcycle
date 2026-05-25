package channels

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
	feedCh       chan coord.Event
}

func newStubBackplane() *stubBackplane {
	return &stubBackplane{feedCh: make(chan coord.Event, 16)}
}

func (s *stubBackplane) Publish(_ context.Context, _ string, _ []byte) error {
	s.publishCount.Add(1)
	return nil
}

func (s *stubBackplane) Subscribe(_ context.Context, _ string) (<-chan coord.Event, error) {
	return s.feedCh, nil
}

func (s *stubBackplane) Close() error { return nil }

func TestBus_Notify_PublishesOnBackplane_WhenBPSet(t *testing.T) {
	b := NewBus()
	bp := newStubBackplane()
	b.SetBackplane(bp)
	b.Notify("my-channel")
	if bp.publishCount.Load() != 1 {
		t.Errorf("backplane publishes = %d, want 1", bp.publishCount.Load())
	}
}

func TestBus_Notify_NoBackplane_NoPublish(t *testing.T) {
	// Single-replica path — no backplane interaction.
	b := NewBus()
	b.Notify("my-channel") // no panic on nil backplane
}

func TestBus_SubscribeBackplane_WakesLocalWaiters(t *testing.T) {
	b := NewBus()
	bp := newStubBackplane()
	b.SetBackplane(bp)
	if err := b.SubscribeBackplane(context.Background(), bp); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Start a Wait in a goroutine.
	waited := make(chan bool, 1)
	go func() {
		waited <- b.Wait(context.Background(), "my-channel", time.Second)
	}()
	// Give Wait time to register.
	time.Sleep(50 * time.Millisecond)

	// Inject a backplane event.
	payload, _ := json.Marshal(channelBackplaneEvent{Channel: "my-channel"})
	bp.feedCh <- coord.Event{Topic: "loomcycle.channel", Payload: payload}

	select {
	case got := <-waited:
		if !got {
			t.Error("Wait returned false; expected true (woken by backplane event)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return — backplane event did not wake local waiter")
	}
}

func TestBus_SubscribeBackplane_DoesNotRePublish(t *testing.T) {
	// Same invariant as runstate.Bus: backplane-received events MUST
	// NOT trigger another backplane publish (would loop).
	b := NewBus()
	bp := newStubBackplane()
	b.SetBackplane(bp)
	if err := b.SubscribeBackplane(context.Background(), bp); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	payload, _ := json.Marshal(channelBackplaneEvent{Channel: "x"})
	bp.feedCh <- coord.Event{Topic: "loomcycle.channel", Payload: payload}
	time.Sleep(150 * time.Millisecond)

	if bp.publishCount.Load() != 0 {
		t.Errorf("backplane re-publish detected (count=%d) — notifyLocal contract broken", bp.publishCount.Load())
	}
}
