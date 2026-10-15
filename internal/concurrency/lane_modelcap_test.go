package concurrency

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capLane is a lane of n slots whose model "A" runs one call at a time.
func capLane(n int, extra LaneConfig) *Lane {
	extra.MaxConcurrent = n
	if extra.ModelCaps == nil {
		extra.ModelCaps = map[string]int{"A": 1}
	}
	return NewLane(extra)
}

// A second call to a model at its cap waits while calls to other models take
// the free slots; when the model's call ends, the waiting call is granted.
func TestLane_ModelCapHoldsTheModelWhileOtherModelsTakeFreeSlots(t *testing.T) {
	l := capLane(4, LaneConfig{})
	a1 := laneNow(t, l, CallInfo{Owner: "a1", Model: "A"})
	a2 := laneAsync(t, l, CallInfo{Owner: "a2", Model: "A"})
	mustStillWait(t, a2, "a second call to a model capped at one, with three slots free")
	laneNow(t, l, CallInfo{Owner: "b1", Model: "B"})
	laneNow(t, l, CallInfo{Owner: "b2", Model: "B"})
	if st := l.Stats(); st.Active != 3 || st.Queued != 1 || st.BlockedOnModel != 1 {
		t.Fatalf("stats = %+v, want Active 3 Queued 1 BlockedOnModel 1", st)
	}
	a1()
	mustGrant(t, a2, "the waiting call once its model's call ended")
	if st := l.Stats(); st.Active != 3 || st.Queued != 0 || st.BlockedOnModel != 0 {
		t.Fatalf("stats = %+v, want Active 3 Queued 0 BlockedOnModel 0", st)
	}
}

// A waiter held back by its model does not hold back a call to another model
// queued behind it: the freed slot goes to the first ELIGIBLE waiter in fair
// order — within one (tenant, user) bucket, across buckets, and across the
// foreground/background classes.
func TestLane_ModelCapDoesNotBlockOtherModelsBehindIt(t *testing.T) {
	cases := []struct {
		name            string
		blocked, behind CallInfo
	}{
		{"same bucket",
			CallInfo{Owner: "a2", TenantID: "t", UserID: "u", Model: "A"},
			CallInfo{Owner: "b2", TenantID: "t", UserID: "u", Model: "B"}},
		{"another bucket",
			CallInfo{Owner: "a2", TenantID: "t", UserID: "u1", Model: "A"},
			CallInfo{Owner: "b2", TenantID: "t", UserID: "u2", Model: "B"}},
		{"background behind a blocked foreground waiter",
			CallInfo{Owner: "a2", Model: "A"},
			CallInfo{Owner: "b2", Model: "B", Background: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := capLane(2, LaneConfig{})
			a1 := laneNow(t, l, CallInfo{Owner: "a1", Model: "A"})
			x := laneNow(t, l, CallInfo{Owner: "x", Model: "B"})
			a2 := laneAsync(t, l, tc.blocked)
			b2 := laneAsync(t, l, tc.behind)
			x() // one slot frees: a2 is first in fair order but its model is at its cap
			mustGrant(t, b2, "the call to another model behind the blocked waiter")
			mustStillWait(t, a2, "the waiter whose model is at its cap")
			a1()
			mustGrant(t, a2, "the blocked waiter once its model's call ended")
		})
	}
}

// A cap on a plain model name covers that model at every context size: Ollama's
// identity is model@num_ctx, and a host that runs the model one request at a
// time does so whatever its context. A longer name that merely starts with the
// capped one is another model.
func TestLane_ModelCapOnAPlainNameCoversItsContextSizes(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 4, ModelCaps: map[string]int{"qwen3.6:latest": 1}})
	rel := laneNow(t, l, CallInfo{Owner: "r1", Model: "qwen3.6:latest@65536"})
	sameCtx := laneAsync(t, l, CallInfo{Owner: "r2", Model: "qwen3.6:latest@65536"})
	otherCtx := laneAsync(t, l, CallInfo{Owner: "r3", Model: "qwen3.6:latest@32768"})
	plain := laneAsync(t, l, CallInfo{Owner: "r4", Model: "qwen3.6:latest"})
	mustStillWait(t, sameCtx, "the capped model at the same context size")
	mustStillWait(t, otherCtx, "the capped model at another context size")
	mustStillWait(t, plain, "the capped model's plain name")
	laneNow(t, l, CallInfo{Owner: "r5", Model: "qwen3.6:latest-instruct@65536"})
	if st := l.Stats(); st.BlockedOnModel != 3 {
		t.Fatalf("stats = %+v, want BlockedOnModel 3", st)
	}
	rel()
	// The three take turns, one at a time.
	var next func()
	for i := 0; i < 3; i++ {
		select {
		case next = <-sameCtx:
		case next = <-otherCtx:
		case next = <-plain:
		case <-time.After(schedWait):
			t.Fatalf("turn %d: no waiter granted", i)
		}
		if st := l.Stats(); st.Queued != 2-i || st.BlockedOnModel != 2-i {
			t.Fatalf("turn %d: stats = %+v, want the others still held back", i, st)
		}
		next()
	}
}

// A run that comes back to its reservation while its model is at its cap
// cannot compute: it gives the reservation up (so a call to another model can
// use the slot) and queues; it is granted when its model's call ends. A run
// whose model is NOT at its cap still lands on its reserved slot — its own idle
// reservation is not a call in flight.
func TestLane_ModelCapOwnerReturnDropsItsReservationAndQueues(t *testing.T) {
	l := capLane(2, LaneConfig{AffinityHold: time.Minute})

	slots := make(chan int, 4)
	onSlot := func(n int) { slots <- n }
	laneNow(t, l, CallInfo{Owner: "r1", Model: "A", OnSlot: onSlot})()
	first := <-slots
	laneNow(t, l, CallInfo{Owner: "r1", Model: "A", OnSlot: onSlot})()
	if again := <-slots; again != first {
		t.Fatalf("r1 came back to slot %d, want its reserved slot %d", again, first)
	}
	if st := l.Stats(); st.Reserved != 1 || st.Active != 0 {
		t.Fatalf("stats = %+v, want r1's slot reserved", st)
	}

	// r2 takes the other slot with model A: A is now at its cap.
	r2 := laneNow(t, l, CallInfo{Owner: "r2", Model: "A"})
	r1 := laneAsync(t, l, CallInfo{Owner: "r1", Model: "A"})
	mustStillWait(t, r1, "r1 back while its model is at its cap")
	if st := l.Stats(); st.Reserved != 0 || st.Queued != 1 || st.BlockedOnModel != 1 {
		t.Fatalf("stats = %+v, want r1's reservation given up and r1 queued on its model", st)
	}
	// The slot it gave up serves another model. (A one-shot call, so it leaves
	// no reservation of its own behind.)
	laneNow(t, l, CallInfo{Model: "B"})()
	mustStillWait(t, r1, "r1 while r2's call to its model still runs")
	r2() // r2 keeps its own slot reserved; r1 takes the free one
	mustGrant(t, r1, "r1 once its model's call ended")
}

// Reconfigure: removing or raising a cap releases the waiters it held; adding
// one holds new calls of that model, counting the calls already in flight from
// before the cap existed.
func TestLane_ModelCapReconfigureReleasesAndHolds(t *testing.T) {
	l := capLane(4, LaneConfig{})
	laneNow(t, l, CallInfo{Owner: "a1", Model: "A"})
	a2 := laneAsync(t, l, CallInfo{Owner: "a2", Model: "A"})
	mustStillWait(t, a2, "a second call at the cap")
	l.Reconfigure(LaneConfig{MaxConcurrent: 4, ModelCaps: map[string]int{"A": 2}})
	mustGrant(t, a2, "the waiter after its cap was raised")
	a3 := laneAsync(t, l, CallInfo{Owner: "a3", Model: "A"})
	mustStillWait(t, a3, "a third call at the raised cap")
	l.Reconfigure(LaneConfig{MaxConcurrent: 4})
	mustGrant(t, a3, "the waiter after its cap was removed")

	// "B" ran uncapped; a cap added now counts the B call already in flight.
	l2 := NewLane(LaneConfig{MaxConcurrent: 4})
	b1 := laneNow(t, l2, CallInfo{Owner: "b1", Model: "B@8192"})
	l2.Reconfigure(LaneConfig{MaxConcurrent: 4, ModelCaps: map[string]int{"B": 1}})
	b2 := laneAsync(t, l2, CallInfo{Owner: "b2", Model: "B@8192"})
	mustStillWait(t, b2, "a call to a model capped while its first call ran")
	b1()
	mustGrant(t, b2, "the call once the model's earlier call ended")
}

// A bounded acquire counts a call held back by its model as queued, so a
// request outside any run is refused rather than parked beside a free slot
// once the bound is reached.
func TestLane_ModelCapBoundedAcquireRefusesAtTheQueueBound(t *testing.T) {
	l := capLane(4, LaneConfig{})
	laneNow(t, l, CallInfo{Model: "A"})
	_, err := l.AcquireBounded(context.Background(), CallInfo{Model: "A"}, 0)
	if !IsQueueFull(err) {
		t.Fatalf("AcquireBounded = %v, want *QueueFullError", err)
	}
	if rel, err := l.AcquireBounded(context.Background(), CallInfo{Model: "B"}, 0); err != nil {
		t.Fatalf("a call to another model: %v, want the free slot", err)
	} else {
		rel()
	}
}

// Under concurrent load with leases, quanta and cancellations, no model ever
// has more calls in flight than its cap, the lane never more than its slots,
// and every call that is not cancelled completes.
func TestLane_ModelCapHoldsUnderConcurrentLoad(t *testing.T) {
	caps := map[string]int{"A": 1, "B": 2}
	l := NewLane(LaneConfig{MaxConcurrent: 3, AffinityHold: time.Millisecond, QuantumCalls: 2, ModelCaps: caps})
	models := []string{"A", "B", "C"}
	var inFlight [3]atomic.Int32
	var peak [3]atomic.Int32
	var total, totalPeak atomic.Int32
	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("run-%d", g)
			for i := 0; i < 15; i++ {
				m := (g + i) % len(models)
				ctx, cancel := context.WithTimeout(context.Background(), schedWait)
				if i%7 == 6 {
					cancel() // some give up at once
				}
				rel, err := l.Acquire(ctx, CallInfo{Owner: owner, UserID: fmt.Sprint(g % 4), Model: models[m] + "@4096"})
				if err != nil {
					cancel()
					continue
				}
				n := inFlight[m].Add(1)
				for p := peak[m].Load(); n > p && !peak[m].CompareAndSwap(p, n); p = peak[m].Load() {
				}
				tn := total.Add(1)
				for p := totalPeak.Load(); tn > p && !totalPeak.CompareAndSwap(p, tn); p = totalPeak.Load() {
				}
				time.Sleep(50 * time.Microsecond)
				inFlight[m].Add(-1)
				total.Add(-1)
				rel()
				cancel()
			}
			l.Done(owner)
		}()
	}
	wg.Wait()
	for i, m := range models {
		if c, ok := caps[m]; ok && int(peak[i].Load()) > c {
			t.Errorf("model %s peaked at %d calls in flight, cap %d", m, peak[i].Load(), c)
		}
	}
	if totalPeak.Load() > 3 {
		t.Errorf("lane peaked at %d calls in flight, cap 3", totalPeak.Load())
	}
	if st := l.Stats(); st.Active != 0 || st.Queued != 0 || st.Reserved != 0 || st.BlockedOnModel != 0 {
		t.Errorf("stats after the load = %+v, want an idle lane", st)
	}
}

// EstimatedWait for a model at its own cap is that model's queue over that
// cap — even with lane slots free — while another model on the same lane still
// waits nothing.
func TestCallScheduler_EstimatedWaitForACappedModelReadsItsOwnQueue(t *testing.T) {
	const call = 10 * time.Second
	s := NewCallScheduler(map[string]ProviderLanesConfig{"p": {
		Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 4, ModelCaps: map[string]int{"m": 1}}},
	}})
	if w := s.EstimatedWait("p", "chat", "m", call); w != 0 {
		t.Fatalf("an idle lane: wait %s, want 0", w)
	}
	rel := schedNow(t, s, "p", "chat", CallInfo{Model: "m@65536"})
	if w := s.EstimatedWait("p", "chat", "m", call); w != call {
		t.Fatalf("the capped model at its cap, three slots free: wait %s, want %s (its one call)", w, call)
	}
	q := schedAsync(t, s, "p", "chat", CallInfo{Model: "m@65536"})
	eventually(t, "a call queued on its model", func() bool { return s.Stats()["p"].Lanes["chat"].BlockedOnModel == 1 })
	if w := s.EstimatedWait("p", "chat", "m", call); w != 2*call {
		t.Fatalf("the capped model with one queued: wait %s, want %s", w, 2*call)
	}
	if w := s.EstimatedWait("p", "chat", "other", call); w != 0 {
		t.Fatalf("another model with slots free: wait %s, want 0", w)
	}
	rel()
	mustGrant(t, q, "the queued call")()
}

// In a full lane, a call to another model waits only behind the queued calls
// that could take a freed slot: calls held back by their own model's cap are
// not ahead of it, however many there are.
func TestCallScheduler_EstimatedWaitInAFullLaneSkipsCallsHeldByTheirModelCap(t *testing.T) {
	const call = 10 * time.Second
	s := NewCallScheduler(map[string]ProviderLanesConfig{"p": {
		Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 2, ModelCaps: map[string]int{"m": 1}}},
	}})
	relM := schedNow(t, s, "p", "chat", CallInfo{Model: "m"})
	relX := schedNow(t, s, "p", "chat", CallInfo{Model: "x"}) // the lane is full
	var held []<-chan func()
	for i := 0; i < 3; i++ {
		held = append(held, schedAsync(t, s, "p", "chat", CallInfo{Model: "m"}))
	}
	eventually(t, "three calls held by m's cap", func() bool { return s.Stats()["p"].Lanes["chat"].BlockedOnModel == 3 })
	if w := s.EstimatedWait("p", "chat", "other", call); w != call/2 {
		t.Errorf("another model in a full lane, nothing it waits behind: wait %s, want %s — the calls held by m's cap were counted as ahead of it", w, call/2)
	}
	q := schedAsync(t, s, "p", "chat", CallInfo{Model: "y"})
	eventually(t, "y queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 4 })
	if w := s.EstimatedWait("p", "chat", "other", call); w != call {
		t.Errorf("one eligible call ahead: wait %s, want %s", w, call)
	}
	relX()
	mustGrant(t, q, "y")()
	relM()
	// m's cap lets one through at a time, in queue order — not slice order —
	// so release whichever is granted.
	for done := 0; done < len(held); done++ {
		granted := false
		for !granted {
			for _, c := range held {
				select {
				case rel := <-c:
					rel()
					granted = true
				default:
				}
				if granted {
					break
				}
			}
			if !granted {
				time.Sleep(time.Millisecond)
			}
		}
	}
}
