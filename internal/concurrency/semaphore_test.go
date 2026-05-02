package concurrency

import (
	"context"
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
	a, q := s.Stats()
	if a != 2 || q != 0 {
		t.Errorf("active=%d queued=%d", a, q)
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
	if a, q := s.Stats(); a != 1 || q != 1 {
		t.Errorf("expected 1 active 1 queued; got %d / %d", a, q)
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
	a, q := s.Stats()
	if a != 0 || q != 0 {
		t.Errorf("after %d racy iterations: active=%d queued=%d, want 0/0 (slot leak)", iterations, a, q)
	}
}
