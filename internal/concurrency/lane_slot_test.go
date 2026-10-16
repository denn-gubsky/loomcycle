package concurrency

import (
	"context"
	"testing"
	"time"
)

// slotOf acquires on l for owner and returns the slot it was granted and the
// release.
func slotOf(t *testing.T, l *Lane, owner string) (int, func()) {
	t.Helper()
	slot := -1
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rel, err := l.Acquire(ctx, CallInfo{Owner: owner, OnSlot: func(n int) { slot = n }})
	if err != nil {
		t.Fatalf("Acquire(%s): %v", owner, err)
	}
	if slot < 0 {
		t.Fatalf("Acquire(%s) reported no slot", owner)
	}
	return slot, rel
}

// A run's next call lands on the slot its reservation kept — the number a
// backend's own slot is pinned to — and a new call takes the lowest free
// number, which stays below the cap.
func TestLane_AReservationKeepsItsSlotNumber(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 3, AffinityHold: time.Minute})

	a, relA := slotOf(t, l, "run-a")
	b, relB := slotOf(t, l, "run-b")
	if a != 0 || b != 1 {
		t.Fatalf("first grants = %d, %d, want 0 and 1", a, b)
	}
	relA() // run-a's slot 0 is now reserved for it
	relB()

	c, relC := slotOf(t, l, "run-c")
	if c != 2 {
		t.Fatalf("a new run while 0 and 1 are reserved = slot %d, want 2", c)
	}
	again, relAgain := slotOf(t, l, "run-a")
	if again != 0 {
		t.Fatalf("run-a's next call = slot %d, want its reserved 0", again)
	}
	relAgain()
	relC()

	// Dropping run-b's reservation frees its number for the next new run.
	l.Done("run-b")
	d, relD := slotOf(t, l, "run-d")
	if d != 1 {
		t.Fatalf("after run-b ended, a new run = slot %d, want the freed 1", d)
	}
	relD()
}

// Without a reservation (no affinity hold, or no owner) a slot's number is
// freed when its call ends, and the lowest free one is handed out.
func TestLane_SlotNumbersAreReusedWithoutAHold(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 2})
	a, relA := slotOf(t, l, "run-a")
	b, relB := slotOf(t, l, "")
	if a != 0 || b != 1 {
		t.Fatalf("grants = %d, %d, want 0 and 1", a, b)
	}
	relA()
	c, relC := slotOf(t, l, "run-c")
	if c != 0 {
		t.Fatalf("after slot 0's call ended, a new call = slot %d, want 0", c)
	}
	relB()
	relC()
	if l.inUse[0] || l.inUse[1] {
		t.Fatalf("slots still marked in use after every call ended: %v", l.inUse)
	}
}

// A queued call is told the slot it is handed when one frees.
func TestLane_AQueuedCallIsToldItsSlot(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	_, rel := slotOf(t, l, "run-a")
	got := make(chan int, 1)
	done := make(chan func(), 1)
	go func() {
		r, err := l.Acquire(context.Background(), CallInfo{Owner: "run-b", OnSlot: func(n int) { got <- n }})
		if err == nil {
			done <- r
		}
	}()
	eventually(t, "run-b queued", func() bool { return l.Stats().Queued == 1 })
	rel()
	select {
	case n := <-got:
		if n != 0 {
			t.Fatalf("queued call was given slot %d, want 0", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the queued call was never told its slot")
	}
	(<-done)()
}

// A lane shrunk by a reload never hands out a slot number past its new cap: a
// reservation on such a slot is dropped, whether it was idle at the reload or
// its call was still in flight, and the owner's next call takes a slot inside
// the cap. A backend pinned to the slot (llama.cpp's id_slot) would refuse it.
func TestLane_AShrunkLaneKeepsNoReservationPastItsCap(t *testing.T) {
	for name, inFlight := range map[string]bool{"idle at the reload": false, "in flight at the reload": true} {
		t.Run(name, func(t *testing.T) {
			cfg := LaneConfig{MaxConcurrent: 4, AffinityHold: time.Minute}
			l := NewLane(cfg)
			var rels []func()
			for _, owner := range []string{"a", "b", "c", "d"} {
				_, rel := slotOf(t, l, owner)
				rels = append(rels, rel)
			}
			// a, b and c end their runs: only d holds a slot, number 3.
			for i, owner := range []string{"a", "b", "c"} {
				rels[i]()
				l.Done(owner)
			}
			if !inFlight {
				rels[3]() // d's slot 3 is now reserved for it
			}
			cfg.MaxConcurrent = 2
			l.Reconfigure(cfg)
			if inFlight {
				rels[3]()
			}
			slot, rel := slotOf(t, l, "d")
			defer rel()
			if slot >= 2 {
				t.Fatalf("d's next call = slot %d, want one below the new cap of 2", slot)
			}
		})
	}
}
