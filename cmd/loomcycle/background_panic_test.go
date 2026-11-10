package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdvisoryGatedSweeper_KeepsTickingAfterAPanickingTick: one tick that
// panics must cost that tick only. Unrecovered, it ended the process; recovered
// around the whole loop, it would stop the sweeper for the rest of the
// process's life.
func TestAdvisoryGatedSweeper_KeepsTickingAfterAPanickingTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ticks atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAdvisoryGatedSweeper(ctx, time.Millisecond, nil, 0, "test", func(context.Context) {
			if ticks.Add(1) == 1 {
				panic("boom")
			}
		})
	}()
	deadline := time.After(5 * time.Second)
	for ticks.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("sweeper ticked %d times after a panicking tick, want 3", ticks.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestSuperviseLoop_StartsAPanickedLoopAgainWithAFreshContext: a loop from
// another package that panics is started again rather than left stopped, and
// the panicked attempt's ctx is cancelled first so what it subscribed to is
// released.
func TestSuperviseLoop_StartsAPanickedLoopAgainWithAFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var starts atomic.Int32
	var first context.Context
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseLoop(ctx, "test.supervised", func(actx context.Context) {
			switch starts.Add(1) {
			case 1:
				first = actx
				panic("boom")
			default:
				if first.Err() == nil {
					t.Error("the panicked attempt's ctx is still live in the next attempt")
				}
				<-actx.Done()
			}
		}, time.Millisecond, time.Millisecond)
	}()
	deadline := time.After(5 * time.Second)
	for starts.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("the loop was not started again after a panic")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestSuperviseLoop_DoesNotRestartALoopThatReturned: a loop that ends on its
// own (its subscribe failed, its work is done) keeps that outcome.
func TestSuperviseLoop_DoesNotRestartALoopThatReturned(t *testing.T) {
	var starts atomic.Int32
	superviseLoop(context.Background(), "test.supervised", func(context.Context) {
		starts.Add(1)
	}, time.Millisecond, time.Millisecond)
	if n := starts.Load(); n != 1 {
		t.Fatalf("loop started %d times, want 1", n)
	}
}

// TestConcurrentShutdown_ReportsAPanickingFlushAsAnError: a flush that panics
// is reported with the others' errors; the shutdown neither ends the process
// nor waits forever for an answer that never comes.
func TestConcurrentShutdown_ReportsAPanickingFlushAsAnError(t *testing.T) {
	ok := func(context.Context) error { return nil }
	boom := func(context.Context) error { panic("boom") }
	errc := make(chan error, 1)
	go func() { errc <- concurrentShutdown(ok, boom)(context.Background()) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("a panicking flush reported no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown waited on a flush that panicked")
	}
}
