package http

import (
	"encoding/json"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// statefulSeed is where a stateful run starts, derived from persisted events.
//
// ⚠️ A STATEFUL RUN'S HISTORY IS NOT ITS MESSAGES. Resume and continuation used
// to hand a stateful run replayTranscript's output, and the stateful loop
// rendered its first observation from it: every operator message and every
// answer so far, relabelled "Task:", growing with the conversation. And resume
// decided whether a run could continue from the last MESSAGE's role, on a run
// that has no messages. Both answers come from the stateful events instead.
type statefulSeed struct {
	// HasState is false when no step ever recorded a context_state: there is
	// no Σ to restore, and the caller keeps its message-shaped path.
	HasState bool
	Sigma    map[string]any
	// Observation is the first observation, verbatim. "" means nothing is
	// pending: an interactive run parks for the operator (StartParked).
	Observation string
	StartParked bool
}

// statefulSeedFromEvents reads the seed forward from the LAST context_state
// marker (C).
//
// ⚠️ THIS MAKES context_state LOAD-BEARING: each marker carries the whole
// post-merge Σ, and the last one is the run's memory on resume and on
// continuation. Trimming those markers to save transcript bytes would make
// every such run silently forget everything it knew (the loop's own comment on
// EventContextState says the same, pointing here).
//
// Rules:
//
//  1. no C: nothing to restore (HasState=false). The observation is the task —
//     the user_input so far plus fresh — for a caller that runs anyway.
//  2. C named an action and its tool_result follows (paused between steps):
//     the observation is that result, marked as an error when it was one.
//  3. C named an action and no result exists (the process stopped during the
//     call): the observation says so. The action is NEVER re-run on the
//     model's behalf — it may not be idempotent, and whether it took effect is
//     unknown.
//  4. C ended a turn and user_input follows (a steer the park consumed, or a
//     continuation's fresh segments): the observation is that message.
//  5. C ended a turn and nothing follows: nothing is pending — an interactive
//     run parks for the operator.
//
// fresh is a continuation's new prompt (nil on resume). interactive selects how
// a person's message is framed: `operator: ` is what the stateful prompt tells
// an interactive run to expect, `Task: ` what an autonomous run is given.
func statefulSeedFromEvents(events []store.Event, fresh []loop.PromptSegment, interactive bool) statefulSeed {
	var seed statefulSeed
	last := -1
	var lastAction string
	for i, ev := range events {
		if ev.Type != string(providers.EventContextState) {
			continue
		}
		var pe providers.Event
		if err := json.Unmarshal(ev.Payload, &pe); err != nil || pe.ContextState == nil {
			continue
		}
		last, lastAction = i, pe.ContextState.Action
		// A marker whose state was dropped (it would not parse after secret
		// masking) still marks the position; the Σ comes from the one before.
		if pe.ContextState.State != nil {
			seed.Sigma = pe.ContextState.State
		}
	}
	freshText := userText(fresh)

	if last < 0 {
		var texts []string
		for _, ev := range events {
			if t := userInputText(ev); t != "" {
				texts = append(texts, t)
			}
		}
		if freshText != "" {
			texts = append(texts, freshText)
		}
		if len(texts) > 0 {
			seed.Observation = "Task: " + strings.Join(texts, "\n")
		}
		return seed
	}
	seed.HasState = true

	// What the step did next is read from the events, not from the marker's
	// action field: a step may name an action AND set done, and the loop treats
	// that as the end of the turn. The first tool_call or text after C says
	// which one happened.
	var parts []string
	callID, turnEnded := "", false
	rest := events[last+1:]
	for _, ev := range rest {
		if ev.Type == string(providers.EventToolCall) {
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				callID = pe.ToolUse.ID
			}
			break
		}
		if ev.Type == string(providers.EventText) {
			turnEnded = true
			break
		}
	}
	switch {
	case turnEnded || (callID == "" && lastAction == ""):
		for _, ev := range rest {
			if t := userInputText(ev); t != "" {
				parts = append(parts, personSays(t, interactive))
			}
		}
	default:
		result, answered := "", false
		for _, ev := range rest {
			if ev.Type != string(providers.EventToolResult) {
				continue
			}
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err != nil {
				continue
			}
			if callID != "" && (pe.ToolUse == nil || pe.ToolUse.ID != callID) {
				continue
			}
			result, answered = pe.Text, true
			if pe.IsError && !strings.HasPrefix(result, "ERROR: ") {
				result = "ERROR: " + result
			}
			break
		}
		if !answered {
			result = "ERROR: the action `" + lastAction + "` was interrupted by a restart before it " +
				"returned, so it may or may not have taken effect. Check before re-issuing it."
		}
		parts = append(parts, result)
	}
	if freshText != "" {
		parts = append(parts, personSays(freshText, interactive))
	}
	seed.Observation = strings.Join(parts, "\n\n")
	seed.StartParked = seed.Observation == "" && interactive
	return seed
}

// statefulContinuationSeed seeds a session continuation that will run the
// stateful loop from the session's own events. ok is false — and the caller
// keeps its replayed messages — when the run is not stateful, or when the
// session never recorded a Σ (it was running another mode until now, and its
// conversation is only in its messages).
//
// ⚠️ THIS WAS NEVER DONE. Only resume restored Σ, so every continuation of a
// stateful session started from an EMPTY state with the whole replayed
// transcript as its first observation — and in the embedded terminal that was
// every message after the first.
func statefulContinuationSeed(sessionEvents []store.Event, cx *config.Context, local, interactive bool,
	fresh []loop.PromptSegment) (statefulSeed, bool) {
	if len(sessionEvents) == 0 || !loop.StatefulMode(cx, local, interactive) {
		return statefulSeed{}, false
	}
	seed := statefulSeedFromEvents(sessionEvents, fresh, interactive)
	if !seed.HasState {
		return statefulSeed{}, false
	}
	// A continuation carries a new message, so it never starts parked.
	seed.StartParked = false
	return seed, true
}

// personSays frames a person's message the way the live loop does: the park
// hands an interactive run `operator: <text>`, and an autonomous run's task is
// `Task: <text>`.
func personSays(text string, interactive bool) string {
	if interactive {
		return "operator: " + text
	}
	return "Task: " + text
}

// userInputText is the user-role text of one user_input event.
func userInputText(ev store.Event) string {
	if ev.Type != "user_input" {
		return ""
	}
	var segs []loop.PromptSegment
	if err := json.Unmarshal(ev.Payload, &segs); err != nil {
		return ""
	}
	return userText(segs)
}

func userText(segs []loop.PromptSegment) string {
	var b strings.Builder
	for _, seg := range segs {
		if seg.Role != "user" {
			continue
		}
		for _, c := range seg.Content {
			if fc := loop.FlattenContent(c); fc.Type == "text" && fc.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(fc.Text)
			}
		}
	}
	return strings.TrimSpace(b.String())
}
