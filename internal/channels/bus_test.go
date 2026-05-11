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
