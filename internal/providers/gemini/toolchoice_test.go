package gemini

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func tcConfig(t *testing.T, tc providers.ToolChoice) (map[string]any, bool) {
	t.Helper()
	b, err := buildRequestBody(providers.Request{
		Model:    "gemini-2.5-pro",
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
	raw, present := m["toolConfig"]
	if !present {
		return nil, false
	}
	cfg, _ := raw.(map[string]any)
	fcc, _ := cfg["functionCallingConfig"].(map[string]any)
	return fcc, true
}

// ⚠️ GEMINI HAS NO "CALL EXACTLY THIS ONE" MODE. A single forced tool is ANY
// narrowed by a one-element allowedFunctionNames, and the constraint lives in
// its own toolConfig object rather than beside `tools` — two differences from
// every other driver, which is why this assertion is not shared with them.
func TestToolChoice_GeminiWireShapes(t *testing.T) {
	fcc, ok := tcConfig(t, providers.ToolChoice{Mode: providers.ToolChoiceNone})
	if !ok || fcc["mode"] != "NONE" {
		t.Errorf("none → %#v, want mode NONE", fcc)
	}
	fcc, ok = tcConfig(t, providers.ToolChoice{Mode: providers.ToolChoiceRequired})
	if !ok || fcc["mode"] != "ANY" {
		t.Errorf("required → %#v, want mode ANY", fcc)
	}
	if _, has := fcc["allowedFunctionNames"]; has {
		t.Errorf("required must not narrow the allow-list: %#v", fcc)
	}

	fcc, ok = tcConfig(t, providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"})
	if !ok || fcc["mode"] != "ANY" {
		t.Fatalf("named → %#v, want mode ANY", fcc)
	}
	names, _ := fcc["allowedFunctionNames"].([]any)
	if len(names) != 1 || names[0] != "emit_state" {
		t.Errorf("named allowedFunctionNames = %#v, want [emit_state]", fcc["allowedFunctionNames"])
	}
}

func TestToolChoice_GeminiAutoSendsNothing(t *testing.T) {
	for _, tc := range []providers.ToolChoice{
		{}, {Mode: providers.ToolChoiceAuto}, {Mode: providers.ToolChoiceTool},
	} {
		if _, present := tcConfig(t, tc); present {
			t.Errorf("%+v put a toolConfig on the wire; auto must send nothing", tc)
		}
	}
}
