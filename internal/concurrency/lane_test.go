package concurrency

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two bounds shared by the scheduler tests: how long something that must
// happen may take, and how long something that must NOT happen is watched for.
const (
	schedWait   = 5 * time.Second
	schedSettle = 30 * time.Millisecond
)

// schedCtx is a context that ends with the test, so an acquire a test leaves
// queued on purpose does not outlive it.
func schedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// acquireAsync runs acquire on its own goroutine and yields its release once
// it is granted. An error is a test failure unless ctx (the test's) has ended.
func acquireAsync(t *testing.T, ctx context.Context, acquire func(context.Context) (func(), error)) <-chan func() {
	t.Helper()
	ch := make(chan func(), 1)
	go func() {
		rel, err := acquire(ctx)
		if err != nil {
			if ctx.Err() == nil {
				t.Errorf("acquire: %v", err)
			}
			return
		}
		ch <- rel
	}()
	return ch
}

// mustGrant waits for an async acquire to be granted and returns its release.
func mustGrant(t *testing.T, ch <-chan func(), what string) func() {
	t.Helper()
	select {
	case rel := <-ch:
		return rel
	case <-time.After(schedWait):
		t.Fatalf("%s: not granted within %s", what, schedWait)
		return nil
	}
}

// mustStillWait asserts an async acquire has not been granted.
func mustStillWait(t *testing.T, ch <-chan func(), what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s: granted, want it still waiting", what)
	case <-time.After(schedSettle):
	}
}

// eventually polls cond until it holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(schedWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached within %s", what, schedWait)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// laneNow acquires on a lane that must grant at once.
func laneNow(t *testing.T, l *Lane, c CallInfo) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), schedWait)
	defer cancel()
	rel, err := l.Acquire(ctx, c)
	if err != nil {
		t.Fatalf("Acquire(%+v) = %v, want an immediate grant", c, err)
	}
	return rel
}

// laneAsync queues an acquire and waits until the lane counts it as queued, so
// consecutive calls queue in a known order.
func laneAsync(t *testing.T, l *Lane, c CallInfo) <-chan func() {
	t.Helper()
	before := l.Stats().Queued
	ch := acquireAsync(t, schedCtx(t), func(ctx context.Context) (func(), error) { return l.Acquire(ctx, c) })
	eventually(t, "acquire queued", func() bool { return l.Stats().Queued > before || len(ch) > 0 })
	return ch
}

// grantOrder queues one acquire per CallInfo, in order, behind a held slot of a
// one-slot lane, then frees the slot and returns the order the calls ran in
// (each named by its Owner).
func grantOrder(t *testing.T, l *Lane, calls []CallInfo) []string {
	t.Helper()
	hold := laneNow(t, l, CallInfo{})
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, c := range calls {
		before := l.Stats().Queued
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(context.Background(), c)
			if err != nil {
				t.Errorf("Acquire(%s): %v", c.Owner, err)
				return
			}
			mu.Lock()
			order = append(order, c.Owner)
			mu.Unlock()
			rel()
		}()
		eventually(t, "call queued", func() bool { return l.Stats().Queued == before+1 })
	}
	hold()
	wg.Wait()
	return order
}

func TestLane_CapsCallsInFlightAtMaxConcurrent(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 2})
	r1 := laneNow(t, l, CallInfo{Owner: "a"})
	laneNow(t, l, CallInfo{Owner: "b"})
	c := laneAsync(t, l, CallInfo{Owner: "c"})
	mustStillWait(t, c, "third call on a two-slot lane")
	if st := l.Stats(); st.Active != 2 || st.Queued != 1 {
		t.Fatalf("stats = %+v, want Active 2 Queued 1", st)
	}
	r1()
	mustGrant(t, c, "third call after a release")
	if st := l.Stats(); st.Active != 2 || st.Queued != 0 || st.Grants != 3 {
		t.Fatalf("stats = %+v, want Active 2 Queued 0 Grants 3", st)
	}
}

func TestLane_QueuedAcquireEndsOnlyWhenContextEnds(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	laneNow(t, l, CallInfo{Owner: "a"})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := l.Acquire(ctx, CallInfo{Owner: "b"}); errc <- err }()
	eventually(t, "b queued", func() bool { return l.Stats().Queued == 1 })
	select {
	case err := <-errc:
		t.Fatalf("queued Acquire returned %v on its own, want it to wait", err)
	case <-time.After(schedSettle):
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire = %v, want context.Canceled", err)
		}
	case <-time.After(schedWait):
		t.Fatal("cancelled Acquire did not return")
	}
	if st := l.Stats(); st.Queued != 0 || st.Active != 1 {
		t.Fatalf("stats = %+v, want Queued 0 Active 1", st)
	}
}

func TestLane_QueueHasNoDepthLimit(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	hold := laneNow(t, l, CallInfo{})
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(context.Background(), CallInfo{})
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			rel()
		}()
	}
	eventually(t, "all queued", func() bool { return l.Stats().Queued == n })
	hold()
	wg.Wait()
	if st := l.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Fatalf("stats = %+v, want an empty lane", st)
	}
}

func TestLane_CancelledContextTakesNoSlot(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, CallInfo{Owner: "a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	if st := l.Stats(); st.Active != 0 || st.Reserved != 0 || st.Grants != 0 {
		t.Fatalf("stats = %+v, want an untouched lane", st)
	}
}

func TestLane_NoHoldHandsSlotToWaiterAtRelease(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	a := laneNow(t, l, CallInfo{Owner: "a"})
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	a()
	mustGrant(t, b, "waiter after release")
	if st := l.Stats(); st.Reserved != 0 {
		t.Fatalf("Reserved = %d, want 0 with no hold", st.Reserved)
	}
}

func TestLane_OwnerInsideHoldTakesSlotAheadOfQueue(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	laneNow(t, l, CallInfo{Owner: "a"})()
	if st := l.Stats(); st.Active != 0 || st.Reserved != 1 {
		t.Fatalf("stats = %+v, want the slot idle but reserved", st)
	}
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	mustStillWait(t, b, "b during a's hold")
	// a returns inside its hold: it gets its slot although b queued first.
	laneNow(t, l, CallInfo{Owner: "a"})
	mustStillWait(t, b, "b while a's second call runs")
	if st := l.Stats(); st.Active != 1 || st.Reserved != 0 || st.Queued != 1 {
		t.Fatalf("stats = %+v, want Active 1 Reserved 0 Queued 1", st)
	}
}

func TestLane_HoldExpiryHandsSlotToWaiterQueuedDuringHold(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: 150 * time.Millisecond})
	laneNow(t, l, CallInfo{Owner: "a"})()
	// b queues DURING a's hold. It must get the slot when the hold runs out,
	// with a never calling again.
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	if st := l.Stats(); st.Reserved != 1 {
		t.Fatalf("Reserved = %d, want b to have queued inside a's hold", st.Reserved)
	}
	mustGrant(t, b, "waiter at hold expiry")
}

func TestLane_HoldExpiryFreesSlotWithNobodyWaiting(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: 5 * time.Millisecond})
	laneNow(t, l, CallInfo{Owner: "a"})()
	eventually(t, "reservation expired", func() bool { return l.Stats().Reserved == 0 })
	laneNow(t, l, CallInfo{Owner: "b"})
}

func TestLane_QuantumSpentYieldsSlotAtRelease(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, QuantumCalls: 2, AffinityHold: time.Minute})
	laneNow(t, l, CallInfo{Owner: "a"})() // call 1
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	a2 := laneNow(t, l, CallInfo{Owner: "a"}) // call 2: still inside the quantum
	mustStillWait(t, b, "b while a is inside its quantum")
	a2() // quantum spent and b waits: the slot goes to b now, not after the hold
	mustGrant(t, b, "waiter once the owner's quantum is spent")
	if st := l.Stats(); st.Preemptions != 1 {
		t.Fatalf("Preemptions = %d, want 1", st.Preemptions)
	}
}

func TestLane_QuantumSpentOwnerQueuesBehindWaiter(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, QuantumCalls: 2, AffinityHold: time.Minute})
	// Nobody waits while a spends its quantum, so it keeps the reservation.
	laneNow(t, l, CallInfo{Owner: "a"})()
	laneNow(t, l, CallInfo{Owner: "a"})()
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	mustStillWait(t, b, "b while a's slot is reserved")
	// a asks for a third call: its turn is over, b goes first.
	// (Not laneAsync: b leaves the queue as a joins it, so its depth stays 1.)
	a3 := acquireAsync(t, schedCtx(t), func(ctx context.Context) (func(), error) {
		return l.Acquire(ctx, CallInfo{Owner: "a"})
	})
	relB := mustGrant(t, b, "waiter when the spent owner asks again")
	mustStillWait(t, a3, "the spent owner behind the waiter")
	// b now holds the lease, with a quantum of its own; ending b's run is the
	// quickest way to pass the slot back.
	relB()
	l.Done("b")
	mustGrant(t, a3, "the owner once the waiter's run is done")
	if st := l.Stats(); st.Preemptions < 1 {
		t.Fatalf("Preemptions = %d, want at least 1", st.Preemptions)
	}
}

func TestLane_QuantumSpentOwnerKeepsSlotWhenNobodyWaits(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, QuantumCalls: 1, AffinityHold: time.Minute})
	for i := 0; i < 5; i++ {
		laneNow(t, l, CallInfo{Owner: "a"})()
	}
	if st := l.Stats(); st.Reserved != 1 || st.Preemptions != 0 {
		t.Fatalf("stats = %+v, want the reservation kept and no preemption", st)
	}
}

func TestLane_UnlimitedQuantumKeepsLeaseWhileOthersWait(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, QuantumCalls: 0, AffinityHold: time.Minute})
	laneNow(t, l, CallInfo{Owner: "a"})()
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	for i := 0; i < 5; i++ {
		laneNow(t, l, CallInfo{Owner: "a"})()
	}
	mustStillWait(t, b, "b behind an owner with no quantum")
	if st := l.Stats(); st.Preemptions != 0 {
		t.Fatalf("Preemptions = %d, want 0", st.Preemptions)
	}
}

func TestLane_AnonymousCallHoldsNoReservation(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	laneNow(t, l, CallInfo{Owner: ""})()
	if st := l.Stats(); st.Reserved != 0 {
		t.Fatalf("Reserved = %d, want 0 for a call with no owner", st.Reserved)
	}
	laneNow(t, l, CallInfo{Owner: "b"})
}

func TestLane_OwnerWithTwoCallsInFlightHoldsOneReservation(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 3, AffinityHold: time.Minute})
	r1 := laneNow(t, l, CallInfo{Owner: "a"})
	r2 := laneNow(t, l, CallInfo{Owner: "a"})
	if st := l.Stats(); st.Active != 2 {
		t.Fatalf("Active = %d, want both of a's calls in flight", st.Active)
	}
	r1()
	r2()
	if st := l.Stats(); st.Active != 0 || st.Reserved != 1 {
		t.Fatalf("stats = %+v, want Active 0 and exactly one reservation", st)
	}
}

func TestLane_OwnerQueuedSecondCallIsNotHeldUpByOwnReservation(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	r1 := laneNow(t, l, CallInfo{Owner: "a"})
	second := laneAsync(t, l, CallInfo{Owner: "a"})
	// Reserving the slot for a's next call would park a's queued call behind
	// a's own idle reservation for the whole hold.
	r1()
	mustGrant(t, second, "the owner's queued second call")
}

func TestLane_DoneFreesIdleReservationForWaiter(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	laneNow(t, l, CallInfo{Owner: "a"})()
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	mustStillWait(t, b, "b during a's hold")
	l.Done("a")
	mustGrant(t, b, "waiter after Done")
}

func TestLane_DoneDuringCallLeavesNoReservation(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	rel := laneNow(t, l, CallInfo{Owner: "a"})
	l.Done("a")
	if st := l.Stats(); st.Active != 1 {
		t.Fatalf("Active = %d, want Done to leave the call in flight", st.Active)
	}
	rel()
	if st := l.Stats(); st.Active != 0 || st.Reserved != 0 {
		t.Fatalf("stats = %+v, want an empty lane after a done run's call ends", st)
	}
}

func TestLane_ForegroundServedBeforeBackground(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	hold := laneNow(t, l, CallInfo{})
	bg := laneAsync(t, l, CallInfo{Owner: "bg", Background: true})
	fg := laneAsync(t, l, CallInfo{Owner: "fg"})
	if st := l.Stats(); st.Queued != 2 || st.QueuedBackground != 1 {
		t.Fatalf("stats = %+v, want Queued 2 QueuedBackground 1", st)
	}
	hold()
	relFg := mustGrant(t, fg, "foreground, queued after background")
	mustStillWait(t, bg, "background while foreground runs")
	relFg()
	mustGrant(t, bg, "background once foreground is served")
}

func TestLane_ZeroBackgroundShareIsStrictPriority(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	got := grantOrder(t, l, []CallInfo{
		{Owner: "bg1", Background: true}, {Owner: "bg2", Background: true},
		{Owner: "fg1"}, {Owner: "fg2"}, {Owner: "fg3"}, {Owner: "fg4"},
	})
	want := "[fg1 fg2 fg3 fg4 bg1 bg2]"
	if fmt.Sprint(got) != want {
		t.Fatalf("grant order = %v, want %s", got, want)
	}
}

func TestLane_BackgroundShareGrantsBackgroundWhileBothWait(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, BackgroundShare: 0.25})
	var calls []CallInfo
	for i := 1; i <= 9; i++ {
		calls = append(calls, CallInfo{Owner: fmt.Sprintf("fg%d", i)})
	}
	calls = append(calls, CallInfo{Owner: "bg1", Background: true}, CallInfo{Owner: "bg2", Background: true})
	got := grantOrder(t, l, calls)
	// One grant in four goes to background for as long as both classes wait.
	want := "[fg1 fg2 fg3 bg1 fg4 fg5 fg6 bg2 fg7 fg8 fg9]"
	if fmt.Sprint(got) != want {
		t.Fatalf("grant order = %v, want %s", got, want)
	}
}

func TestLane_RoundRobinAcrossUsersFIFOWithinUser(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	// The same user id in two tenants is two buckets.
	got := grantOrder(t, l, []CallInfo{
		{Owner: "a1", TenantID: "t1", UserID: "u"},
		{Owner: "a2", TenantID: "t1", UserID: "u"},
		{Owner: "a3", TenantID: "t1", UserID: "u"},
		{Owner: "b1", TenantID: "t2", UserID: "u"},
		{Owner: "c1", TenantID: "t1", UserID: "v"},
	})
	want := "[a1 b1 c1 a2 a3]"
	if fmt.Sprint(got) != want {
		t.Fatalf("grant order = %v, want %s", got, want)
	}
}

func TestLane_ReleaseIsIdempotent(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 2})
	r1 := laneNow(t, l, CallInfo{Owner: "a"})
	laneNow(t, l, CallInfo{Owner: "b"})
	r1()
	r1()
	if st := l.Stats(); st.Active != 1 {
		t.Fatalf("Active = %d after a double release, want 1", st.Active)
	}
	laneNow(t, l, CallInfo{Owner: "c"})
	d := laneAsync(t, l, CallInfo{Owner: "d"})
	mustStillWait(t, d, "a call beyond the cap after a double release")
}

func TestLane_ReconfigureGrowAdmitsWaiters(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	laneNow(t, l, CallInfo{Owner: "a"})
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	c := laneAsync(t, l, CallInfo{Owner: "c"})
	l.Reconfigure(LaneConfig{MaxConcurrent: 3})
	mustGrant(t, b, "first waiter after the cap grew")
	mustGrant(t, c, "second waiter after the cap grew")
	if st := l.Stats(); st.Active != 3 || st.MaxConcurrent != 3 {
		t.Fatalf("stats = %+v, want Active 3 MaxConcurrent 3", st)
	}
}

func TestLane_ReconfigureShrinkLetsCallsInFlightFinish(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 2})
	r1 := laneNow(t, l, CallInfo{Owner: "a"})
	r2 := laneNow(t, l, CallInfo{Owner: "b"})
	l.Reconfigure(LaneConfig{MaxConcurrent: 1})
	if st := l.Stats(); st.Active != 2 {
		t.Fatalf("Active = %d, want both calls still in flight after the shrink", st.Active)
	}
	c := laneAsync(t, l, CallInfo{Owner: "c"})
	r1()
	mustStillWait(t, c, "a waiter while the excess call drains")
	r2()
	mustGrant(t, c, "a waiter once the lane is under its new cap")
}

func TestLane_ReconfigureShrinkGivesUpIdleReservations(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 2, AffinityHold: time.Minute})
	ra := laneNow(t, l, CallInfo{Owner: "a"})
	rb := laneNow(t, l, CallInfo{Owner: "b"})
	ra()
	rb()
	if st := l.Stats(); st.Reserved != 2 {
		t.Fatalf("Reserved = %d, want 2 before the shrink", st.Reserved)
	}
	l.Reconfigure(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	if st := l.Stats(); st.Reserved != 1 {
		t.Fatalf("Reserved = %d, want 1 after shrinking to one slot", st.Reserved)
	}
}

func TestLane_ReconfigureToNoHoldEndsReservations(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	laneNow(t, l, CallInfo{Owner: "a"})()
	b := laneAsync(t, l, CallInfo{Owner: "b"})
	mustStillWait(t, b, "b during a's hold")
	l.Reconfigure(LaneConfig{MaxConcurrent: 1})
	relB := mustGrant(t, b, "waiter once the lane has no hold")
	relB()
	if st := l.Stats(); st.Reserved != 0 {
		t.Fatalf("Reserved = %d, want later releases to reserve nothing", st.Reserved)
	}
}

func TestLane_ReconfigureAppliesHoldToLaterReleases(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	rel := laneNow(t, l, CallInfo{Owner: "a"})
	l.Reconfigure(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	// a's call was granted with no hold configured, so it carries no lease.
	rel()
	if st := l.Stats(); st.Reserved != 0 {
		t.Fatalf("Reserved = %d, want none for a call granted before the hold", st.Reserved)
	}
	laneNow(t, l, CallInfo{Owner: "a"})()
	if st := l.Stats(); st.Reserved != 1 {
		t.Fatalf("Reserved = %d, want the next call to hold its slot", st.Reserved)
	}
}

func TestLane_CancelledWaiterLeavesQueue(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1})
	a := laneNow(t, l, CallInfo{Owner: "a"})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := l.Acquire(ctx, CallInfo{Owner: "b"}); errc <- err }()
	eventually(t, "b queued", func() bool { return l.Stats().Queued == 1 })
	c := laneAsync(t, l, CallInfo{Owner: "c"})
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	a()
	// The slot goes to c, not to the waiter that left ahead of it.
	mustGrant(t, c, "the waiter behind a cancelled one")
	if st := l.Stats(); st.Active != 1 || st.Queued != 0 {
		t.Fatalf("stats = %+v, want Active 1 Queued 0", st)
	}
}

func TestLane_CancelRacingHandOffLeaksNoSlot(t *testing.T) {
	l := NewLane(LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute})
	var won uint64
	for i := 0; i < 500; i++ {
		hold := laneNow(t, l, CallInfo{})
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			rel func()
			err error
		}
		resc := make(chan result, 1)
		go func() {
			rel, err := l.Acquire(ctx, CallInfo{Owner: "w"})
			resc <- result{rel, err}
		}()
		eventually(t, "waiter queued", func() bool { return l.Stats().Queued == 1 })
		// Release (the hand-off) and cancel race each other.
		go hold()
		cancel()
		res := <-resc
		if res.err == nil {
			won++
			res.rel()
			l.Done("w")
		}
		// Whichever side won, the slot must be free again: neither in flight
		// nor reserved for a waiter that gave up.
		eventually(t, "lane empty", func() bool {
			st := l.Stats()
			return st.Active == 0 && st.Reserved == 0 && st.Queued == 0
		})
	}
	// Every iteration grants the holder; the waiter only when it won.
	if st := l.Stats(); st.Grants != 500+won {
		t.Fatalf("Grants = %d, want %d (a slot handed to a cancelled waiter is not a grant)", st.Grants, 500+won)
	}
}

// laneStorm runs many runs' worth of calls with random cancels through l and
// checks the cap was never exceeded and nothing is left behind.
func laneStorm(t *testing.T, cfg LaneConfig) {
	t.Helper()
	l := NewLane(cfg)
	const runs, calls = 24, 40
	var inFlight, peak atomic.Int32
	var granted atomic.Uint64
	var wg sync.WaitGroup
	for r := 0; r < runs; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(r)))
			c := CallInfo{
				TenantID:   fmt.Sprintf("t%d", r%2),
				UserID:     fmt.Sprintf("u%d", r%5),
				Background: r%4 == 0,
			}
			if r%6 != 0 {
				c.Owner = fmt.Sprintf("run%d", r)
			}
			defer l.Done(c.Owner)
			for i := 0; i < calls; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				if rng.Intn(3) == 0 {
					go func(d time.Duration) { time.Sleep(d); cancel() }(time.Duration(rng.Intn(1500)) * time.Microsecond)
				}
				rel, err := l.Acquire(ctx, c)
				if err != nil {
					cancel()
					continue
				}
				granted.Add(1)
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(time.Duration(rng.Intn(400)) * time.Microsecond)
				inFlight.Add(-1)
				rel()
				if rng.Intn(8) == 0 {
					rel() // a double release must not free a second slot
				}
				cancel()
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("storm did not finish: a slot was lost (stats %+v)", l.Stats())
	}
	if p := int(peak.Load()); p > cfg.MaxConcurrent {
		t.Fatalf("peak calls in flight = %d, want <= %d", p, cfg.MaxConcurrent)
	}
	st := l.Stats()
	if st.Active != 0 || st.Queued != 0 || st.QueuedBackground != 0 || st.Reserved != 0 {
		t.Fatalf("stats after the storm = %+v, want an empty lane", st)
	}
	if st.Grants != granted.Load() {
		t.Fatalf("Grants = %d, want %d (one per successful Acquire)", st.Grants, granted.Load())
	}
}

func TestLane_StormAsPlainQueueNeverExceedsMaxConcurrent(t *testing.T) {
	laneStorm(t, LaneConfig{MaxConcurrent: 3})
}

func TestLane_StormWithLeasesNeverExceedsMaxConcurrent(t *testing.T) {
	laneStorm(t, LaneConfig{MaxConcurrent: 3, AffinityHold: 300 * time.Microsecond})
}

func TestLane_StormWithQuantumAndBackgroundShareNeverExceedsMaxConcurrent(t *testing.T) {
	laneStorm(t, LaneConfig{MaxConcurrent: 2, QuantumCalls: 2, AffinityHold: 2 * time.Millisecond, BackgroundShare: 0.3})
}

func TestLane_StormUnderReconfigureLeavesNothingBehind(t *testing.T) {
	// The cap moves under the storm, so only the end state is asserted against
	// the largest cap used.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	l := NewLane(LaneConfig{MaxConcurrent: 2, AffinityHold: time.Millisecond, QuantumCalls: 3})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			l.Reconfigure(LaneConfig{MaxConcurrent: 1 + i%4, AffinityHold: time.Duration(i%3) * time.Millisecond, QuantumCalls: i % 3})
			time.Sleep(300 * time.Microsecond)
		}
	}()
	var inFlight, peak atomic.Int32
	var calls sync.WaitGroup
	for r := 0; r < 16; r++ {
		calls.Add(1)
		go func() {
			defer calls.Done()
			owner := fmt.Sprintf("run%d", r)
			defer l.Done(owner)
			for i := 0; i < 40; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+(r+i)%4)*time.Millisecond)
				rel, err := l.Acquire(ctx, CallInfo{Owner: owner, UserID: owner})
				if err == nil {
					n := inFlight.Add(1)
					for {
						p := peak.Load()
						if n <= p || peak.CompareAndSwap(p, n) {
							break
						}
					}
					time.Sleep(200 * time.Microsecond)
					inFlight.Add(-1)
					rel()
				}
				cancel()
			}
		}()
	}
	calls.Wait()
	close(stop)
	wg.Wait()
	if p := peak.Load(); p > 4 {
		t.Fatalf("peak calls in flight = %d, want <= 4, the largest cap used", p)
	}
	l.Reconfigure(LaneConfig{MaxConcurrent: 1})
	if st := l.Stats(); st.Active != 0 || st.Queued != 0 || st.Reserved != 0 {
		t.Fatalf("stats after the storm = %+v, want an empty lane", st)
	}
}
