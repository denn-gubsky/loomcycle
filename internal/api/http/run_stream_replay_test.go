package http

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// mustSegmentsJSON marshals operator segments the way the run handler persists a
// user_input row (server.go steer/initial-input persist).
func mustSegmentsJSON(t *testing.T, text string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal([]loop.PromptSegment{{
		Role:    "user",
		Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRunEventToFrame_SelfSufficientReattach is the RFC AI S1 regression: the
// re-attach tail must REPLAY a persisted user_input row as a `steer` frame (so a
// cold client reconstructs the operator's turns), skip the system_prompt, and
// pass a normal loop event through unchanged. Fail-before: the old code skipped
// user_input entirely, so a cold re-attach lost the operator's side.
func TestRunEventToFrame_SelfSufficientReattach(t *testing.T) {
	// 1) user_input → a steer/replay frame carrying the operator's text.
	pe, ok := runEventToFrame(store.Event{Type: "user_input", Payload: mustSegmentsJSON(t, "ship it")})
	if !ok {
		t.Fatalf("user_input row should convert to a streamable frame")
	}
	if pe.Type != providers.EventSteer {
		t.Errorf("type = %q, want %q", pe.Type, providers.EventSteer)
	}
	if pe.UserInput == nil || pe.UserInput.Text != "ship it" {
		t.Errorf("user_input frame lost the operator text: %+v", pe.UserInput)
	}
	if pe.UserInput.Source != "replay" {
		t.Errorf("replayed operator turn must be marked source=replay (so a same-session client de-dupes), got %q", pe.UserInput.Source)
	}

	// 2) system_prompt is NOT conversational — skipped.
	if _, ok := runEventToFrame(store.Event{Type: "system_prompt", Payload: json.RawMessage(`{}`)}); ok {
		t.Errorf("system_prompt must be skipped on the tail")
	}

	// 3) a normal loop event round-trips unchanged.
	textPayload, _ := json.Marshal(providers.Event{Type: providers.EventText, Text: "hello"})
	pe, ok = runEventToFrame(store.Event{Type: "text", Payload: textPayload})
	if !ok || pe.Type != providers.EventText || pe.Text != "hello" {
		t.Errorf("normal event didn't round-trip: ok=%v ev=%+v", ok, pe)
	}

	// 4) a malformed user_input (no text) is skipped rather than emitting an
	// empty frame.
	if _, ok := runEventToFrame(store.Event{Type: "user_input", Payload: mustSegmentsJSON(t, "")}); ok {
		t.Errorf("empty-text user_input should be skipped, not emitted as a blank steer frame")
	}
}
