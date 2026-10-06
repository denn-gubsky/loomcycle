package loop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// errWallLimit is the cancel cause the wall-limit watcher sets on the run's ctx.
var errWallLimit = errors.New("code_agent_wall_limit")

// wallRecheckFloor is the least time the watcher waits before re-reading the
// clock. It only matters while a runtime pause holds the run's lifetime still
// close to its limit; it may let a run overshoot its limit by this much.
const wallRecheckFloor = 100 * time.Millisecond

// Run drives one agent run to completion.
//
// A provider that bounds the whole run by time (Capabilities().
// UnboundedIterations — code-js) gets a RunClock on the run's ctx: its budget
// is spent from the clock's active time, which stops while the run waits. Every
// other run is stamped with no clock, so it neither inherits nor pauses its
// parent's.
//
// Because waits do not spend the budget, a run that only waits would never
// end; so such a run also has a lifetime limit (Capabilities().RunWallLimit),
// waits included. Past it, the run's ctx is cancelled — which stops whatever
// it is waiting on, its children included, rather than only its next turn —
// and the run fails code_agent_wall_limit.
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	if opts.Provider == nil {
		return runLoop(ctx, opts)
	}
	caps := opts.Provider.Capabilities()
	if !caps.UnboundedIterations {
		return runLoop(providers.WithRunClock(ctx, nil), opts)
	}
	clock := providers.NewRunClock(time.Now(), opts.RunClockCarry)
	ctx = providers.WithRunClock(ctx, clock)
	limit := caps.RunWallLimit
	if limit <= 0 {
		return runLoop(ctx, opts)
	}
	clock.SetWallLimit(limit)
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go enforceWallLimit(ctx, clock, limit, stop)

	res, err := runLoop(ctx, opts)
	// Reported even when the loop returned no error: the cancel can land while
	// a tool call is in flight, which then returns cancelled children or an
	// empty wait as ordinary results, and a quick final turn can finish on them
	// before it sees the cancel. A run past its limit did not complete its work.
	if errors.Is(context.Cause(ctx), errWallLimit) {
		err = fmt.Errorf("%w: run lived past its %s limit on total time, waits included "+
			"(LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS); what it was waiting on, and its sub-agents, were cancelled", errWallLimit, limit)
	}
	return res, err
}

// enforceWallLimit cancels the run with errWallLimit once its lifetime reaches
// limit, and returns when it does or when the run ends. It re-reads the clock
// rather than arming one deadline because a runtime pause, which does not
// count, can extend the lifetime while the run is under way.
func enforceWallLimit(ctx context.Context, clock *providers.RunClock, limit time.Duration, stop context.CancelCauseFunc) {
	for {
		left := limit - clock.State().Wall
		if left <= 0 {
			stop(errWallLimit)
			return
		}
		t := time.NewTimer(max(left, wallRecheckFloor))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
