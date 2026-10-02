package providers

import (
	"context"
	"testing"
	"time"
)

// stepClock returns t0, t0+steps[0], t0+steps[0]+steps[1], … on successive calls.
func stepClock(t0 time.Time, steps ...time.Duration) func() time.Time {
	now := t0
	i := -1
	return func() time.Time {
		if i >= 0 && i < len(steps) {
			now = now.Add(steps[i])
		}
		i++
		return now
	}
}

func TestCallTimer_StampsDurationAndTimeToFirstEvent(t *testing.T) {
	clk := stepClock(time.Unix(1000, 0), 250*time.Millisecond, 1750*time.Millisecond)
	tm := StartCallTimer(clk)              // t0
	tm.Observe(Event{Type: EventThinking}) // +250ms: first
	tm.Observe(Event{Type: EventText})     // not a clock read
	tm.Observe(Event{Type: EventDone})     // +1750ms: done
	u := &Usage{Timing: &CallTiming{LoadMs: 40, PrefillMs: 100, DecodeMs: 1500, ServerTotalMs: 1700}}
	tm.Stamp(u)
	got := *u.Timing
	want := CallTiming{DurationMs: 2000, TTFTMs: 250, LoadMs: 40, PrefillMs: 100, DecodeMs: 1500, QueueMs: 300, ServerTotalMs: 1700}
	if got != want {
		t.Fatalf("timing = %+v, want %+v", got, want)
	}
}

func TestCallTimer_LeavesAFailedOrUnfinishedCallUnstamped(t *testing.T) {
	failed := StartCallTimer(stepClock(time.Unix(0, 0), time.Second, time.Second))
	failed.Observe(Event{Type: EventError})
	failed.Observe(Event{Type: EventDone})
	u := &Usage{}
	failed.Stamp(u)
	if u.Timing != nil {
		t.Fatalf("a call that errored was stamped: %+v", u.Timing)
	}

	unfinished := StartCallTimer(stepClock(time.Unix(0, 0), time.Second))
	unfinished.Observe(Event{Type: EventText})
	unfinished.Stamp(u)
	if u.Timing != nil {
		t.Fatalf("a call with no done event was stamped: %+v", u.Timing)
	}
}

func TestObserveCall_ReachesTheObserverOnTheContext(t *testing.T) {
	var seen *Usage
	ctx := WithCallObserver(context.Background(), func(u *Usage) { seen = u })
	u := &Usage{Model: "m"}
	ObserveCall(ctx, u)
	if seen != u {
		t.Fatalf("observer saw %v, want the usage", seen)
	}
	ObserveCall(context.Background(), u) // no observer: no panic
}
