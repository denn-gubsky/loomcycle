package steer

import (
	"context"
	"errors"
	"testing"
)

func TestRegistry_PushDeliverDeregister(t *testing.T) {
	r := NewRegistry(2) // per-run buffer depth 2
	q, dereg := r.Register(Entry{RunID: "run1", SessionID: "s1", UserID: "u1"})

	// Deliver to a live run.
	delivered, err := r.Push(context.Background(), "run1", Message{Text: "hi"})
	if err != nil || !delivered {
		t.Fatalf("Push = (%v, %v), want (true, nil)", delivered, err)
	}
	if m := <-q; m.Text != "hi" {
		t.Errorf("received %q, want hi", m.Text)
	}

	// Fill the buffer (cap 2), then a third push → ErrQueueFull.
	if _, err := r.Push(context.Background(), "run1", Message{Text: "1"}); err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if _, err := r.Push(context.Background(), "run1", Message{Text: "2"}); err != nil {
		t.Fatalf("push 2: %v", err)
	}
	if _, err := r.Push(context.Background(), "run1", Message{Text: "3"}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("third push err = %v, want ErrQueueFull", err)
	}

	// A push to an unknown run (single-replica, no cluster) → ErrRunNotFound.
	if _, err := r.Push(context.Background(), "nope", Message{Text: "x"}); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("unknown-run push err = %v, want ErrRunNotFound", err)
	}

	// After deregister, the run is no longer found.
	dereg()
	if _, err := r.Push(context.Background(), "run1", Message{Text: "x"}); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("post-deregister push err = %v, want ErrRunNotFound", err)
	}
	if r.Count() != 0 {
		t.Errorf("Count = %d, want 0 after deregister", r.Count())
	}
}

func TestRegistry_Get(t *testing.T) {
	r := NewRegistry(0) // default cap
	_, dereg := r.Register(Entry{RunID: "run1", SessionID: "sess-9", UserID: "u"})
	defer dereg()
	if e, ok := r.Get("run1"); !ok || e.SessionID != "sess-9" {
		t.Errorf("Get(run1) = (%+v, %v), want SessionID=sess-9, true", e, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) = true, want false")
	}
}
