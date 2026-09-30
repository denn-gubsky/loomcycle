// Package awaited derives what a running agent is currently blocked on — an
// open Channel.subscribe, an open Interruption.ask, a hold for an operator's
// review verdict, or an interactive run parked for the operator's next turn —
// from the run's persisted events.
//
// It lives outside any one transport because every transport's run read model
// reports it (the HTTP agent response and the gRPC Agent message); deriving it
// in one place keeps the two from answering differently for the same run.
package awaited

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The wire string values of a run read's `awaited_state` field. Kept in one
// place so the derivation and the test asserts can't drift apart.
const (
	Channel     = "channel"
	Interrupted = "interrupted"
	// Review: held for an operator's verdict on a finished answer.
	Review = "review"
	// Input: an interactive run parked at the end of a turn, waiting for the
	// operator's next message.
	Input = "input"
)

// EventReader is the slice of store.Store the derivation reads.
type EventReader interface {
	GetLastEventOfTypes(ctx context.Context, runID string, types []string) (store.Event, error)
	GetLastEventOfTypesBefore(ctx context.Context, runID string, types []string, beforeSeq int64) (store.Event, error)
	GetRunEventsSince(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error)
}

// payloadToolCall mirrors providers.Event's persisted JSON shape
// for tool_call rows — we decode only what the awaited-state
// derivation needs (tool name + first-level input dispatch fields).
// Input is decoded twice: once as a Channel.subscribe input, once
// as an Interruption input. Both unmarshals are cheap on a small
// JSON object.
type payloadToolCall struct {
	ToolUse struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"tool_use"`
}

type channelInput struct {
	Op      string `json:"op"`
	Channel string `json:"channel"`
}

type interruptionInput struct {
	Op   string `json:"op"`
	Kind string `json:"kind"`
}

// FromEvent returns (state, on) describing the wait one persisted event
// starts, if any:
//
//	state="channel"     on=<channel name>   — a Channel.subscribe call
//	state="interrupted" on=<kind|op>        — an Interruption.ask call
//	state="input"       on=""               — parked for the operator's turn
//	state=""            on=""               — no wait
//
// Whether that wait is still open is ForRun's question: the event that
// started it is rarely the run's latest (see openCall).
func FromEvent(ev store.Event) (state, on string) {
	if ev.Type == string(providers.EventAwaitingInput) {
		return Input, ""
	}
	if ev.Type != "tool_call" {
		return "", ""
	}
	var p payloadToolCall
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return "", ""
	}
	return FromToolUse(p.ToolUse.Name, p.ToolUse.Input)
}

// FromToolUse is what a tool call blocks its run on while it executes: the
// FromEvent rule for one call, for a caller holding the live event rather than
// its persisted row (the run-state stream announces the hold as the call
// starts). Any other tool, and a non-blocking op of these two, is "".
func FromToolUse(name string, input json.RawMessage) (state, on string) {
	switch name {
	case "Channel":
		var ci channelInput
		if err := json.Unmarshal(input, &ci); err != nil {
			return "", ""
		}
		if ci.Op == "subscribe" {
			return Channel, ci.Channel
		}
	case "Interruption":
		var ii interruptionInput
		if err := json.Unmarshal(input, &ii); err != nil {
			return "", ""
		}
		// v0.8.16 always emits kind="question" for "ask"; future
		// kinds will land verbatim. For non-ask ops the
		// Interruption tool doesn't block, so we ignore them.
		if ii.Op == "ask" {
			kind := ii.Kind
			if kind == "" {
				kind = "question"
			}
			return Interrupted, kind
		}
	}
	return "", ""
}

// HoldEndingEvents are what a run writes when it leaves a hold, whatever the
// verdict: the feedback turn (user_input), the park an approved interactive run
// moves to (awaiting_input), or the end (done). The run is held while its
// latest awaiting_review is newer than all of them. awaiting_input is itself a
// wait: an interactive run is parked while its latest awaiting_input is newer
// than the rest, until the operator's turn lands as user_input.
//
// Keyed on what ENDS a hold rather than on the run's latest event, because
// other writers append to a held run without ending it — a retune's override
// event, a budget limit, a compaction marker — and each of those would make
// "the latest event is awaiting_review" false for a run that is still held.
var HoldEndingEvents = []string{
	string(providers.EventAwaitingReview), // listed so the query can return it
	"user_input",
	string(providers.EventAwaitingInput),
	string(providers.EventDone),
}

// HeldBy reports whether the run is held for review and, if an agent_stop
// hook took the hold, which one.
func HeldBy(ctx context.Context, st EventReader, runID string) (bool, string) {
	last, err := st.GetLastEventOfTypes(ctx, runID, HoldEndingEvents)
	if err != nil || last.Type != string(providers.EventAwaitingReview) {
		return false, ""
	}
	var p providers.Event
	if json.Unmarshal(last.Payload, &p) == nil && p.AwaitingReview != nil {
		return true, p.AwaitingReview.HeldBy
	}
	return true, ""
}

// ForRun returns what a RUNNING run is blocked on — the caller skips runs in
// any other status. ErrNotFound (run has no events yet) is silently treated as
// "no awaited state" — common immediately after a CreateRun. Other store
// errors are logged and read as "no awaited state" too: the field is a
// read-model nicety, not a correctness signal, so it must not fail the read.
//
// An open channel or interruption call is reported first — with several open,
// the newest, as the run-state stream announces it. A review hold reports the
// agent_stop hook that took it as `on` (empty when review arming took it),
// which is what the run-state stream announces too.
func ForRun(ctx context.Context, st EventReader, runID string) (state, on string) {
	state, on, err := openCall(ctx, st, runID)
	if err != nil {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			log.Printf("awaited_state: reading run %s's open tool calls failed: %v", runID, err)
		}
	}
	if state != "" {
		return state, on
	}
	// Not from the latest event: other writers append to a held or parked run
	// without ending the wait (see HoldEndingEvents) — a compaction marker on a
	// parked interactive run is the common one.
	last, err := st.GetLastEventOfTypes(ctx, runID, HoldEndingEvents)
	if err != nil {
		return "", ""
	}
	switch last.Type {
	case string(providers.EventAwaitingReview):
		var p providers.Event
		if json.Unmarshal(last.Payload, &p) == nil && p.AwaitingReview != nil {
			return Review, p.AwaitingReview.HeldBy
		}
		return Review, ""
	case string(providers.EventAwaitingInput):
		return Input, ""
	}
	return "", ""
}

// eventPageSize bounds one read of openCall's scan.
const eventPageSize = 256

// turnBoundaries are written after every call of a turn and before the next
// turn's first: the turn's usage row (once the model finished streaming, before
// the calls run) and its results.
var turnBoundaries = []string{
	string(providers.EventToolResult),
	string(providers.EventUsage),
}

// callsAbandoned are the events after which no call streamed before them is
// still running: the model call is sent again, the turn is cancelled, or the
// run parked, was held or ended.
var callsAbandoned = []string{
	string(providers.EventRetry),
	string(providers.EventProviderFallback),
	string(providers.EventTurnCancelled),
	string(providers.EventAwaitingInput),
	string(providers.EventAwaitingReview),
	string(providers.EventDone),
}

// openCall is the newest channel or interruption call of the run that has not
// returned yet.
//
// Not the run's latest event: a turn's tool calls stream in before any of them
// runs, the turn's usage row lands after them, and then the calls run in
// parallel — so a blocked call is almost never the latest row, and a sibling's
// result does not end it. Only the latest turn with tool calls can have one
// open (the loop runs a turn's calls to completion before the next turn), so
// the scan starts at the newest turn boundary older than the newest tool_call
// and pairs calls with results by tool_use id from there. A call that never
// ran — streamed before a retry or a cancel — has no result, so those events
// end every call before them.
func openCall(ctx context.Context, st EventReader, runID string) (state, on string, err error) {
	lastCall, err := st.GetLastEventOfTypes(ctx, runID, []string{string(providers.EventToolCall)})
	if err != nil {
		return "", "", err
	}
	var after int64
	prev, err := st.GetLastEventOfTypesBefore(ctx, runID, turnBoundaries, lastCall.Seq)
	switch {
	case err == nil:
		after = prev.Seq
	case !errors.As(err, new(*store.ErrNotFound)):
		return "", "", err
	}
	type wait struct{ id, state, on string }
	var open []wait
	for {
		page, err := st.GetRunEventsSince(ctx, runID, after, eventPageSize)
		if err != nil {
			return "", "", err
		}
		for _, ev := range page {
			switch ev.Type {
			case string(providers.EventToolCall):
				if s, o := FromEvent(ev); s != "" {
					open = append(open, wait{toolUseID(ev), s, o})
				}
			case string(providers.EventToolResult):
				id := toolUseID(ev)
				open = slices.DeleteFunc(open, func(w wait) bool { return w.id == id })
			default:
				if slices.Contains(callsAbandoned, ev.Type) {
					open = nil
				}
			}
			// Past the turn's last call nothing new opens, so once its waits
			// have all returned the rest of the run is not read.
			if ev.Seq >= lastCall.Seq && len(open) == 0 {
				return "", "", nil
			}
		}
		if len(page) < eventPageSize {
			break
		}
		after = page[len(page)-1].Seq
	}
	if len(open) == 0 {
		return "", "", nil
	}
	top := open[len(open)-1]
	return top.state, top.on, nil
}

// toolUseID is the tool_use id a persisted tool_call or tool_result names.
func toolUseID(ev store.Event) string {
	var p payloadToolCall
	if json.Unmarshal(ev.Payload, &p) != nil {
		return ""
	}
	return p.ToolUse.ID
}
