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
	// Queue is the wall time outside the reported phases: 2000 − (40+100+1500).
	want := CallTiming{DurationMs: 2000, TTFTMs: 250, LoadMs: 40, PrefillMs: 100, DecodeMs: 1500, QueueMs: 360, ServerTotalMs: 1700}
	if got != want {
		t.Fatalf("timing = %+v, want %+v", got, want)
	}
}

// Two concurrent calls to one Ollama model are served one after the other, and
// Ollama's total_duration for the second INCLUDES its wait for the runner. These
// are the second call's numbers from a live lab run: the ~150 s it waited must
// be queue time, not hidden inside the server total as work.
func TestCallTimer_CountsAWaitInsideTheServerAsQueue(t *testing.T) {
	clk := stepClock(time.Unix(0, 0), 153_777*time.Millisecond, (271_459-153_777)*time.Millisecond)
	tm := StartCallTimer(clk)
	tm.Observe(Event{Type: EventText})
	tm.Observe(Event{Type: EventDone})
	u := &Usage{Timing: &CallTiming{LoadMs: 1, PrefillMs: 3291, DecodeMs: 117_677, ServerTotalMs: 271_454}}
	tm.Stamp(u)
	if got, want := u.Timing.QueueMs, int64(271_459-(1+3291+117_677)); got != want {
		t.Fatalf("queue_ms = %d, want %d (wall − load − prefill − decode); the server total hid the wait", got, want)
	}
}

// A server that reports only its total (no phases) still gets the wall time it
// did not account for as queue.
func TestCallTimer_QueueFromTheServerTotalWhenNoPhaseIsReported(t *testing.T) {
	tm := StartCallTimer(stepClock(time.Unix(0, 0), 2*time.Second))
	tm.Observe(Event{Type: EventDone})
	u := &Usage{Timing: &CallTiming{ServerTotalMs: 1500}}
	tm.Stamp(u)
	if u.Timing.QueueMs != 500 {
		t.Fatalf("queue_ms = %d, want 500", u.Timing.QueueMs)
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
