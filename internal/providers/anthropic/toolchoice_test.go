package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func bodyOf(t *testing.T, req providers.Request) map[string]any {
	t.Helper()
	b, err := buildRequestBody(req)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, b)
	}
	return m
}

func baseReq() providers.Request {
	return providers.Request{
		Model:     "claude-opus-5",
		MaxTokens: 64,
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:     []providers.ToolSpec{{Name: "emit_state", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
}

// ⚠️ THE SHAPES ARE NOT SHARED, so the assertions are not either. Anthropic
// spells "any tool" as {"type":"any"} where OpenAI says "required"; a helper
// that checked "the body mentions the tool somewhere" would pass on either
// driver emitting the other's dialect.
func TestToolChoice_AnthropicWireShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   providers.ToolChoice
		want any
	}{
		{"none", providers.ToolChoice{Mode: providers.ToolChoiceNone}, map[string]any{"type": "none"}},
		{"required", providers.ToolChoice{Mode: providers.ToolChoiceRequired}, map[string]any{"type": "any"}},
		{"named", providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"},
			map[string]any{"type": "tool", "name": "emit_state"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseReq()
			req.ToolChoice = tc.in
			got := bodyOf(t, req)["tool_choice"]
			if !jsonEqual(got, tc.want) {
				t.Errorf("tool_choice = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The unopted body must be BYTE-IDENTICAL to the pre-RFC-DG one, which means
// the key is absent rather than present-and-null.
func TestToolChoice_AnthropicAutoSendsNothing(t *testing.T) {
	for _, tc := range []providers.ToolChoice{
		{}, // the zero value
		{Mode: providers.ToolChoiceAuto},
		{Mode: providers.ToolChoiceTool}, // named with no name is not a choice
	} {
		req := baseReq()
		req.ToolChoice = tc
		if _, present := bodyOf(t, req)["tool_choice"]; present {
			t.Errorf("%+v put a tool_choice on the wire; auto must send nothing", tc)
		}
	}
}

// ⚠️ THE COMBINATION IS REACHABLE FROM ORDINARY CONFIGURATION, not just from a
// mistake: this driver attaches extended thinking whenever an agent declares an
// effort hint on a reasoning-capable model, and Anthropic returns 400 for a
// forced choice in that state. Thinking wins and the choice is dropped, for the
// same reason it already wins over temperature — the operator opted into
// reasoning explicitly, while forcing is an optimisation of a prompt contract.
func TestToolChoice_AnthropicDropsForcingUnderExtendedThinking(t *testing.T) {
	req := baseReq()
	req.Effort = "high"
	// max_tokens must leave room for the budget: the driver skips thinking
	// entirely when the 8192-token high budget cannot fit under it, which is
	// how the first draft of this test ended up asserting nothing.
	req.MaxTokens = 16384
	req.ToolChoice = providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	body := bodyOf(t, req)

	// Non-vacuity: thinking must actually be attached, or this proves nothing.
	if _, ok := body["thinking"]; !ok {
		t.Fatalf("fixture did not engage extended thinking, so the conflict is untested: %v", body)
	}
	if got, present := body["tool_choice"]; present {
		t.Errorf("tool_choice %v survived alongside extended thinking — Anthropic 400s on that pair", got)
	}

	// `none` is accepted under thinking, so it must NOT be dropped: an
	// over-broad guard would silently re-enable tools on a run that asked for
	// none of them.
	req.ToolChoice = providers.ToolChoice{Mode: providers.ToolChoiceNone}
	if got := bodyOf(t, req)["tool_choice"]; !jsonEqual(got, map[string]any{"type": "none"}) {
		t.Errorf("tool_choice none = %#v under thinking, want it preserved", got)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return strings.EqualFold(string(x), string(y))
}
