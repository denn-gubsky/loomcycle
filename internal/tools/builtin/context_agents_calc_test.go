package builtin

import (
	"encoding/json"
	"testing"
)

// RFC GY P1 PR 9a: Context op=agents counts the tools a run holds, Calc included.
func TestContextTool_AgentsCountsTheEffectiveTools(t *testing.T) {
	tool, _, ctx, agentName, _, _ := substrateFixture(t) // agentName: [Read Context]
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"agents","prefix":"researcher_"}`))
	if res.IsError {
		t.Fatalf("agents: %s", res.Text)
	}
	first := decodeResult(t, res.Text)["agents"].([]any)[0].(map[string]any)
	if first["name"] != agentName || first["tools_count"] != float64(3) {
		t.Errorf("agent %v tools_count = %v, want 3 (Read, Context, Calc)", first["name"], first["tools_count"])
	}
}
