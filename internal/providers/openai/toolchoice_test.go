package openai

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func tcBody(t *testing.T, tc providers.ToolChoice) map[string]any {
	t.Helper()
	b, err := buildRequestBody(providers.Request{
		Model:    "gpt-5.6",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []providers.ToolSpec{{Name: "emit_state", Description: "d",
			InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: tc,
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, b)
	}
	return m
}

// ⚠️ THE NAMED FORM IS THE NESTED ONE. Chat Completions takes
// {"type":"function","function":{"name":X}}; the flat {"type":"function",
// "name":X} is the Responses API's shape, and this driver posts to
// /chat/completions. Some gateways accept the wrong one as a silent no-op,
// which is the worst outcome available — forcing that does nothing, reported
// as success.
func TestToolChoice_OpenAIWireShapes(t *testing.T) {
	if got := tcBody(t, providers.ToolChoice{Mode: providers.ToolChoiceNone})["tool_choice"]; got != "none" {
		t.Errorf("none = %#v, want the bare string \"none\"", got)
	}
	if got := tcBody(t, providers.ToolChoice{Mode: providers.ToolChoiceRequired})["tool_choice"]; got != "required" {
		t.Errorf("required = %#v, want the bare string \"required\"", got)
	}

	named := tcBody(t, providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"})["tool_choice"]
	obj, ok := named.(map[string]any)
	if !ok {
		t.Fatalf("named = %#v, want an object", named)
	}
	if obj["type"] != "function" {
		t.Errorf("named type = %#v, want \"function\"", obj["type"])
	}
	fn, ok := obj["function"].(map[string]any)
	if !ok {
		t.Fatalf("named form is FLAT (%#v) — that is the Responses API shape, and this driver "+
			"posts to /chat/completions, which wants {\"type\":\"function\",\"function\":{\"name\":…}}", obj)
	}
	if fn["name"] != "emit_state" {
		t.Errorf("named function.name = %#v, want \"emit_state\"", fn["name"])
	}
}

func TestToolChoice_OpenAIAutoSendsNothing(t *testing.T) {
	for _, tc := range []providers.ToolChoice{
		{},
		{Mode: providers.ToolChoiceAuto},
		{Mode: providers.ToolChoiceTool}, // no name → not a choice
	} {
		if _, present := tcBody(t, tc)["tool_choice"]; present {
			t.Errorf("%+v put a tool_choice on the wire; auto must send nothing", tc)
		}
	}
}
