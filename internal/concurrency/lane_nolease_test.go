package concurrency

import (
	"context"
	"testing"
	"time"
)

// acquireOn takes one call on l for c within 3s and returns its slot and
// release.
func acquireOn(t *testing.T, l *Lane, c CallInfo) (int, func()) {
	t.Helper()
	slot := -1
	c.OnSlot = func(n int) { slot = n }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rel, err := l.Acquire(ctx, c)
	if err != nil {
		t.Fatalf("Acquire(%+v): %v", c, err)
	}
	return slot, rel
}

// A NoLease call of an owner holding a reservation takes another free slot,
// leaves the reservation where it is, and starts none of its own; the owner's
// next ordinary call is back on its reserved slot.
func TestLane_NoLeaseCallTakesAFreeSlotBesideTheReservation(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 2, AffinityHold: time.Hour, QuantumCalls: 1})
	turn := CallInfo{Owner: "run-a", UserID: "u"}
	side := CallInfo{Owner: "run-a", UserID: "u", NoLease: true}

	s0, rel := acquireOn(t, l, turn)
	rel()
	s1, rel := acquireOn(t, l, side)
	rel()
	if st := l.Stats(); st.Reserved != 1 || st.Active != 0 {
		t.Fatalf("after the side call: active=%d reserved=%d, want 0/1", st.Active, st.Reserved)
	}
	// QuantumCalls 1 with nobody waiting: the reservation is honoured, and the
	// side call spent none of the run's quantum.
	s2, rel := acquireOn(t, l, turn)
	rel()
	if s0 != 0 || s1 != 1 || s2 != 0 {
		t.Fatalf("slots = %d, %d, %d, want 0, 1, 0", s0, s1, s2)
	}
	if st := l.Stats(); st.Preemptions != 0 {
		t.Fatalf("preemptions = %d, want 0", st.Preemptions)
	}
}

// With no slot free, a foreground NoLease call gives up its owner's idle
// reservation rather than wait for it to lapse; a background one queues and
// leaves it.
func TestLane_NoLeaseCallGivesUpItsOwnersReservationRatherThanWait(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Hour})
	_, rel := acquireOn(t, l, CallInfo{Owner: "run-a"})
	rel()

	bg, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(bg, CallInfo{Owner: "run-a", NoLease: true, Background: true}); err == nil {
		t.Fatal("a background NoLease call was granted the owner's reserved slot")
	}
	if st := l.Stats(); st.Reserved != 1 {
		t.Fatalf("reserved = %d after the background call gave up, want 1", st.Reserved)
	}

	slot, rel := acquireOn(t, l, CallInfo{Owner: "run-a", NoLease: true})
	rel()
	if st := l.Stats(); slot != 0 || st.Reserved != 0 || st.Active != 0 {
		t.Fatalf("foreground side call: slot=%d active=%d reserved=%d, want 0/0/0", slot, st.Active, st.Reserved)
	}
}

// A NoLease call still counts as its owner's queued call: the owner's call in
// flight, ending while it waits, does not keep its slot reserved against it.
func TestLane_QueuedNoLeaseCallStillCountsAsItsOwners(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Hour})
	_, relTurn := acquireOn(t, l, CallInfo{Owner: "run-a"})
	got := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		slot := -1
		rel, err := l.Acquire(ctx, CallInfo{Owner: "run-a", NoLease: true, OnSlot: func(n int) { slot = n }})
		if err == nil {
			rel()
		}
		got <- slot
	}()
	end := time.Now().Add(3 * time.Second)
	for l.Stats().Queued != 1 {
		if time.Now().After(end) {
			t.Fatal("the side call never queued")
		}
		time.Sleep(time.Millisecond)
	}
	relTurn()
	select {
	case slot := <-got:
		if slot != 0 {
			t.Fatalf("side call slot = %d, want 0 (-1: it waited behind its own run's reservation)", slot)
		}
		if st := l.Stats(); st.Reserved != 0 {
			t.Fatalf("reserved = %d after the side call, want 0", st.Reserved)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the side call never returned")
	}
}
