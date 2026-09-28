package teamrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// A state's `timeout_ms` bounds the runs its handler starts.
//
// WHAT IT BOUNDS depends on the kind, because a Starter's runs are independent
// and every other kind's are one piece of work:
//
//   - agent / parallel / consolidator: ONE execution of the handler — every run
//     it starts (the agent, each fan-out member, the consolidator), measured
//     from when the handler starts. On expiry the runs still going are
//     cancelled and the state fails with a *TimeoutError, which ends the walk
//     as any other handler failure does.
//   - starter: EACH spawned run, from when it is dispatched. A run that runs
//     out is cancelled and publishes `status: timeout` to the sink — one
//     message per run, as always — and does not count toward the wave's wait.
//
// Time a run spends HELD for a verdict (a review hold, or an agent_stop hook's
// hold) is not counted: a person deciding must not race the clock. A Starter
// run's own clock simply stops while it is held. A handler-wide clock stops
// while EVERY run in flight is held: a run still working keeps it going, and
// if that run spends the budget the whole handler ends, held siblings with it
// — the state as a whole ran out of working time.

// TimeoutError is why a state (or one Starter run) was stopped by timeout_ms.
type TimeoutError struct {
	State     string
	TimeoutMS int
	// Agent is set for one Starter run; empty for a whole handler.
	Agent string
}

func (e *TimeoutError) Error() string {
	if e.Agent != "" {
		return fmt.Sprintf("agent %q in state %q timed out: timeout_ms=%d elapsed (time held for review not counted)",
			e.Agent, e.State, e.TimeoutMS)
	}
	return fmt.Sprintf("state %q timed out: timeout_ms=%d elapsed (time held for review not counted)",
		e.State, e.TimeoutMS)
}

// boundsWholeHandler reports whether timeout_ms bounds the handler as one
// unit. A Starter bounds each run instead; the kinds that start no runs have
// nothing to bound.
func boundsWholeHandler(h teamgraph.Handler) bool {
	if h.TimeoutMS <= 0 {
		return false
	}
	switch h.Kind {
	case teamgraph.HandlerAgent, teamgraph.HandlerParallel, teamgraph.HandlerConsolidator:
		return true
	}
	return false
}

type holdObserverKey struct{}

// WithHoldObserver attaches, for the SpawnFunc, what a member run reports its
// holds to: held(true) when it parks for a verdict, held(false) when the hold
// ends. nil CLEARS an observer inherited from an enclosing walk, so a walk
// nested inside a member can never stop its parent's clock.
func WithHoldObserver(ctx context.Context, held func(bool)) context.Context {
	return context.WithValue(ctx, holdObserverKey{}, held)
}

// HoldObserver returns the member's hold observer, or nil.
func HoldObserver(ctx context.Context) func(bool) {
	f, _ := ctx.Value(holdObserverKey{}).(func(bool))
	return f
}

type clockKey struct{}

// spawnMember starts one member run, reporting its holds to the clock bounding
// it (if any). Every run a handler starts goes through here.
//
// The clock is taken OFF the ctx the run receives: a walk nested inside the
// member would otherwise find this clock and count its own runs on it. The
// nested walk is still bounded — cancelling this ctx reaches it — it just
// cannot stop or start a clock it does not own.
func (r *agentRunner) spawnMember(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error) {
	clk, _ := ctx.Value(clockKey{}).(*heldClock)
	if clk == nil {
		return r.spawn(WithHoldObserver(ctx, nil), agent, p, defID)
	}
	m := clk.join()
	defer m.leave()
	ctx = context.WithValue(ctx, clockKey{}, (*heldClock)(nil))
	return r.spawn(WithHoldObserver(ctx, m.setHeld), agent, p, defID)
}

// heldClock is a deadline that does not run while every run it bounds is held.
// Its ctx is cancelled, with the clock's cause, when the budget is spent.
type heldClock struct {
	mu        sync.Mutex
	remaining time.Duration
	since     time.Time   // start of the current running stretch
	timer     *time.Timer // nil while stopped
	gen       int         // invalidates a timer that fired as it was stopped
	live      int         // runs in flight
	held      int         // of those, held for a verdict
	expired   bool
	finished  bool
	cause     error
	cancel    context.CancelCauseFunc
}

// startClock returns a ctx cancelled with cause once d of unheld time passes.
// Call finish when the bounded work is over.
func startClock(ctx context.Context, d time.Duration, cause error) (context.Context, *heldClock) {
	cctx, cancel := context.WithCancelCause(ctx)
	c := &heldClock{remaining: d, cause: cause, cancel: cancel}
	c.mu.Lock()
	c.run()
	c.mu.Unlock()
	return context.WithValue(cctx, clockKey{}, c), c
}

// run starts the timer on what is left. Caller holds mu.
func (c *heldClock) run() {
	c.gen++
	gen := c.gen
	c.since = time.Now()
	c.timer = time.AfterFunc(c.remaining, func() { c.fire(gen) })
}

// stop banks the elapsed stretch and stops the timer. Caller holds mu.
func (c *heldClock) stop() {
	if c.timer == nil {
		return
	}
	c.timer.Stop()
	c.timer = nil
	c.gen++ // a fire already in flight is now stale
	if c.remaining -= time.Since(c.since); c.remaining < 0 {
		c.remaining = 0
	}
}

func (c *heldClock) fire(gen int) {
	c.mu.Lock()
	if gen != c.gen || c.finished {
		c.mu.Unlock()
		return
	}
	c.timer = nil
	c.expired = true
	c.mu.Unlock()
	c.cancel(c.cause)
}

// settle stops or restarts the clock for the current live/held counts.
// Caller holds mu.
func (c *heldClock) settle() {
	if c.expired || c.finished {
		return
	}
	allHeld := c.live > 0 && c.held == c.live
	switch {
	case allHeld && c.timer != nil:
		c.stop()
	case !allHeld && c.timer == nil:
		c.run()
	}
}

// timedOut reports whether the clock ran out.
func (c *heldClock) timedOut() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expired
}

// finish ends the clock and releases its ctx.
func (c *heldClock) finish() {
	c.mu.Lock()
	c.finished = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	c.cancel(context.Canceled)
}

// clockMember is one run on a clock.
type clockMember struct {
	c    *heldClock
	held bool
	done bool
}

func (c *heldClock) join() *clockMember {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live++
	c.settle()
	return &clockMember{c: c}
}

// setHeld records that the run's hold began or ended. Idempotent, and ignored
// once the run has left the clock.
func (m *clockMember) setHeld(held bool) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.done || m.held == held {
		return
	}
	m.held = held
	if held {
		c.held++
	} else {
		c.held--
	}
	c.settle()
}

func (m *clockMember) leave() {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.done {
		return
	}
	m.done = true
	if m.held {
		c.held--
	}
	c.live--
	c.settle()
}
