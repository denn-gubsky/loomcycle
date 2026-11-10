package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A trace flush stuck until the deadline does not keep the metric flush
// from running: both start at once, and both errors come back.
func TestConcurrentShutdown_ASlowFlushDoesNotHoldBackTheOther(t *testing.T) {
	metricsRan := make(chan struct{})
	slow := func(ctx context.Context) error {
		select {
		case <-metricsRan:
			return errors.New("trace flush gave up")
		case <-ctx.Done():
			return errors.New("the metric flush never ran while the trace flush waited")
		}
	}
	fast := func(context.Context) error {
		close(metricsRan)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := concurrentShutdown(slow, fast)(ctx)
	if err == nil || err.Error() != "trace flush gave up" {
		t.Errorf("err = %v, want only the trace flush's own error", err)
	}
}
