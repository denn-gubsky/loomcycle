package providers

import (
	"context"
	"sync"
	"time"
)

// RunClock measures a run's ACTIVE time: the wall time since it started minus
// the time it spent waiting — on a sub-agent, a team walk, a channel, a
// person's answer. A provider that bounds a whole run by a time budget (the
// synthetic code-js provider, Capabilities().UnboundedIterations) spends its
// budget from this clock, so an orchestrator that blocks on its children is
// parked, not burning its budget, while they work.
//
// A wait site brackets ONLY its blocking part with BeginWait(ctx) — never the
// validation or admission before it. Waits may overlap (a team walk asking a
// question inside its own wait, two tool calls blocking at once): the clock
// counts wall time during which AT LEAST ONE wait is open (and nothing works
// beside it, below), so overlapping waits are counted once.
//
// A wait only stops the clock while nothing else of the run is working. The
// loop runs a turn's tool calls side by side and marks each one as work
// (BeginWork); a call's own wait suspends that call's work for as long as it
// blocks. So time is waited only while at least one wait is open AND no call
// is working outside a wait: a wait beside a working call (a channel await
// next to a Bash command) does not let the working call run free. The model's
// turn is not marked: it never overlaps a tool call, and with no wait open its
// time is active anyway.
//
// Because waits do not count, the budget alone cannot bound a run that does
// little but wait. The clock therefore also measures the run's WALL time — its
// whole lifetime, waits included — against which the loop enforces an
// absolute limit. Only a runtime pause (BeginPause), which the operator
// declared, is left out of it.
//
// The loop creates one per run for a provider that needs it and stamps it on
// the run's ctx; every other run carries none, and BeginWait is then a no-op.
// All methods are safe on a nil *RunClock.
type RunClock struct {
	now func() time.Time // time.Now outside tests

	mu     sync.Mutex
	start  time.Time
	prior  RunClockState // carried over from before a resume
	open   int           // waits in progress
	work   int           // tool calls working outside a wait
	mark   time.Time     // when open or work last changed
	waited time.Duration // waited time since start, up to mark

	paused   int           // runtime pauses in progress (each also a wait)
	pausedAt time.Time     // when paused last went 0 → 1
	pausedT  time.Duration // closed pause time since start

	budget    time.Duration // the run's total budget, as its provider resolved it
	wallLimit time.Duration // the run's lifetime limit, as the loop enforces it
}

// RunClockState is a run's time so far: Active counts against its budget,
// Waited does not, and Wall — its lifetime, waits included, runtime pauses
// excluded — counts against its wall limit. It is what a paused run persists
// so that a resumed run neither loses nor regains either.
type RunClockState struct {
	Active time.Duration
	Waited time.Duration
	Wall   time.Duration
}

// NewRunClock starts a clock at start, adding prior — the state a resumed run
// had recorded before it paused (zero for a fresh run).
func NewRunClock(start time.Time, prior RunClockState) *RunClock {
	return &RunClock{now: time.Now, start: start, prior: prior}
}

// waiting reports whether the run is waiting right now: a wait is open and no
// call is working outside one. Called with mu held.
func (c *RunClock) waiting() bool { return c.open > 0 && c.work == 0 }

// settle closes the span since mark, as waited time if the run was waiting
// through it, before open or work changes. Called with mu held.
func (c *RunClock) settle() {
	now := c.now()
	if c.waiting() {
		c.waited += now.Sub(c.mark)
	}
	c.mark = now
}

// BeginWait marks the start of a wait and returns the func that ends it. The
// end func is idempotent, so `defer end()` beside an early explicit end() is
// safe. A wait made inside a call marked by BeginWork goes through the
// package-level BeginWait(ctx), which also suspends that call's work.
func (c *RunClock) BeginWait() (end func()) {
	return c.beginWait(nil)
}

// beginWait opens a wait on behalf of w's call (nil: of no call). The call's
// work is suspended while at least one of its waits is open.
func (c *RunClock) beginWait(w *workMark) (end func()) {
	if c == nil {
		return func() {}
	}
	c.mu.Lock()
	c.settle()
	c.open++
	if w != nil {
		if w.waits == 0 && !w.ended {
			c.work--
		}
		w.waits++
	}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.settle()
			c.open--
			if w != nil {
				w.waits--
				if w.waits == 0 && !w.ended {
					c.work++
				}
			}
			c.mu.Unlock()
		})
	}
}

// workMark is one call marked by BeginWork. Its fields are guarded by the
// clock's mu.
type workMark struct {
	c     *RunClock
	waits int  // the call's own waits in progress; its work is suspended while > 0
	ended bool // the call returned
}

// beginWork marks the start of a call's work and returns its mark and the func
// that ends it (idempotent).
func (c *RunClock) beginWork() (*workMark, func()) {
	w := &workMark{c: c}
	c.mu.Lock()
	c.settle()
	c.work++
	c.mu.Unlock()
	var once sync.Once
	return w, func() {
		once.Do(func() {
			c.mu.Lock()
			c.settle()
			if w.waits == 0 {
				c.work--
			}
			// A wait the call left open (in a goroutine that outlived it) no
			// longer hands work back when it ends.
			w.ended = true
			c.mu.Unlock()
		})
	}
}

// BeginPause marks the start of a runtime pause — the operator stopped the
// runtime, and the run is parked — and returns the func that ends it. A pause
// is a wait (it does not spend the budget) and, unlike any other wait, does
// not count towards the wall limit either. Idempotent like BeginWait's.
func (c *RunClock) BeginPause() (end func()) {
	if c == nil {
		return func() {}
	}
	endWait := c.BeginWait()
	c.mu.Lock()
	if c.paused == 0 {
		c.pausedAt = c.now()
	}
	c.paused++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.paused--
			if c.paused == 0 {
				c.pausedT += c.now().Sub(c.pausedAt)
			}
			c.mu.Unlock()
			endWait()
		})
	}
}

// State reports the run's time as of now, including a wait or pause still in
// progress and whatever was carried over from before a resume.
func (c *RunClock) State() RunClockState {
	if c == nil {
		return RunClockState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	waited := c.waited
	if c.waiting() {
		waited += now.Sub(c.mark)
	}
	pausedT := c.pausedT
	if c.paused > 0 {
		pausedT += now.Sub(c.pausedAt)
	}
	elapsed := now.Sub(c.start)
	active := max(elapsed-waited, 0)
	wall := max(elapsed-pausedT, 0)
	return RunClockState{
		Active: c.prior.Active + active,
		Waited: c.prior.Waited + waited,
		Wall:   c.prior.Wall + wall,
	}
}

// SetBudget records the run's total budget. The provider that enforces the
// budget is the one that knows it (a per-run or per-agent override, else its
// own default), so it publishes it here for anything that reports on the run.
func (c *RunClock) SetBudget(d time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.budget = d
	c.mu.Unlock()
}

// Budget returns what SetBudget last recorded; 0 before the provider's first
// call.
func (c *RunClock) Budget() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget
}

// SetWallLimit records the run's lifetime limit, for anything that reports on
// the run. The loop sets it when it starts enforcing the limit.
func (c *RunClock) SetWallLimit(d time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.wallLimit = d
	c.mu.Unlock()
}

// WallLimit returns what SetWallLimit recorded; 0 when none is enforced.
func (c *RunClock) WallLimit() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wallLimit
}

type ctxKeyRunClock struct{}

type ctxKeyWorkMark struct{}

// WithRunClock returns ctx carrying c. A nil c is stamped too, deliberately: a
// sub-run's ctx descends from its parent's, and a run without a clock of its
// own must not spend — or pause — its parent's.
func WithRunClock(ctx context.Context, c *RunClock) context.Context {
	return context.WithValue(ctx, ctxKeyRunClock{}, c)
}

// RunClockFromContext returns the run's clock, or nil when it has none.
func RunClockFromContext(ctx context.Context) *RunClock {
	c, _ := ctx.Value(ctxKeyRunClock{}).(*RunClock)
	return c
}

// BeginWait marks the start of a wait on the clock of the run that ctx belongs
// to and returns the func that ends it — a no-op pair when the run has no
// clock. Bracket only the part that blocks:
//
//	endWait := providers.BeginWait(ctx)
//	wg.Wait()
//	endWait()
//
// When ctx is a tool call's (BeginWork), the call is taken to be blocked on
// this wait, so its work is suspended until the wait ends. A wait whose ctx has
// lost the call's mark is still a wait, but the call keeps counting as work:
// the run is charged, never let off.
func BeginWait(ctx context.Context) (end func()) {
	c := RunClockFromContext(ctx)
	w, _ := ctx.Value(ctxKeyWorkMark{}).(*workMark)
	if w == nil || w.c != c {
		// No call, or a call of another run (a sub-run's ctx descends from
		// its parent's tool call).
		w = nil
	}
	return c.beginWait(w)
}

// BeginWork marks one tool call of the run that ctx belongs to as working, and
// returns the ctx to run the call under and the func that ends it (idempotent)
// — ctx itself and a no-op when the run has no clock. While the call works, the
// run's other waits do not stop its clock; a wait the call itself makes, under
// the returned ctx, does (see BeginWait).
func BeginWork(ctx context.Context) (context.Context, func()) {
	c := RunClockFromContext(ctx)
	if c == nil {
		return ctx, func() {}
	}
	w, end := c.beginWork()
	return context.WithValue(ctx, ctxKeyWorkMark{}, w), end
}

// BeginPause is BeginWait for a runtime pause (see RunClock.BeginPause).
func BeginPause(ctx context.Context) (end func()) {
	return RunClockFromContext(ctx).BeginPause()
}
