package concurrency

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// admitWithSlot admits a run bound to a fresh RunSlot.
func admitWithSlot(t *testing.T, s *Semaphore, tenant, user string) (*RunSlot, func()) {
	t.Helper()
	rs := &RunSlot{}
	release, err := s.Admit(context.Background(), AdmitRequest{TenantID: tenant, UserID: user, Slot: rs})
	if err != nil {
		t.Fatal(err)
	}
	return rs, release
}

// startRunAsync admits a run in the background; the channel yields its release
// once it has a slot.
func startRunAsync(s *Semaphore, user string) chan func() {
	got := make(chan func(), 1)
	go func() {
		release, err := s.Admit(context.Background(), AdmitRequest{UserID: user})
		if err == nil {
			got <- release
		}
	}()
	return got
}

func admittedWithin(t *testing.T, got chan func(), d time.Duration) (func(), bool) {
	t.Helper()
	select {
	case r := <-got:
		return r, true
	case <-time.After(d):
		return nil, false
	}
}

// A run that lends its slot lets a waiting run start; taking it back, it goes
// ahead of every new run, and waits only for a slot to free.
func TestRunSlot_LendLetsAWaitingRunStartAndReclaimGoesFirst(t *testing.T) {
	s := New(1, 8, 0)
	rs, endA := admitWithSlot(t, s, "", "alice")

	b := startRunAsync(s, "bob")
	if _, ok := admittedWithin(t, b, 50*time.Millisecond); ok {
		t.Fatal("bob started while alice held the only slot")
	}
	rs.Lend()
	endB, ok := admittedWithin(t, b, time.Second)
	if !ok {
		t.Fatal("alice lent her slot and bob did not start")
	}
	if !rs.Lent() {
		t.Fatal("Lent() = false after Lend")
	}

	// carol queues behind bob as a NEW run; alice reclaims after her.
	c := startRunAsync(s, "carol")
	waitQueued(t, s, 1)
	reclaimed := make(chan error, 1)
	go func() { reclaimed <- rs.Reclaim(context.Background()) }()
	waitQueued(t, s, 2)
	endB()
	select {
	case err := <-reclaimed:
		if err != nil {
			t.Fatalf("Reclaim: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bob ended and alice did not get her slot back")
	}
	if _, ok := admittedWithin(t, c, 50*time.Millisecond); ok {
		t.Fatal("carol, a new run, went ahead of alice reclaiming")
	}
	endA()
	endC, ok := admittedWithin(t, c, time.Second)
	if !ok {
		t.Fatal("alice ended and carol did not start")
	}
	endC()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("after all ended: %+v, want nothing active or queued", st)
	}
}

// A run that ends while its slot is lent gives back its user's count but no
// global slot — it holds none — so the runtime never counts a slot twice.
func TestRunSlot_EndingWhileLentGivesBackNoGlobalSlot(t *testing.T) {
	s := New(2, 8, 0).WithPerUserCap(1)
	rs, endA := admitWithSlot(t, s, "", "alice")
	rs.Lend()
	_, endB := admitWithSlot(t, s, "", "bob")
	_, endC := admitWithSlot(t, s, "", "carol")
	if st := s.Stats(); st.Active != 2 {
		t.Fatalf("active = %d with alice's slot lent and two runs started, want 2", st.Active)
	}
	endA()
	if st := s.Stats(); st.Active != 2 {
		t.Errorf("active = %d after alice ended lent, want 2 — her end freed a slot she did not hold", st.Active)
	}
	// Her user's count is given back: alice may start again.
	if _, err := s.Admit(context.Background(), AdmitRequest{UserID: "alice", Reject: true}); err == nil {
		t.Error("alice started a run with no free slot")
	}
	endB()
	endC()
	rs2, endA2 := admitWithSlot(t, s, "", "alice")
	_ = rs2
	endA2()
	if st := s.Stats(); st.Active != 0 {
		t.Errorf("active = %d at the end, want 0", st.Active)
	}
	endA() // idempotent
	if st := s.Stats(); st.Active != 0 {
		t.Errorf("a second end changed the count: %+v", st)
	}
}

// A reclaim whose ctx ends leaves the slot lent; the run's end then gives back
// no global slot. A Lend or Reclaim on a slot not bound, or a nil one, does
// nothing.
func TestRunSlot_ACancelledReclaimLeavesTheSlotLent(t *testing.T) {
	s := New(1, 8, 0)
	rs, endA := admitWithSlot(t, s, "", "alice")
	rs.Lend()
	_, endB := admitWithSlot(t, s, "", "bob")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rs.Reclaim(ctx) }()
	waitQueued(t, s, 1)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled reclaim reported a slot")
	}
	if !rs.Lent() {
		t.Fatal("the slot is not lent after a cancelled reclaim")
	}
	endA()
	if st := s.Stats(); st.Active != 1 || st.Queued != 0 {
		t.Errorf("after alice ended lent: %+v, want bob's one slot and nothing queued", st)
	}
	endB()

	var nilSlot *RunSlot
	nilSlot.Lend()
	if err := nilSlot.Reclaim(context.Background()); err != nil || nilSlot.Lent() {
		t.Error("a nil slot lent or reclaimed")
	}
	unbound := &RunSlot{}
	unbound.Lend()
	if unbound.Lent() {
		t.Error("a slot no Admit bound was lent")
	}
}

// Under load, lending never lets more runs hold a global slot than the cap:
// runs lend and reclaim at random while others start and end.
func TestRunSlot_LendingNeverExceedsTheCap(t *testing.T) {
	const cap, runs = 3, 24
	s := New(cap, 1000, 0)
	var holding, peak atomic.Int64
	note := func(d int64) {
		n := holding.Add(d)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rs := &RunSlot{}
			end, err := s.Admit(context.Background(), AdmitRequest{UserID: "u", Slot: rs})
			if err != nil {
				t.Error(err)
				return
			}
			note(1)
			for k := 0; k < 3; k++ {
				note(-1)
				rs.Lend()
				time.Sleep(time.Duration(i%3) * time.Millisecond)
				if err := rs.Reclaim(context.Background()); err != nil {
					t.Error(err)
				}
				note(1)
				time.Sleep(time.Millisecond)
			}
			note(-1)
			end()
		}(i)
	}
	wg.Wait()
	if p := peak.Load(); p > cap {
		t.Errorf("peak runs holding a slot = %d, cap %d", p, cap)
	}
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("after all ended: %+v", st)
	}
}

// A lent run counts against the queue bound with the queued ones: lending is
// refused at the bound, and so is a new run. Runs alive stay within the cap
// plus the queue bound, whatever the per-user cap (off here).
func TestRunSlot_LentRunsCountAgainstTheQueueBound(t *testing.T) {
	s := New(1, 1, 0)
	a, endA := admitWithSlot(t, s, "", "")
	if !a.Lend() {
		t.Fatal("lending refused with the queue empty")
	}
	b, endB := admitWithSlot(t, s, "", "") // takes the lent slot
	if _, err := s.Admit(context.Background(), AdmitRequest{}); !IsBackpressure(err) {
		t.Errorf("a new run with one run lent and the bound at 1: err = %v, want backpressure", err)
	}
	if b.Lend() {
		t.Error("a second run lent past the queue bound")
	}
	if st := s.Stats(); st.Active != 1 {
		t.Errorf("active = %d, want b's 1", st.Active)
	}
	endB()
	if !a.TryReclaim() {
		t.Fatal("TryReclaim failed with a slot free")
	}
	endA()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("after both ended: %+v", st)
	}
	// The bound is free again: a run may lend, and one that ends lent gives
	// back no slot it does not hold.
	c, endC := admitWithSlot(t, s, "", "")
	if !c.Lend() {
		t.Error("lending refused after the lent run took its slot back and ended")
	}
	endC()
	if st := s.Stats(); st.Active != 0 {
		t.Errorf("a run that ended lent left active = %d", st.Active)
	}
}
