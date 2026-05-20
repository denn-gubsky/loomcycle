package http

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// AwaitedState constants — the wire string values returned in the
// agent response's `awaited_state` field. Kept in one place so the
// derivation and the test asserts can't drift apart.
const (
	awaitedStateChannel     = "channel"
	awaitedStateInterrupted = "interrupted"
)

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

// deriveAwaitedState inspects the latest event of a running agent
// and returns (state, on) describing what (if anything) the agent
// is blocked on:
//
//	state="channel"     on=<channel name>   — open Channel.subscribe
//	state="interrupted" on=<kind|op>        — open Interruption.ask
//	state=""            on=""               — agent is making progress
//
// Why "last event" suffices: the loomcycle loop is synchronous from
// the goroutine's view — between a tool_call and its matching
// tool_result, NO other events emit. So "the last event is a
// tool_call to X" is equivalent to "X is currently executing."
// This collapses what the client-side derivation needs (walking
// unresolved tool_uses) into a single row lookup.
func deriveAwaitedState(ev store.Event) (state, on string) {
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
			return awaitedStateChannel, ci.Channel
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
			return awaitedStateInterrupted, kind
		}
	}
	return "", ""
}

// fillAwaitedStateForRunning enriches a slice of agentResponse with
// the awaited_state / awaited_on fields by issuing a GetLastEventForRun
// per running entry. Non-running rows are skipped. ErrNotFound
// (run has no events yet) is silently treated as "no awaited state"
// — common immediately after a CreateRun. Other store errors are
// logged but don't fail the request (the chip tint is a UI nicety,
// not a correctness signal).
func fillAwaitedStateForRunning(ctx context.Context, st store.Store, items []agentResponse) {
	for i, item := range items {
		if item.Status != store.RunRunning {
			continue
		}
		ev, err := st.GetLastEventForRun(ctx, item.RunID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				continue
			}
			log.Printf("awaited_state: GetLastEventForRun(%s) failed: %v", item.RunID, err)
			continue
		}
		state, on := deriveAwaitedState(ev)
		if state != "" {
			items[i].AwaitedState = state
			items[i].AwaitedOn = on
		}
	}
}
