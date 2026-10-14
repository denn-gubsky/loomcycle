package concurrency

import (
	"context"
	"testing"
	"time"
)

// semOrder records which waiter got each slot, in grant order.
type semOrder struct {
	got chan string
}

// queueAcquire starts an Acquire for (tenant, user) labelled name and waits
// until the semaphore counts it as queued, so waiters enqueue in a known order.
// A waiter that is granted a slot reports its name and holds the slot until
// the test releases it through the returned channel.
func (o *semOrder) queueAcquire(t *testing.T, s *Semaphore, tenant, user, name string) chan struct{} {
	t.Helper()
	before := s.Stats().Queued
	done := make(chan struct{})
	go func() {
		rel, err := s.AcquireForUser(context.Background(), tenant, user)
		if err != nil {
			o.got <- name + ":" + err.Error()
			return
		}
		o.got <- name
		<-done
		rel()
	}()
	waitQueued(t, s, before+1)
	return done
}

func (o *semOrder) queueWait(t *testing.T, s *Semaphore, name string) chan struct{} {
	t.Helper()
	before := s.Stats().Queued
	done := make(chan struct{})
	go func() {
		rel, err := s.Wait(context.Background())
		if err != nil {
			o.got <- name + ":" + err.Error()
			return
		}
		o.got <- name
		<-done
		rel()
	}()
	waitQueued(t, s, before+1)
	return done
}

func (o *semOrder) next(t *testing.T) string {
	t.Helper()
	select {
	case n := <-o.got:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("no waiter was granted a slot")
		return ""
	}
}

// TestSemaphore_ServesWaitingUsersRoundRobin — when a slot frees, the next
// waiting (tenant, user) in turn gets it, not the oldest request overall: a
// user who queued a burst first cannot hold back a user who queued after it.
func TestSemaphore_ServesWaitingUsersRoundRobin(t *testing.T) {
	s := New(1, 16, 0)
	holder, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	o := &semOrder{got: make(chan string, 8)}
	a1 := o.queueAcquire(t, s, "t1", "alice", "a1")
	a2 := o.queueAcquire(t, s, "t1", "alice", "a2")
	a3 := o.queueAcquire(t, s, "t1", "alice", "a3")
	b1 := o.queueAcquire(t, s, "t2", "bob", "b1")

	holder()
	want := []struct {
		name string
		done chan struct{}
	}{{"a1", a1}, {"b1", b1}, {"a2", a2}, {"a3", a3}}
	for _, w := range want {
		if got := o.next(t); got != w.name {
			t.Fatalf("granted %q, want %q", got, w.name)
		}
		close(w.done)
	}
}

// TestSemaphore_SameUserDifferentTenantsAreSeparateTurns — the turn is per
// (tenant, user), so one subject name in two tenants takes two turns.
func TestSemaphore_SameUserDifferentTenantsAreSeparateTurns(t *testing.T) {
	s := New(1, 16, 0)
	holder, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	o := &semOrder{got: make(chan string, 8)}
	x1 := o.queueAcquire(t, s, "t1", "ops", "x1")
	x2 := o.queueAcquire(t, s, "t1", "ops", "x2")
	y1 := o.queueAcquire(t, s, "t2", "ops", "y1")

	holder()
	for _, w := range []struct {
		name string
		done chan struct{}
	}{{"x1", x1}, {"y1", y1}, {"x2", x2}} {
		if got := o.next(t); got != w.name {
			t.Fatalf("granted %q, want %q", got, w.name)
		}
		close(w.done)
	}
}

// TestSemaphore_ServesAdmittedWorkFirst — a Wait-er (work that already started
// once, such as a resumed run) gets the next slot ahead of new requests that
// queued before it.
func TestSemaphore_ServesAdmittedWorkFirst(t *testing.T) {
	s := New(1, 16, 0)
	holder, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	o := &semOrder{got: make(chan string, 8)}
	n1 := o.queueAcquire(t, s, "t1", "alice", "new")
	r1 := o.queueWait(t, s, "resumed")

	holder()
	for _, w := range []struct {
		name string
		done chan struct{}
	}{{"resumed", r1}, {"new", n1}} {
		if got := o.next(t); got != w.name {
			t.Fatalf("granted %q, want %q", got, w.name)
		}
		close(w.done)
	}
}

// TestSemaphore_CancelKeepsTheBucketsTurn — cancelling one waiter of a user
// leaves that user's other waiters in their place in the turn order.
func TestSemaphore_CancelKeepsTheBucketsTurn(t *testing.T) {
	s := New(1, 16, 0)
	holder, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	o := &semOrder{got: make(chan string, 8)}
	a1 := o.queueAcquire(t, s, "t1", "alice", "a1")

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	before := s.Stats().Queued
	go func() {
		_, err := s.AcquireForUser(ctx, "t1", "bob")
		cancelled <- err
	}()
	waitQueued(t, s, before+1)
	b2 := o.queueAcquire(t, s, "t1", "bob", "b2")
	c1 := o.queueAcquire(t, s, "t1", "carol", "c1")

	cancel()
	if err := <-cancelled; err == nil {
		t.Fatal("cancelled Acquire returned a slot")
	}

	holder()
	for _, w := range []struct {
		name string
		done chan struct{}
	}{{"a1", a1}, {"b2", b2}, {"c1", c1}} {
		if got := o.next(t); got != w.name {
			t.Fatalf("granted %q, want %q", got, w.name)
		}
		close(w.done)
	}
	// The last holder releases on its own goroutine after done closes.
	deadline := time.Now().Add(2 * time.Second)
	for st := s.Stats(); st.Active != 0 || st.Queued != 0; st = s.Stats() {
		if time.Now().After(deadline) {
			t.Fatalf("after all releases: active %d queued %d, want 0 0", st.Active, st.Queued)
		}
		time.Sleep(time.Millisecond)
	}
}
