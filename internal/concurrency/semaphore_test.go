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
