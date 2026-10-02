package steer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// A verdict-only run (a sub-agent its parent drives) takes a review verdict
// and nothing else: a steer or a compaction is refused as if the run were not
// live, on the local push and on a cluster delivery alike.
func TestRegistry_AVerdictOnlyRunTakesOnlyVerdicts(t *testing.T) {
	r := NewRegistry(4)
	q, dereg := r.Register(Entry{RunID: "child", VerdictsOnly: true})
	defer dereg()
	for _, m := range []Message{{Text: "steer"}, {Kind: KindCompact}} {
		if _, err := r.Push(context.Background(), "child", m); !errors.Is(err, ErrRunNotFound) {
			t.Errorf("push %+v: err %v, want ErrRunNotFound", m, err)
		}
		if _, found, _ := r.PushLocal("child", m); found {
			t.Errorf("PushLocal %+v was accepted", m)
		}
	}
	for _, kind := range []string{KindApprove, KindReject} {
		if ok, err := r.Push(context.Background(), "child", Message{Kind: kind}); !ok || err != nil {
			t.Errorf("verdict %s: %v %v", kind, ok, err)
		}
	}
	if len(q) != 2 {
		t.Fatalf("queue holds %d, want the 2 verdicts", len(q))
	}
}

// A run that has read its queue for the last time closes it, and from then on
// a push is refused exactly as for a run that has ended — locally, and on a
// cluster delivery to the owner — rather than accepted into a queue nobody
// reads.
func TestRegistry_APushAfterTheQueueClosesIsRefusedLikeAnEndedRun(t *testing.T) {
	r := NewRegistry(4)
	_, dereg := r.Register(Entry{RunID: "run1"})
	defer dereg()
	if !r.CloseIfEmpty("run1") {
		t.Fatal("CloseIfEmpty on an empty queue = false, want true")
	}
	if delivered, err := r.Push(context.Background(), "run1", Message{Text: "late"}); delivered || !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Push after close = (%v, %v), want (false, ErrRunNotFound)", delivered, err)
	}
	if delivered, found, closed := r.PushLocal("run1", Message{Text: "late"}); delivered || !found || !closed {
		t.Errorf("PushLocal after close = (delivered=%v, found=%v, closed=%v), want (false, true, true)", delivered, found, closed)
	}
	if e, ok := r.Get("run1"); !ok || !e.Closed() {
		t.Errorf("Get after close = (closed=%v, %v), want a closed entry still registered", e.Closed(), ok)
	}
}

// The queue is never closed on an unread message: CloseIfEmpty reports one is
// waiting, and the queue keeps taking messages until it is drained and closed.
func TestRegistry_CloseIfEmptyLeavesAQueueWithAWaitingMessageOpen(t *testing.T) {
	r := NewRegistry(4)
	q, dereg := r.Register(Entry{RunID: "run1"})
	defer dereg()
	if _, err := r.Push(context.Background(), "run1", Message{Text: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if r.CloseIfEmpty("run1") {
		t.Fatal("CloseIfEmpty with a message waiting = true, want false")
	}
	if delivered, err := r.Push(context.Background(), "run1", Message{Text: "second"}); !delivered || err != nil {
		t.Errorf("Push to an unclosed queue = (%v, %v), want delivered", delivered, err)
	}
	<-q
	<-q
	if !r.CloseIfEmpty("run1") {
		t.Error("CloseIfEmpty after the drain = false, want true")
	}
}

// Every push either lands before the close, where the draining run reads it,
// or after, where it is refused: none is reported delivered and then lost.
func TestRegistry_EveryAcceptedPushIsReadBeforeTheQueueCloses(t *testing.T) {
	for round := 0; round < 50; round++ {
		r := NewRegistry(64)
		q, dereg := r.Register(Entry{RunID: "run1"})
		var accepted atomic.Int64
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					if ok, _ := r.Push(context.Background(), "run1", Message{Text: "m"}); ok {
						accepted.Add(1)
					}
				}
			}()
		}
		read := int64(0)
		for {
			for drained := false; !drained; {
				select {
				case <-q:
					read++
				default:
					drained = true
				}
			}
			if r.CloseIfEmpty("run1") {
				break
			}
		}
		wg.Wait()
		if got := accepted.Load(); got != read {
			t.Fatalf("round %d: %d pushes were accepted but the run read %d before closing", round, got, read)
		}
		dereg()
	}
}
