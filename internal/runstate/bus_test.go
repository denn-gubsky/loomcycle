package runstate

import (
	"sync"
	"testing"
	"time"
)

func TestBus_PublishDeliversToMatchingSubscriber(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	sub := bus.Subscribe("user-a")
	defer sub.Close()

	bus.Publish(RunStateEvent{
		RunID: "r1", UserID: "user-a", Status: "running",
	})

	select {
	case evt := <-sub.C:
		if evt.RunID != "r1" || evt.Status != "running" {
			t.Fatalf("unexpected event: %+v", evt)
		}
		if evt.TS.IsZero() {
			t.Errorf("Publish should stamp TS when zero")
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered within 1s")
	}
}

func TestBus_PublishSkipsNonMatchingUser(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	a := bus.Subscribe("user-a")
	defer a.Close()
	b := bus.Subscribe("user-b")
	defer b.Close()

	bus.Publish(RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})

	select {
	case evt := <-a.C:
		if evt.RunID != "r1" {
			t.Fatalf("user-a got wrong event: %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("user-a never received its event")
	}

	select {
	case evt := <-b.C:
		t.Fatalf("user-b should not have received event for user-a: %+v", evt)
	case <-time.After(50 * time.Millisecond):
		// expected — no event for user-b
	}
}

func TestBus_EmptyUserIDSubscribesToAll(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	all := bus.Subscribe("")
	defer all.Close()

	bus.Publish(RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})
	bus.Publish(RunStateEvent{RunID: "r2", UserID: "user-b", Status: "completed"})

	var seen []string
	timeout := time.After(time.Second)
	for len(seen) < 2 {
		select {
		case evt := <-all.C:
			seen = append(seen, evt.RunID)
		case <-timeout:
			t.Fatalf("only saw %v before timeout", seen)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(seen), seen)
	}
}

func TestBus_FanOutToMultipleSubscribersSameUser(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	a := bus.Subscribe("user-a")
	defer a.Close()
	a2 := bus.Subscribe("user-a")
	defer a2.Close()

	bus.Publish(RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})

	for i, sub := range []*Subscription{a, a2} {
		select {
		case evt := <-sub.C:
			if evt.RunID != "r1" {
				t.Errorf("subscriber %d got wrong event: %+v", i, evt)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d never received event", i)
		}
	}
}

func TestBus_CloseUnregistersAndClosesChannel(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	sub := bus.Subscribe("user-a")
	if got := bus.ActiveSubscriberCount(); got != 1 {
		t.Fatalf("expected 1 active subscriber, got %d", got)
	}
	sub.Close()
	if got := bus.ActiveSubscriberCount(); got != 0 {
		t.Fatalf("expected 0 active subscribers after Close, got %d", got)
	}
	// Channel should be drained + closed.
	if _, ok := <-sub.C; ok {
		t.Errorf("channel should be closed after Close")
	}
}

func TestBus_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	sub := bus.Subscribe("user-a")
	sub.Close()
	sub.Close() // must not panic
}

func TestBus_DropOnFullBufferRatherThanBlock(t *testing.T) {
	t.Parallel()
	bus := NewBus()
	bus.bufferSize = 2 // tighten for the test
	sub := bus.Subscribe("user-a")
	defer sub.Close()

	// Publish 5; only first 2 fit; rest dropped.
	for i := 0; i < 5; i++ {
		bus.Publish(RunStateEvent{RunID: "r", UserID: "user-a", Status: "running"})
	}
	if got := sub.DroppedEvents(); got != 3 {
		t.Errorf("expected 3 dropped events, got %d", got)
	}
}

func TestBus_ConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()
	bus := NewBus()

	var wg sync.WaitGroup
	const subscribers = 8
	const eventsPer = 50

	// Start subscribers; each gets its own user_id.
	received := make([][]string, subscribers)
	for i := 0; i < subscribers; i++ {
		i := i
		sub := bus.Subscribe("user-" + string(rune('a'+i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sub.Close()
			deadline := time.After(2 * time.Second)
			for len(received[i]) < eventsPer {
				select {
				case evt := <-sub.C:
					received[i] = append(received[i], evt.RunID)
				case <-deadline:
					return
				}
			}
		}()
	}

	// Publish concurrently per user.
	for i := 0; i < subscribers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < eventsPer; j++ {
				bus.Publish(RunStateEvent{
					RunID:  string(rune('a' + i)),
					UserID: "user-" + string(rune('a'+i)),
					Status: "running",
				})
			}
		}()
	}
	wg.Wait()

	for i, got := range received {
		if len(got) != eventsPer {
			t.Errorf("subscriber %d received %d events, want %d", i, len(got), eventsPer)
		}
	}
}
