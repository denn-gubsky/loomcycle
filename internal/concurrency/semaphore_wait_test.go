package concurrency

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// TestSemaphore_ZeroTimeoutWaitsUntilRelease — a timeout of 0 means "no wait
// timeout": an over-cap Acquire with queue room waits until a slot frees. It
// used to arm a zero timer and refuse with backpressure at once.
func TestSemaphore_ZeroTimeoutWaitsUntilRelease(t *testing.T) {
	s := New(1, 4, 0)
	r1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		r, err := s.Acquire(context.Background())
		if err == nil {
			defer r()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("Acquire with timeout 0 returned before a release: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	r1()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Acquire after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting Acquire not admitted after release")
	}
}

// TestSemaphore_WaitAdmitsWhenQueueFull — Wait is for work already admitted:
// with every slot taken and the queue full (where Acquire is refused), and
// past the semaphore's own timeout, Wait still queues and gets the next slot.
// It does no per-user accounting.
func TestSemaphore_WaitAdmitsWhenQueueFull(t *testing.T) {
	s := New(1, 0, 10*time.Millisecond).WithPerUserCap(1)
	r1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if _, err := s.Acquire(context.Background()); !IsBackpressure(err) {
		t.Fatalf("Acquire with a full queue: err = %v, want backpressure", err)
	}

	got := make(chan error, 1)
	var rel func()
	go func() {
		r, err := s.Wait(context.Background())
		rel = r
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("Wait returned before a release: %v", err)
	case <-time.After(100 * time.Millisecond): // 10x the semaphore's timeout
	}
	if st := s.Stats(); st.Queued != 1 {
		t.Fatalf("queued = %d while Wait queues, want 1", st.Queued)
	}
	r1()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait not admitted after release")
	}
	if st := s.Stats(); st.Active != 1 || st.Queued != 0 || st.PerUser != nil {
		t.Fatalf("after Wait admitted: active=%d queued=%d per_user=%v, want 1/0/nil", st.Active, st.Queued, st.PerUser)
	}
	rel()
	rel() // idempotent
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("after release: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}
}

// TestSemaphore_WaitCancelLeavesCountsBalanced — a Wait ended by its ctx,
// including one that loses the race with a release handing it the slot, gives
// everything back: after a storm of cancelled and admitted Waits both counters
// are zero. Run with -race.
func TestSemaphore_WaitCancelLeavesCountsBalanced(t *testing.T) {
	s := New(1, 0, time.Second)

	// Deterministic: a queued Wait cancelled leaves nothing queued.
	r1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := s.Wait(ctx)
		got <- err
	}()
	waitQueued(t, s, 1)
	cancel()
	if err := <-got; err != context.Canceled {
		t.Fatalf("cancelled Wait: err = %v, want context.Canceled", err)
	}
	if st := s.Stats(); st.Active != 1 || st.Queued != 0 {
		t.Fatalf("after cancel: active=%d queued=%d, want 1/0", st.Active, st.Queued)
	}
	r1()

	// Storm: many short-lived Waits against two slots, so cancels race the
	// releases that hand them a slot (the stranded-token path).
	s = New(2, 0, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 2000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := time.Duration(rand.Intn(4000)) * time.Microsecond
			ctx, cancel := context.WithTimeout(context.Background(), d)
			defer cancel()
			r, err := s.Wait(ctx)
			if err != nil {
				return
			}
			time.Sleep(time.Duration(rand.Intn(200)) * time.Microsecond)
			r()
		}()
	}
	wg.Wait()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("after the storm: active=%d queued=%d, want 0/0 (leaked slot)", st.Active, st.Queued)
	}
}

// TryAcquire never waits: a free slot is taken, a full semaphore refuses at once
// even with room in its queue, a refusal leaves nothing queued, and it counts no
// user.
func TestSemaphore_TryAcquireTakesAFreeSlotAndNeverQueues(t *testing.T) {
	s := New(1, 8, time.Hour).WithPerUserCap(4)
	rel, err := s.TryAcquire()
	if err != nil {
		t.Fatalf("TryAcquire with a free slot: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.TryAcquire()
		done <- err
	}()
	select {
	case err := <-done:
		if !IsBackpressure(err) {
			t.Fatalf("TryAcquire with no free slot = %v, want backpressure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TryAcquire waited for a slot")
	}
	if st := s.Stats(); st.Active != 1 || st.Queued != 0 || len(st.PerUser) != 0 {
		t.Fatalf("after the refusal: active=%d queued=%d perUser=%v, want 1/0 and no user counted", st.Active, st.Queued, st.PerUser)
	}

	rel()
	rel() // idempotent
	if st := s.Stats(); st.Active != 0 {
		t.Fatalf("after release: active=%d, want 0", st.Active)
	}
}

// ReserveUser counts a request against its user's cap without taking a slot,
// for as long as the reservation is held — so a request that gives its slot up
// while it waits for something else stays counted.
func TestSemaphore_ReserveUserCountsAgainstTheCapWithoutTakingASlot(t *testing.T) {
	s := New(4, 8, time.Hour).WithPerUserCap(2)
	ctx := context.Background()
	r1, err := s.ReserveUser(ctx, "t", "alice")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.ReserveUser(ctx, "t", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Active != 0 || st.PerUser["t/alice"] != 2 {
		t.Fatalf("two reservations: active=%d perUser=%v, want no slot taken and 2 counted for t/alice", st.Active, st.PerUser)
	}
	if _, err := s.ReserveUser(ctx, "t", "alice"); !IsPerUserQuotaExhausted(err) {
		t.Fatalf("third reservation at a cap of 2 = %v, want per-user quota exhausted", err)
	}
	// The cap is one count shared with AcquireForUser, not a second one.
	if _, err := s.AcquireForUser(ctx, "t", "alice"); !IsPerUserQuotaExhausted(err) {
		t.Fatalf("AcquireForUser for a user at its cap by reservation = %v, want per-user quota exhausted", err)
	}
	// Another user, and the same name in another tenant, are not affected.
	if rel, err := s.ReserveUser(ctx, "t", "bob"); err != nil {
		t.Fatalf("another user: %v", err)
	} else {
		rel()
	}
	if rel, err := s.ReserveUser(ctx, "other", "alice"); err != nil {
		t.Fatalf("same user id in another tenant: %v", err)
	} else {
		rel()
	}

	r1()
	r1() // idempotent
	if st := s.Stats(); st.PerUser["t/alice"] != 1 {
		t.Fatalf("after one release: perUser=%v, want 1 for t/alice", st.PerUser)
	}
	r2()
	if st := s.Stats(); len(st.PerUser) != 0 {
		t.Fatalf("after both releases: perUser=%v, want none", st.PerUser)
	}
}

// With no cap, or no user, there is nothing to count.
func TestSemaphore_ReserveUserIsANoopWithoutACapOrAUser(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		rel, err := New(1, 1, time.Hour).ReserveUser(ctx, "t", "alice")
		if err != nil {
			t.Fatalf("no cap configured: %v", err)
		}
		rel()
	}
	s := New(1, 1, time.Hour).WithPerUserCap(1)
	for i := 0; i < 5; i++ {
		if _, err := s.ReserveUser(ctx, "t", ""); err != nil {
			t.Fatalf("empty user: %v", err)
		}
	}
	if st := s.Stats(); len(st.PerUser) != 0 {
		t.Fatalf("perUser=%v, want nothing counted", st.PerUser)
	}
}

func waitQueued(t *testing.T, s *Semaphore, want int) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if s.Stats().Queued == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("queued = %d, want %d", s.Stats().Queued, want)
}
