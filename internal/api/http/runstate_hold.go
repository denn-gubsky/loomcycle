package http

import (
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// runHold is what a run is waiting on, as the run-state stream was last told.
//
// A waiting run stays "running" — the owner's call: a wait is not a lifecycle
// status, and every consumer that switches on status keeps working. Before
// this the stream said nothing at all about a wait, so a run held for a person
// looked, to everyone watching the stream, like a run that was simply slow; the
// wait was visible only on the run's own event stream.
type runHold struct {
	state, on, expiresAt string
	// calls are the channel and interruption waits still open, oldest first;
	// state and on name the newest. A turn's tool calls run in parallel, so
	// one call's result ends that wait only — the run is still held while any
	// other is open. Empty for a park (review, input), which the model working
	// again ends instead.
	calls []openWait
}

// openWait is one blocking tool call that has not returned yet.
type openWait struct{ toolUseID, state, on string }

// waitingOn is the hold for the open calls: the newest one's wait, or no wait
// once none is open.
func waitingOn(calls []openWait) runHold {
	if len(calls) == 0 {
		return runHold{}
	}
	top := calls[len(calls)-1]
	return runHold{state: top.state, on: top.on, calls: calls}
}

// sameAnnouncement reports whether two holds read the same on the stream. A
// review hold is announced again after a compaction, with the same deadline;
// telling subscribers twice would say the run was held twice.
func (h runHold) sameAnnouncement(o runHold) bool {
	return h.state == o.state && h.on == o.on && h.expiresAt == o.expiresAt
}

// holdAfter folds one of the run's events into its wait.
//
// Every wait starts on an event the recording emit sees: awaiting_review,
// awaiting_input, or the tool_call of a blocking Channel/Interruption op. The
// ends are what the loop does next — a park ends on the operator's turn
// (steer) or the model producing output again, which is also how the steer
// registry decides a run is no longer parked; a tool wait ends on ITS
// tool_result, not any, because tools run in parallel and a sibling call's
// result says nothing about this one — and when another wait is still open the
// hold becomes that one; and done ends everything.
//
// An interactive run's approval moves a review hold straight to input, and an
// abandoned or rejected hold ends in a terminal status, which the finishRun
// publish reports; neither needs an event of its own here.
func holdAfter(cur runHold, ev providers.Event) runHold {
	switch ev.Type {
	case providers.EventAwaitingReview:
		h := runHold{state: awaited.Review}
		if r := ev.AwaitingReview; r != nil {
			h.on, h.expiresAt = r.HeldBy, r.ExpiresAt
		}
		return h
	case providers.EventAwaitingInput:
		return runHold{state: awaited.Input}
	case providers.EventToolCall:
		if ev.ToolUse != nil {
			if state, on := awaited.FromToolUse(ev.ToolUse.Name, ev.ToolUse.Input); state != "" {
				// A fresh slice: cur's is the stream's last-told hold and must not
				// change under it.
				calls := append(slices.Clone(cur.calls), openWait{ev.ToolUse.ID, state, on})
				return waitingOn(calls)
			}
		}
		if len(cur.calls) == 0 {
			return runHold{}
		}
	case providers.EventToolResult:
		if ev.ToolUse != nil {
			if i := slices.IndexFunc(cur.calls, func(w openWait) bool { return w.toolUseID == ev.ToolUse.ID }); i >= 0 {
				return waitingOn(slices.Delete(slices.Clone(cur.calls), i, i+1))
			}
		}
	case providers.EventSteer, providers.EventText:
		if len(cur.calls) == 0 {
			return runHold{}
		}
	case providers.EventDone:
		return runHold{}
	}
	return cur
}

// publishHold announces a run's wait — or, for the zero hold, its end — on
// the run-state stream as a "running" transition.
func (s *Server) publishHold(m runStateMeta, h runHold) {
	if s.runStateBus == nil {
		return
	}
	evt := m.runStateEvent("running")
	evt.AwaitedState, evt.AwaitedOn, evt.HoldExpiresAt = h.state, h.on, h.expiresAt
	s.runStateBus.Publish(evt)
}
