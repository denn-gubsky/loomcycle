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
