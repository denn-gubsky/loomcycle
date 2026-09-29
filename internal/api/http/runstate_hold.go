package http

import (
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
	// toolUseID is the open tool call a channel or interruption wait ends
	// with. Empty for a park (review, input), which the model working again
	// ends instead.
	toolUseID string
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
// result says nothing about this one; and done ends everything.
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
				return runHold{state: state, on: on, toolUseID: ev.ToolUse.ID}
			}
		}
		if cur.toolUseID == "" {
			return runHold{}
		}
	case providers.EventToolResult:
		if cur.toolUseID != "" && ev.ToolUse != nil && ev.ToolUse.ID == cur.toolUseID {
			return runHold{}
		}
	case providers.EventSteer, providers.EventText:
		if cur.toolUseID == "" {
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
