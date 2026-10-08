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

// wallCancelGrace is how long the watcher gives RunOptions.OnWallLimit to end
// the run before it cancels the run's ctx itself. The limit must hold even
// when that cancel does not land — a run its registry no longer knows.
const wallCancelGrace = 5 * time.Second

// Run drives one agent run to completion.
//
// A provider that bounds the whole run by time (Capabilities().
// UnboundedIterations — code-js) gets a RunClock on the run's ctx: its budget
// is spent from the clock's active time, which stops while the run waits. A
// run with a lifetime limit of its own (RunOptions.MaxWallSeconds) gets one
// too, whatever its provider, because the clock is what knows how long the run
// has lived with the runtime's pauses left out. Every other run is stamped
// with no clock, so it neither inherits nor pauses its parent's.
//
// Because waits do not spend the budget, a run that only waits would never
// end; so a code-js run also has a lifetime limit (Capabilities().
// RunWallLimit), waits included. Past it, the run's ctx is cancelled — which
// stops whatever it is waiting on, its children included, rather than only its
// next turn — and the run fails code_agent_wall_limit.
//
// The run's own limit is enforced by the same watcher when it is the tighter
// of the two. It ends the run differently: OnWallLimit cancels it, so the run
// is recorded as cancelled with the limit as the reason, not as failed.
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	if opts.Provider == nil {
		return runLoop(ctx, opts)
	}
	caps := opts.Provider.Capabilities()
	own := time.Duration(opts.MaxWallSeconds) * time.Second
	if !caps.UnboundedIterations && own <= 0 {
		return runLoop(providers.WithRunClock(ctx, nil), opts)
	}
	clock := providers.NewRunClock(time.Now(), opts.RunClockCarry)
	ctx = providers.WithRunClock(ctx, clock)
	var limit time.Duration
	if caps.UnboundedIterations {
		limit = caps.RunWallLimit
	}
	ownLimit := own > 0 && (limit <= 0 || own <= limit)
	if ownLimit {
		limit = own
	}
	if limit <= 0 {
		return runLoop(ctx, opts)
	}
	clock.SetWallLimit(limit)
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	onLimit := func() { stop(errWallLimit) }
	if ownLimit && opts.OnWallLimit != nil {
		grace := wallCancelGrace
		if opts.wallCancelGrace > 0 {
			grace = opts.wallCancelGrace
		}
		onLimit = func() {
			opts.OnWallLimit()
			select {
			case <-ctx.Done():
			case <-time.After(grace):
				stop(errWallLimit)
			}
		}
	}
	go enforceWallLimit(ctx, clock, limit, onLimit)

	res, err := runLoop(ctx, opts)
	// Reported even when the loop returned no error: the cancel can land while
	// a tool call is in flight, which then returns cancelled children or an
	// empty wait as ordinary results, and a quick final turn can finish on them
	// before it sees the cancel. A run past its limit did not complete its work.
	if errors.Is(context.Cause(ctx), errWallLimit) {
		source := "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS"
		if ownLimit {
			source = "max_wall_seconds"
		}
		err = fmt.Errorf("%w: run lived past its %s limit on total time, waits included "+
			"(%s); what it was waiting on, and its sub-agents, were cancelled", errWallLimit, limit, source)
	}
	return res, err
}

// enforceWallLimit calls onLimit once the run's lifetime reaches limit, and
// returns when it has or when the run ends. It re-reads the clock rather than
// arming one deadline because a runtime pause, which does not count, can
// extend the lifetime while the run is under way.
func enforceWallLimit(ctx context.Context, clock *providers.RunClock, limit time.Duration, onLimit func()) {
	for {
		left := limit - clock.State().Wall
		if left <= 0 {
			onLimit()
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
