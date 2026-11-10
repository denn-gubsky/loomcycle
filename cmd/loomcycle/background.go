package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// Restart delays for a background loop that panicked: doubled after each panic
// in a row, so a loop that panics on every start logs a stack a minute at most.
const (
	supervisedRestartMin = time.Second
	supervisedRestartMax = time.Minute
)

// goSupervised runs loop in a new goroutine until ctx ends, starting it again
// after a panic. The boot's sweepers and backplane subscribers loop in
// packages that recover nothing per tick; recovering the panic and returning
// would leave the replica with that loop silently stopped (a subscriber no
// longer hearing cancels, a sweeper no longer sweeping), so the loop is
// started again instead. A loop that returns on its own is not restarted.
func goSupervised(ctx context.Context, name string, loop func(context.Context)) {
	safego.Go(ctx, name, func() {
		superviseLoop(ctx, name, loop, supervisedRestartMin, supervisedRestartMax)
	})
}

func superviseLoop(ctx context.Context, name string, loop func(context.Context), minDelay, maxDelay time.Duration) {
	delay := minDelay
	for {
		started := time.Now()
		if !runLoopAttempt(ctx, name, loop) {
			return
		}
		// A loop that ran a long while before it panicked is not failing at
		// start: begin the backoff again.
		if time.Since(started) > maxDelay {
			delay = minDelay
		}
		slog.WarnContext(ctx, "background loop panicked; starting it again", "goroutine", name, "delay", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}

// runLoopAttempt runs loop once and reports whether it panicked. Each attempt
// has its own ctx, cancelled when it ends, so what the panicked attempt bound
// to it (a backplane subscription) is released before the next one starts.
func runLoopAttempt(ctx context.Context, name string, loop func(context.Context)) (panicked bool) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	panicked = true
	safego.Run(actx, name, func() {
		loop(actx)
		panicked = false
	})
	return panicked
}

// goAdvisoryGatedSweeper starts runAdvisoryGatedSweeper in a new goroutine.
func goAdvisoryGatedSweeper(
	ctx context.Context,
	interval time.Duration,
	lock *coord.AdvisoryLock,
	lockKey int64,
	name string,
	tick func(ctx context.Context),
) {
	safego.Go(ctx, "sweeper."+name, func() {
		runAdvisoryGatedSweeper(ctx, interval, lock, lockKey, name, tick)
	})
}
