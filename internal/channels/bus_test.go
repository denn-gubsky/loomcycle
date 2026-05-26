package channels

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBus_NotifyWakesWaiter pins the happy path: Wait blocks; Notify
// from another goroutine returns true.
func TestBus_NotifyWakesWaiter(t *testing.T) {
	t.Parallel()
	b := NewBus()
	var got int32
	done := make(chan struct{})
	go func() {
		if b.Wait(context.Background(), "ch", 2*time.Second) {
			atomic.StoreInt32(&got, 1)
		}
		close(done)
	}()
	// Give the waiter a beat to register.
	time.Sleep(20 * time.Millisecond)
	b.Notify("ch")
	<-done
	if atomic.LoadInt32(&got) != 1 {
		t.Fatal("waiter never woke")
	}
}

// TestBus_TimeoutReturnsFalse pins the no-notify path.
func TestBus_TimeoutReturnsFalse(t *testing.T) {
	t.Parallel()
	b := NewBus()
	if b.Wait(context.Background(), "ch", 50*time.Millisecond) {
		t.Error("Wait returned true; want false (no Notify)")
	}
}

// TestBus_ZeroTimeoutIsImmediate pins that wait_ms == 0 doesn't
// register a waiter — Subscribe with no long-poll budget hits this.
func TestBus_ZeroTimeoutIsImmediate(t *testing.T) {
	t.Parallel()
	b := NewBus()
	start := time.Now()
	if b.Wait(context.Background(), "ch", 0) {
		t.Error("Wait(timeout=0) returned true")
	}
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Errorf("Wait(0) took %v; want immediate", d)
	}
}

// TestBus_ContextCancellationReturnsEarly pins that an HTTP client
// disconnect (which cancels the ctx) doesn't leave the waiter
// blocking until timeout.
func TestBus_ContextCancellationReturnsEarly(t *testing.T) {
	t.Parallel()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- b.Wait(ctx, "ch", 10*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case got := <-done:
		if got {
			t.Error("Wait returned true after cancel; want false")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait didn't return after ctx cancel")
	}
}

// TestBus_FanOutToMultipleWaiters pins that broadcast-shape channels
// wake every subscriber on one Notify.
func TestBus_FanOutToMultipleWaiters(t *testing.T) {
	t.Parallel()
	b := NewBus()
	const N = 5
	var woke int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if b.Wait(context.Background(), "ch", 2*time.Second) {
				atomic.AddInt32(&woke, 1)
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	b.Notify("ch")
	wg.Wait()
	if got := atomic.LoadInt32(&woke); got != N {
		t.Errorf("woke %d of %d", got, N)
	}
}

// TestBus_ChannelIsolation pins that a Notify on "a" doesn't wake a
// waiter on "b".
func TestBus_ChannelIsolation(t *testing.T) {
	t.Parallel()
	b := NewBus()
	woke := make(chan bool, 1)
	go func() { woke <- b.Wait(context.Background(), "b", 200*time.Millisecond) }()
	time.Sleep(20 * time.Millisecond)
	b.Notify("a")
	if <-woke {
		t.Error("waiter on 'b' woke on Notify('a')")
	}
}

// TestBus_RaceDetectorCleanUnderConcurrentNotifyWait stress-runs
// Notify + Wait simultaneously to surface any lock-order bug under
// `go test -race`.
func TestBus_RaceDetectorCleanUnderConcurrentNotifyWait(t *testing.T) {
	t.Parallel()
	b := NewBus()
	const N = 20
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			b.Wait(context.Background(), "ch", 50*time.Millisecond)
		}()
		go func() {
			defer wg.Done()
			b.Notify("ch")
		}()
	}
	wg.Wait()
}

// TestBus_RegisterBuffersNotifyBeforeSelect is the key correctness
// invariant for the v0.12.x check-then-wait race fix. After calling
// Register, ANY subsequent Notify fires the waker — even one that
// arrives before the caller has started `select`-ing on it. The
// waker's channel buffer (cap 1) captures the signal.
//
// This property is what makes the "register-before-read" pattern
// race-free in Channel.execSubscribe: by the time the SQL read
// runs, the waker is already registered, so a Notify that fires
// concurrent with the read is queued in the buffer and the
// post-read `select` consumes it immediately.
//
// Without this property, a Notify between Register and select
// would be lost — defeating the purpose of pre-registration.
func TestBus_RegisterBuffersNotifyBeforeSelect(t *testing.T) {
	t.Parallel()
	b := NewBus()

	waker := b.Register("ch")
	defer b.Unregister("ch", waker)

	// Fire Notify BEFORE we start selecting. The waker's buffer
	// (cap 1) must capture it so the next read finds the signal.
	b.Notify("ch")

	select {
	case <-waker:
		// ok — pre-fired notify was captured by the buffered waker
	default:
		t.Fatal("waker did not capture Notify that fired before select; the check-then-wait race fix relies on this")
	}
}

// TestBus_RegisterRaceFreePattern simulates the exact
// Channel.execSubscribe pattern under concurrent publish.
// Demonstrates that the race-free pattern captures every notify,
// regardless of whether the publish lands during the "read" or
// during the wait. The 1000-iteration loop with tight timing
// surfaces any regression in the buffered-waker semantics.
func TestBus_RegisterRaceFreePattern(t *testing.T) {
	t.Parallel()
	b := NewBus()

	const iterations = 1000
	var (
		received atomic.Int32
		missed   atomic.Int32
	)
	for i := 0; i < iterations; i++ {
		// Each iteration: fresh "publish state" via per-iter counter.
		var published atomic.Bool
		check := func() bool { return published.Load() }

		var wg sync.WaitGroup
		wg.Add(2)

		// Race-free subscriber: Register → check → select-on-waker.
		go func() {
			defer wg.Done()
			waker := b.Register("ch")
			defer b.Unregister("ch", waker)

			if check() {
				received.Add(1)
				return
			}
			select {
			case <-waker:
				if check() {
					received.Add(1)
					return
				}
				// Notify fired but check still empty: this can only
				// happen if the iteration's Notify was for a different
				// "publish" event, which can't happen in this test
				// because publisher only fires once per iteration.
				missed.Add(1)
			case <-time.After(200 * time.Millisecond):
				// Timeout: would mean Notify was lost — the bug.
				missed.Add(1)
			}
		}()

		// Publisher.
		go func() {
			defer wg.Done()
			// Tiny random delay so we land in different windows
			// relative to subscriber across iterations: some
			// publishes happen before Register, some after, some
			// concurrent with check.
			time.Sleep(time.Duration(i%23) * time.Microsecond)
			published.Store(true)
			b.Notify("ch")
		}()

		wg.Wait()
	}

	// Hard assertion: zero misses across 1000 iterations.
	if missed.Load() > 0 {
		t.Errorf("race-free pattern: %d/%d notifies lost (want 0); received=%d",
			missed.Load(), iterations, received.Load())
	}
}

// TestBus_UnregisterIdempotent — Unregister must be safe to call
// even after Notify already drained the waker. Defer pattern relies
// on this.
func TestBus_UnregisterIdempotent(t *testing.T) {
	t.Parallel()
	b := NewBus()
	waker := b.Register("ch")
	b.Notify("ch") // drains the waker slice
	<-waker        // confirm waker fired
	// Now Unregister: must be a no-op (waker already gone).
	b.Unregister("ch", waker)
	// And calling Unregister twice is safe.
	b.Unregister("ch", waker)
}

// TestBus_RegisterFiresOnNotify — minimal happy path: Register then
// receive on the returned channel after Notify.
func TestBus_RegisterFiresOnNotify(t *testing.T) {
	t.Parallel()
	b := NewBus()
	waker := b.Register("ch")
	defer b.Unregister("ch", waker)

	go func() {
		time.Sleep(10 * time.Millisecond)
		b.Notify("ch")
	}()

	select {
	case <-waker:
		// ok
	case <-time.After(time.Second):
		t.Fatal("waker never fired after Notify")
	}
}
