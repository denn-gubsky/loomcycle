package providers

import (
	"context"
	"testing"
	"time"
)

// fakeNow is a hand-advanced clock for RunClock tests.
type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time          { return f.t }
func (f *fakeNow) advance(d time.Duration) { f.t = f.t.Add(d) }
func newTestClock(prior RunClockState) (*RunClock, *fakeNow) {
	f := &fakeNow{t: time.Unix(1_700_000_000, 0)}
	c := NewRunClock(f.t, prior)
	c.now = f.now
	return c, f
}

func TestRunClock_WaitTimeIsNotActiveTime(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	f.advance(1 * time.Second) // busy
	end := c.BeginWait()
	f.advance(10 * time.Second) // waiting
	if got := c.State(); got.Active != time.Second || got.Waited != 10*time.Second {
		t.Fatalf("mid-wait state = %+v, want active 1s waited 10s (an open wait already counts as waited)", got)
	}
	end()
	f.advance(2 * time.Second) // busy again
	got := c.State()
	if got.Active != 3*time.Second || got.Waited != 10*time.Second {
		t.Fatalf("state = %+v, want active 3s waited 10s", got)
	}
}

func TestRunClock_OverlappingWaitsCountOnce(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	endA := c.BeginWait()
	f.advance(2 * time.Second)
	endB := c.BeginWait() // overlaps A
	f.advance(3 * time.Second)
	endA()
	f.advance(4 * time.Second) // B still open
	endB()
	f.advance(1 * time.Second) // busy
	got := c.State()
	if got.Waited != 9*time.Second {
		t.Errorf("waited = %s, want 9s: wall time with at least one wait open, not the 2+3+3+4 sum", got.Waited)
	}
	if got.Active != time.Second {
		t.Errorf("active = %s, want 1s", got.Active)
	}
}

func TestRunClock_EndIsIdempotent(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	endA := c.BeginWait()
	endB := c.BeginWait()
	f.advance(time.Second)
	endA()
	endA() // a second end must not close B's wait
	f.advance(time.Second)
	if got := c.State(); got.Waited != 2*time.Second || got.Active != 0 {
		t.Fatalf("state = %+v, want waited 2s active 0 — a repeated end closed another wait", got)
	}
	endB()
}

func TestRunClock_CarriesPriorState(t *testing.T) {
	c, f := newTestClock(RunClockState{Active: 5 * time.Second, Waited: 7 * time.Second})
	f.advance(time.Second)
	end := c.BeginWait()
	f.advance(time.Second)
	end()
	if got := c.State(); got.Active != 6*time.Second || got.Waited != 8*time.Second {
		t.Fatalf("state = %+v, want active 6s waited 8s (prior plus this run's)", got)
	}
}

func TestBeginWait_NoClockIsANoOp(t *testing.T) {
	end := BeginWait(context.Background())
	end()
	var c *RunClock
	c.BeginWait()()
	if got := c.State(); got != (RunClockState{}) {
		t.Fatalf("nil clock state = %+v, want zero", got)
	}
	if c.Budget() != 0 {
		t.Fatal("nil clock budget must be 0")
	}
	c.SetBudget(time.Second) // must not panic
}

func TestWithRunClock_NilShadowsAnOuterClock(t *testing.T) {
	outer, f := newTestClock(RunClockState{})
	ctx := WithRunClock(context.Background(), outer)
	inner := WithRunClock(ctx, nil) // a sub-run with no clock of its own
	end := BeginWait(inner)
	f.advance(time.Second)
	end()
	if got := outer.State(); got.Waited != 0 {
		t.Fatalf("a sub-run's wait paused its parent's clock (waited %s)", got.Waited)
	}
}

// Wall time is the run's lifetime: waits count towards it, a runtime pause
// does not (it is a wait for the budget, too).
func TestRunClock_WallCountsWaitsButNotRuntimePauses(t *testing.T) {
	c, f := newTestClock(RunClockState{Active: time.Second, Waited: 2 * time.Second, Wall: 3 * time.Second})
	f.advance(time.Second) // busy
	endWait := c.BeginWait()
	f.advance(4 * time.Second) // waiting
	endWait()
	endPause := c.BeginPause()
	f.advance(100 * time.Second) // the runtime is paused
	if got := c.State(); got.Wall != 8*time.Second {
		t.Fatalf("mid-pause wall = %s, want 8s (3s carried + 1s busy + 4s waiting; the pause excluded)", got.Wall)
	}
	endPause()
	endPause()             // idempotent
	f.advance(time.Second) // busy
	got := c.State()
	if got.Wall != 9*time.Second {
		t.Errorf("wall = %s, want 9s", got.Wall)
	}
	if got.Active != 3*time.Second || got.Waited != 106*time.Second {
		t.Errorf("active = %s waited = %s, want 3s and 106s (the pause is a wait for the budget)", got.Active, got.Waited)
	}
}

// A wait beside a call that is working does not stop the clock: the working
// call is charged for as long as it runs, and only the stretch where the wait
// is the sole thing in flight counts as waited.
func TestRunClock_WaitBesideAWorkingCallChargesTheWork(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	ctx := WithRunClock(context.Background(), c)
	waitCtx, endWaiter := BeginWork(ctx) // e.g. a channel await
	_, endWorker := BeginWork(ctx)       // e.g. a Bash command, beside it
	endWait := BeginWait(waitCtx)
	f.advance(300 * time.Millisecond) // both in flight
	endWorker()
	f.advance(200 * time.Millisecond) // only the wait left
	endWait()
	endWaiter()
	if got := c.State(); got.Active != 300*time.Millisecond || got.Waited != 200*time.Millisecond {
		t.Fatalf("state = %+v, want active 300ms (the working call) waited 200ms", got)
	}
}

// Calls that are all waiting stop the clock, and overlapping waits count once.
func TestRunClock_ConcurrentWaitingCallsCountOnce(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	ctx := WithRunClock(context.Background(), c)
	aCtx, endA := BeginWork(ctx)
	bCtx, endB := BeginWork(ctx)
	endWaitA := BeginWait(aCtx)
	endWaitB := BeginWait(bCtx)
	f.advance(300 * time.Millisecond)
	endWaitA()
	endA()
	f.advance(100 * time.Millisecond) // B still waiting
	endWaitB()
	endB()
	if got := c.State(); got.Active != 0 || got.Waited != 400*time.Millisecond {
		t.Fatalf("state = %+v, want active 0 waited 400ms", got)
	}
}

func TestRunClock_ASingleWaitingCallChargesNothing(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	callCtx, endCall := BeginWork(WithRunClock(context.Background(), c))
	endWait := BeginWait(callCtx)
	f.advance(300 * time.Millisecond)
	endWait()
	endCall()
	if got := c.State(); got.Active != 0 || got.Waited != 300*time.Millisecond {
		t.Fatalf("state = %+v, want active 0 waited 300ms", got)
	}
}

// A call that waits for part of its run is charged for the rest, even with
// another call waiting the whole time beside it.
func TestRunClock_ACallIsChargedOnlyForItsNonWaitPart(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	ctx := WithRunClock(context.Background(), c)
	otherCtx, endOther := BeginWork(ctx)
	endOtherWait := BeginWait(otherCtx) // waits throughout
	callCtx, endCall := BeginWork(ctx)
	f.advance(200 * time.Millisecond) // the call works
	endWait := BeginWait(callCtx)
	f.advance(300 * time.Millisecond) // the call waits
	endWait()
	f.advance(100 * time.Millisecond) // the call works again
	endCall()
	endOtherWait()
	endOther()
	if got := c.State(); got.Active != 300*time.Millisecond || got.Waited != 300*time.Millisecond {
		t.Fatalf("state = %+v, want active 300ms (200+100 of work) waited 300ms", got)
	}
}

// A wait inside a wait of the same call (a team walk asking a question)
// suspends the call's work once and hands it back once.
func TestRunClock_NestedWaitsInOneCallSuspendItsWorkOnce(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	callCtx, endCall := BeginWork(WithRunClock(context.Background(), c))
	endOuter := BeginWait(callCtx)
	endInner := BeginWait(callCtx)
	f.advance(time.Second)
	endInner()
	f.advance(time.Second) // still inside the outer wait
	endOuter()
	f.advance(time.Second) // working
	endCall()
	if got := c.State(); got.Active != time.Second || got.Waited != 2*time.Second {
		t.Fatalf("state = %+v, want active 1s waited 2s", got)
	}
	endWait := c.BeginWait() // the call's work is gone: a later wait stops the clock
	f.advance(time.Second)
	endWait()
	if got := c.State(); got.Waited != 3*time.Second {
		t.Fatalf("waited = %s after the call ended, want 3s — its work outlived it", got.Waited)
	}
}

// A wait left open past its call's end (in a goroutine that outlived it) must
// not hand the call's work back when it closes.
func TestRunClock_AWaitOutlivingItsCallLeavesNoWork(t *testing.T) {
	c, f := newTestClock(RunClockState{})
	callCtx, endCall := BeginWork(WithRunClock(context.Background(), c))
	endWait := BeginWait(callCtx)
	endCall()
	f.advance(time.Second)
	endWait()
	endWait2 := c.BeginWait()
	f.advance(time.Second)
	endWait2()
	if got := c.State(); got.Active != 0 || got.Waited != 2*time.Second {
		t.Fatalf("state = %+v, want active 0 waited 2s — the ended call still counted as work", got)
	}
}

// A sub-run's ctx descends from its parent's tool call. A wait on the sub-run's
// own clock is a plain wait there, and must not suspend the parent call's work.
func TestBeginWait_AnotherRunsCallIsNotSuspended(t *testing.T) {
	parent, f := newTestClock(RunClockState{})
	ctx := WithRunClock(context.Background(), parent)
	callCtx, endCall := BeginWork(ctx)
	child := NewRunClock(f.t, RunClockState{})
	child.now = f.now
	endChildWait := BeginWait(WithRunClock(callCtx, child))
	endParentWait := BeginWait(ctx) // another wait of the parent, beside the call
	f.advance(time.Second)
	endParentWait()
	endChildWait()
	endCall()
	if got := parent.State(); got.Active != time.Second || got.Waited != 0 {
		t.Fatalf("parent state = %+v, want active 1s — the child's wait suspended the parent's call", got)
	}
	if got := child.State(); got.Active != 0 || got.Waited != time.Second {
		t.Fatalf("child state = %+v, want waited 1s — the parent's call mark leaked into the child's clock", got)
	}
}

func TestBeginWork_NoClockIsANoOp(t *testing.T) {
	ctx := context.Background()
	got, end := BeginWork(ctx)
	end()
	if got != ctx {
		t.Fatal("BeginWork without a clock must return ctx unchanged")
	}
}
