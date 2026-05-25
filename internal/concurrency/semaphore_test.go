package concurrency

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestImmediateAcquire(t *testing.T) {
	s := New(2, 4, time.Second)
	r1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Active != 2 || st.Queued != 0 {
		t.Errorf("active=%d queued=%d", st.Active, st.Queued)
	}
	r1()
	r2()
}

func TestQueueWakesWaiter(t *testing.T) {
	s := New(1, 4, time.Second)
	r1, _ := s.Acquire(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r2, err := s.Acquire(context.Background())
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		r2()
	}()

	// Give the goroutine time to enqueue.
	time.Sleep(10 * time.Millisecond)
	if st := s.Stats(); st.Active != 1 || st.Queued != 1 {
		t.Errorf("expected 1 active 1 queued; got %d / %d", st.Active, st.Queued)
	}
	r1()
	wg.Wait()
}

func TestQueueFullReturnsBackpressure(t *testing.T) {
	s := New(1, 0, 10*time.Millisecond) // queue depth 0 = no waiting
	r1, _ := s.Acquire(context.Background())
	defer r1()
	_, err := s.Acquire(context.Background())
	if !IsBackpressure(err) {
		t.Errorf("expected backpressure, got %v", err)
	}
}

func TestQueueTimeoutReturnsBackpressure(t *testing.T) {
	s := New(1, 4, 20*time.Millisecond)
	r1, _ := s.Acquire(context.Background())
	defer r1()

	start := time.Now()
	_, err := s.Acquire(context.Background())
	if !IsBackpressure(err) {
		t.Errorf("expected backpressure, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("waited too long: %v", elapsed)
	}
}

func TestCtxCancelDuringWait(t *testing.T) {
	s := New(1, 4, time.Second)
	r1, _ := s.Acquire(context.Background())
	defer r1()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := s.Acquire(ctx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// Regression: when releaseFn races a waiter's ctx-cancellation, the slot
// transfer can be silently stranded (release won the race, removed the
// waiter from the queue, sent to its chan, but the waiter goroutine took
// the ctx.Done() branch and returned without picking up the slot). After
// many such races the active counter drifts up and never recovers.
//
// We synthesise the race by running many short-timeout acquires against a
// small pool. After everything settles, both counters must be zero.
func TestSemaphoreNoLeakOnCancellationRace(t *testing.T) {
	const (
		concurrency = 2
		queueDepth  = 64
		iterations  = 5000
	)
	s := New(concurrency, queueDepth, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Variable timeout (0–4 ms) so cancellations happen at
			// many different points relative to a release.
			d := time.Duration(i%5) * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), d)
			defer cancel()
			release, err := s.Acquire(ctx)
			if err == nil {
				time.Sleep(time.Microsecond) // brief work
				release()
			}
		}(i)
	}
	wg.Wait()

	// Let any in-flight transfers settle before sampling Stats.
	time.Sleep(50 * time.Millisecond)
	st := s.Stats()
	if st.Active != 0 || st.Queued != 0 {
		t.Errorf("after %d racy iterations: active=%d queued=%d, want 0/0 (slot leak)", iterations, st.Active, st.Queued)
	}
}

// ---- v0.10.1 per-tenant fairness ----

// TestAcquireForUser_NoCapBehavesLikeAcquire pins the back-compat
// guarantee: when maxPerUser==0 (the default for new semaphores
// constructed without WithPerUserCap), AcquireForUser is identical
// to Acquire, regardless of userID. Existing call sites that don't
// care about per-tenant fairness see no behavior change.
func TestAcquireForUser_NoCapBehavesLikeAcquire(t *testing.T) {
	s := New(2, 4, time.Second) // no WithPerUserCap call
	r1, err := s.AcquireForUser(context.Background(), "user_a")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.AcquireForUser(context.Background(), "user_a")
	if err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Active != 2 {
		t.Errorf("active=%d, want 2", st.Active)
	}
	if st.PerUser != nil {
		t.Errorf("PerUser should be nil when no cap configured, got %v", st.PerUser)
	}
	r1()
	r2()
}

// TestAcquireForUser_EmptyUserIDSkipsCheck — anonymous calls bypass
// the per-user cap even when the cap is configured.
func TestAcquireForUser_EmptyUserIDSkipsCheck(t *testing.T) {
	s := New(10, 4, time.Second).WithPerUserCap(2)
	// Three acquires with empty userID should all succeed.
	for i := 0; i < 3; i++ {
		r, err := s.AcquireForUser(context.Background(), "")
		if err != nil {
			t.Fatalf("empty-userID acquire %d: %v", i, err)
		}
		defer r()
	}
	if st := s.Stats(); len(st.PerUser) != 0 {
		t.Errorf("empty-userID acquires leaked into PerUser: %v", st.PerUser)
	}
}

// TestAcquireForUser_CapRefusesAtLimit — the load-bearing test.
// With cap=2, a third acquire by the same user returns
// *ErrPerUserQuotaExhausted. Different users each get their own
// quota.
func TestAcquireForUser_CapRefusesAtLimit(t *testing.T) {
	s := New(10, 4, time.Second).WithPerUserCap(2)
	r1, err := s.AcquireForUser(context.Background(), "user_a")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	r2, err := s.AcquireForUser(context.Background(), "user_a")
	if err != nil {
		t.Fatal(err)
	}
	defer r2()

	// Third acquire by user_a hits the cap.
	_, err = s.AcquireForUser(context.Background(), "user_a")
	if !IsPerUserQuotaExhausted(err) {
		t.Errorf("third acquire: got %v, want *ErrPerUserQuotaExhausted", err)
	}
	var pue *ErrPerUserQuotaExhausted
	if !errors.As(err, &pue) {
		t.Fatalf("errors.As: %v", err)
	}
	if pue.UserID != "user_a" || pue.Cap != 2 {
		t.Errorf("error fields: user=%q cap=%d, want user_a / 2", pue.UserID, pue.Cap)
	}
	if pue.Code() != "per_user_quota_exhausted" {
		t.Errorf("Code() = %q, want per_user_quota_exhausted", pue.Code())
	}

	// user_b is unaffected — independent quota.
	r3, err := s.AcquireForUser(context.Background(), "user_b")
	if err != nil {
		t.Errorf("user_b first acquire: %v", err)
	} else {
		defer r3()
	}

	// Stats reflect both users.
	st := s.Stats()
	if st.PerUser["user_a"] != 2 {
		t.Errorf("user_a count = %d, want 2", st.PerUser["user_a"])
	}
	if st.PerUser["user_b"] != 1 {
		t.Errorf("user_b count = %d, want 1", st.PerUser["user_b"])
	}
}

// TestAcquireForUser_ReleaseDecrementsPerUser — after release, the
// per-user count drops AND the map entry is pruned when the count
// hits zero (so Stats().PerUser doesn't accumulate stale zero
// entries for users no longer in flight).
func TestAcquireForUser_ReleaseDecrementsPerUser(t *testing.T) {
	s := New(10, 4, time.Second).WithPerUserCap(4)
	r, _ := s.AcquireForUser(context.Background(), "user_a")
	if s.Stats().PerUser["user_a"] != 1 {
		t.Fatalf("after acquire, expected 1; got %d", s.Stats().PerUser["user_a"])
	}
	r()
	st := s.Stats()
	if _, ok := st.PerUser["user_a"]; ok {
		t.Errorf("after release, user_a should be pruned from PerUser; got %v", st.PerUser)
	}
}

// TestAcquireForUser_QueuedCountsAgainstCap — the cap applies to
// active+queued, not just active. Without this, a user could fill
// the queue with their own runs while at active-cap, starving
// everyone else for the queue's lifetime.
func TestAcquireForUser_QueuedCountsAgainstCap(t *testing.T) {
	// 1 concurrent slot, 4 queue slots, per-user cap of 2.
	s := New(1, 4, 100*time.Millisecond).WithPerUserCap(2)

	// First acquire by user_a takes the active slot.
	r1, err := s.AcquireForUser(context.Background(), "user_a")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	// Second acquire by user_a goes into the queue (active+queued = 2
	// for user_a, exactly at cap). Run in a goroutine so we don't block.
	queued := make(chan error, 1)
	go func() {
		_, err := s.AcquireForUser(context.Background(), "user_a")
		queued <- err
	}()
	time.Sleep(20 * time.Millisecond) // let it enqueue

	// Third acquire by user_a should be refused — user_a is at cap
	// (1 active + 1 queued = 2).
	_, err = s.AcquireForUser(context.Background(), "user_a")
	if !IsPerUserQuotaExhausted(err) {
		t.Errorf("third acquire: got %v, want per_user_quota_exhausted", err)
	}

	// Let the queued acquire timeout to clean up.
	<-queued
}

// TestAcquireForUser_CancelDecrementsPerUser — ctx.Cancel mid-queue
// must decrement perUser, otherwise stale counts leak.
func TestAcquireForUser_CancelDecrementsPerUser(t *testing.T) {
	s := New(1, 4, time.Second).WithPerUserCap(2)
	r1, _ := s.AcquireForUser(context.Background(), "user_a")
	defer r1()

	// user_a queues a second acquire, then cancels.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.AcquireForUser(ctx, "user_a")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	// At this point user_a has 1 active + 1 queued = 2 in flight.
	if c := s.Stats().PerUser["user_a"]; c != 2 {
		t.Errorf("during queue: user_a count = %d, want 2", c)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("cancelled acquire: got %v, want ctx.Canceled", err)
	}
	// After cancel, user_a count returns to 1 (just the active slot).
	time.Sleep(20 * time.Millisecond) // let cancelWaiter run
	if c := s.Stats().PerUser["user_a"]; c != 1 {
		t.Errorf("after cancel: user_a count = %d, want 1 (queued one was cleaned up)", c)
	}
}

// ---- v0.12.1 cluster-mode (userQuotaGate) tests ----

// stubQuotaGate is the in-process fake for the v0.12.1 DB-backed
// per-user counter. Lets us test Semaphore's cluster-mode dispatch
// without standing up Postgres.
type stubQuotaGate struct {
	mu       sync.Mutex
	counts   map[string]int
	failNext bool
	atCapFor map[string]bool // when true, TryAcquire returns false+nil for this user
	releases int
}

func newStubQuotaGate() *stubQuotaGate {
	return &stubQuotaGate{counts: map[string]int{}, atCapFor: map[string]bool{}}
}

func (g *stubQuotaGate) TryAcquire(_ context.Context, userID string, _ int) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failNext {
		g.failNext = false
		return false, errors.New("stub: simulated failure")
	}
	if g.atCapFor[userID] {
		return false, nil
	}
	g.counts[userID]++
	return true, nil
}

func (g *stubQuotaGate) Release(_ context.Context, userID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.counts[userID] > 0 {
		g.counts[userID]--
	}
	g.releases++
}

func (g *stubQuotaGate) Snapshot(_ context.Context) (map[string]int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.counts) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(g.counts))
	for k, v := range g.counts {
		if v > 0 {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (g *stubQuotaGate) count(userID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counts[userID]
}

func (g *stubQuotaGate) releaseCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.releases
}

// TestSemaphore_ClusterMode_DelegatesToQuotaGate verifies that when
// the quotaStore is wired, AcquireForUser calls the gate's
// TryAcquire (not the in-memory perUser path) and the release func
// triggers an asynchronous gate Release. The in-memory perUser map
// must stay empty throughout — cluster mode is authoritative on the
// DB side.
func TestSemaphore_ClusterMode_DelegatesToQuotaGate(t *testing.T) {
	gate := newStubQuotaGate()
	s := New(10, 10, time.Second).WithPerUserCap(3).WithUserQuotaStore(gate)

	r, err := s.AcquireForUser(context.Background(), "alice")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if gate.count("alice") != 1 {
		t.Errorf("gate count = %d, want 1", gate.count("alice"))
	}
	// In-memory map must NOT be touched in cluster mode.
	s.mu.Lock()
	got := s.perUser["alice"]
	s.mu.Unlock()
	if got != 0 {
		t.Errorf("in-memory perUser[alice] = %d in cluster mode, want 0", got)
	}

	r()
	// Release is async (background goroutine). Poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if gate.count("alice") == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gate.count("alice") != 0 {
		t.Errorf("after release: gate count = %d, want 0", gate.count("alice"))
	}
}

// TestSemaphore_ClusterMode_AtCapReturnsTypedError verifies that a
// false+nil from the gate (user is at cap) becomes the canonical
// *ErrPerUserQuotaExhausted that HTTP handlers map to 429.
func TestSemaphore_ClusterMode_AtCapReturnsTypedError(t *testing.T) {
	gate := newStubQuotaGate()
	gate.atCapFor["alice"] = true
	s := New(10, 10, time.Second).WithPerUserCap(3).WithUserQuotaStore(gate)

	_, err := s.AcquireForUser(context.Background(), "alice")
	if !IsPerUserQuotaExhausted(err) {
		t.Errorf("got %v, want *ErrPerUserQuotaExhausted", err)
	}
	var pue *ErrPerUserQuotaExhausted
	if errors.As(err, &pue) {
		if pue.UserID != "alice" {
			t.Errorf("UserID = %q, want alice", pue.UserID)
		}
		if pue.Cap != 3 {
			t.Errorf("Cap = %d, want 3", pue.Cap)
		}
	}
}

// TestSemaphore_ClusterMode_QueueFullCompensateReleases verifies the
// compensate-release path: when TryAcquire succeeds on the DB but
// the global queue rejects (queue full), the Semaphore must Release
// the DB slot so the cluster-wide count stays balanced.
func TestSemaphore_ClusterMode_QueueFullCompensateReleases(t *testing.T) {
	gate := newStubQuotaGate()
	// max_concurrent=1, max_queue=0 → second acquire always hits "queue full".
	s := New(1, 0, time.Second).WithPerUserCap(5).WithUserQuotaStore(gate)

	// First acquire takes the global slot.
	r, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer r()

	// Second acquire (with userID) calls gate.TryAcquire successfully
	// but then hits queue-full backpressure.
	_, err = s.AcquireForUser(context.Background(), "alice")
	if !IsBackpressure(err) {
		t.Fatalf("got %v, want *BackpressureError", err)
	}

	// Compensate-release runs in a goroutine; poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if gate.count("alice") == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gate.count("alice") != 0 {
		t.Errorf("after queue-full reject: gate count = %d, want 0 (compensate-release failed)", gate.count("alice"))
	}
}

// TestSemaphore_ClusterMode_GateFailureWrapsError verifies a real
// infrastructure failure from the gate propagates as a wrapped error
// (NOT as a quota-exhausted false signal). Distinguishes "user is at
// cap" (false+nil → typed error) from "DB is down" (false+error →
// wrapped 5xx-shaped error).
func TestSemaphore_ClusterMode_GateFailureWrapsError(t *testing.T) {
	gate := newStubQuotaGate()
	gate.failNext = true
	s := New(10, 10, time.Second).WithPerUserCap(3).WithUserQuotaStore(gate)

	_, err := s.AcquireForUser(context.Background(), "alice")
	if err == nil {
		t.Fatal("expected error from gate failure")
	}
	if IsPerUserQuotaExhausted(err) {
		t.Errorf("gate failure should NOT be classified as quota exhausted: %v", err)
	}
}

// TestSemaphore_ClusterMode_StatsFromSnapshot verifies that Stats()
// reads PerUser from the gate's Snapshot (not the in-memory map)
// when in cluster mode. The Active/Queued counters remain
// in-memory (per-replica accounting).
func TestSemaphore_ClusterMode_StatsFromSnapshot(t *testing.T) {
	gate := newStubQuotaGate()
	s := New(10, 10, time.Second).WithPerUserCap(3).WithUserQuotaStore(gate)

	r, _ := s.AcquireForUser(context.Background(), "alice")
	defer r()

	st := s.Stats()
	if st.PerUser["alice"] != 1 {
		t.Errorf("Stats.PerUser[alice] = %d, want 1 (from gate.Snapshot)", st.PerUser["alice"])
	}
	if st.Active != 1 {
		t.Errorf("Stats.Active = %d, want 1 (per-replica counter)", st.Active)
	}
}

// TestSemaphore_ClusterMode_QueuedWaiterWakesAndReleases pins the
// cluster-mode hot path that no other test covers: a request queues
// (TryAcquire fires at enqueue), then wakes when the global slot
// frees, then completes and releases. Exactly one TryAcquire + one
// Release for the user — a future regression that double-TryAcquires
// on wakeup, or skips the eventual Release, fails here.
// (Review-1 finding #2.)
func TestSemaphore_ClusterMode_QueuedWaiterWakesAndReleases(t *testing.T) {
	gate := newStubQuotaGate()
	s := New(1, 4, time.Second).WithPerUserCap(3).WithUserQuotaStore(gate)

	// Hold the single global slot with an anonymous Acquire — no
	// per-user gate involvement, so the gate's only interaction is
	// alice's enqueue + later release.
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold acquire: %v", err)
	}

	done := make(chan func(), 1)
	go func() {
		r, err := s.AcquireForUser(context.Background(), "alice")
		if err != nil {
			t.Errorf("queued acquire: %v", err)
			done <- nil
			return
		}
		done <- r
	}()

	// Give alice's goroutine time to enqueue and call TryAcquire.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if gate.count("alice") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gate.count("alice") != 1 {
		t.Fatalf("after enqueue: gate count = %d, want 1 (TryAcquire should fire at enqueue)", gate.count("alice"))
	}

	hold() // wake alice
	r := <-done
	if r == nil {
		t.Fatal("alice's release func is nil — acquire failed")
	}
	r() // alice releases

	// Background Release; poll briefly.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if gate.count("alice") == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gate.count("alice") != 0 {
		t.Errorf("after release: gate count = %d, want 0", gate.count("alice"))
	}
	if r := gate.releaseCount(); r != 1 {
		t.Errorf("gate.releaseCount = %d, want exactly 1 (no double-release on wakeup)", r)
	}
}
