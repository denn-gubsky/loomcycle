// Package awaited derives what a running agent is currently blocked on — an
// open Channel.subscribe, an open Interruption.ask, or a hold for an
// operator's review verdict — from the run's persisted events.
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
)

// EventReader is the slice of store.Store the derivation reads.
type EventReader interface {
	GetLastEventForRun(ctx context.Context, runID string) (store.Event, error)
	GetLastEventOfTypes(ctx context.Context, runID string, types []string) (store.Event, error)
}

// payloadToolCall mirrors providers.Event's persisted JSON shape
// for tool_call rows — we decode only what the awaited-state
// derivation needs (tool name + first-level input dispatch fields).
// Input is decoded twice: once as a Channel.subscribe input, once
// as an Interruption input. Both unmarshals are cheap on a small
// JSON object.
type payloadToolCall struct {
	ToolUse struct {
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

// FromEvent inspects the latest event of a running agent
// and returns (state, on) describing what (if anything) the agent
// is blocked on:
//
//	state="channel"     on=<channel name>   — open Channel.subscribe
//	state="interrupted" on=<kind|op>        — open Interruption.ask
//	state=""            on=""               — agent is making progress
//
// A review hold is not read from the latest event; see ForRun.
//
// Why "last event" suffices: the loomcycle loop is synchronous from
// the goroutine's view — between a tool_call and its matching
// tool_result, NO other events emit. So "the last event is a
// tool_call to X" is equivalent to "X is currently executing."
// This collapses what the client-side derivation needs (walking
// unresolved tool_uses) into a single row lookup.
func FromEvent(ev store.Event) (state, on string) {
	if ev.Type != "tool_call" {
		return "", ""
	}
	var p payloadToolCall
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return "", ""
	}
	switch p.ToolUse.Name {
	case "Channel":
		var ci channelInput
		if err := json.Unmarshal(p.ToolUse.Input, &ci); err != nil {
			return "", ""
		}
		if ci.Op == "subscribe" {
			return Channel, ci.Channel
		}
	case "Interruption":
		var ii interruptionInput
		if err := json.Unmarshal(p.ToolUse.Input, &ii); err != nil {
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
// latest awaiting_review is newer than all of them.
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
func ForRun(ctx context.Context, st EventReader, runID string) (state, on string) {
	ev, err := st.GetLastEventForRun(ctx, runID)
	if err != nil {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			log.Printf("awaited_state: GetLastEventForRun(%s) failed: %v", runID, err)
		}
		return "", ""
	}
	state, on = FromEvent(ev)
	if state == "" {
		if held, _ := HeldBy(ctx, st, runID); held {
			// Not from the latest event: other writers append to a held run
			// without ending the hold (see HoldEndingEvents).
			return Review, ""
		}
	}
	return state, on
}
