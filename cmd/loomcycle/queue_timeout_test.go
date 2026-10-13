package main

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestGlobalQueueTimeout_NonPositiveStillRefusesAtOnce — concurrency.
// queue_timeout_ms <= 0 passes validation and has always meant "refuse at once
// when every run slot is busy". Semaphore now reads a timeout <= 0 as "no wait
// timeout", so the global semaphore built from such a config must still refuse
// with backpressure instead of queueing without end.
func TestGlobalQueueTimeout_NonPositiveStillRefusesAtOnce(t *testing.T) {
	for _, ms := range []int{0, -5} {
		sem := concurrency.New(1, 4, globalQueueTimeout(config.Concurrency{QueueTimeoutMS: ms}))
		rel, err := sem.Acquire(context.Background())
		if err != nil {
			t.Fatalf("queue_timeout_ms=%d: first acquire: %v", ms, err)
		}
		// Bounded so a regression (an unbounded wait) fails here, not by hanging.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		start := time.Now()
		_, err = sem.Acquire(ctx)
		cancel()
		if !concurrency.IsBackpressure(err) {
			t.Errorf("queue_timeout_ms=%d: second acquire err = %v, want backpressure", ms, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("queue_timeout_ms=%d: refusal took %s, want immediate", ms, d)
		}
		rel()
	}
	if got := globalQueueTimeout(config.Concurrency{QueueTimeoutMS: 1500}); got != 1500*time.Millisecond {
		t.Errorf("positive queue_timeout_ms: got %s, want 1.5s", got)
	}
}
