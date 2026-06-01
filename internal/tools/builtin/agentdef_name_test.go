package builtin

import (
	"encoding/json"
	"testing"
)

// TestAgentDefTool_RejectsTraversalName pins the substrate name floor that
// keeps a code-js agent name (a path segment: agent_code/<name>/index.js)
// from escaping CodeRoot. Regression-grade — the fixture grants scope ["any"]
// so on the unfixed create/fork (no character validation) these names were
// accepted and persisted.
func TestAgentDefTool_RejectsTraversalName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	bad := []string{"../x", "../../etc/cron.d/x", "a/b", `a\b`, "..", ".", "has space", "name!"}
	for _, n := range bad {
		body, _ := json.Marshal(map[string]any{"op": "create", "name": n, "overlay": map[string]any{"provider": "code-js"}})
		res, _ := tool.Execute(ctx, json.RawMessage(body))
		if !res.IsError {
			t.Errorf("create name=%q was accepted; want a name-validation refusal", n)
		}
		bodyF, _ := json.Marshal(map[string]any{"op": "fork", "name": n, "overlay": map[string]any{}})
		resF, _ := tool.Execute(ctx, json.RawMessage(bodyF))
		if !resF.IsError {
			t.Errorf("fork name=%q was accepted; want a name-validation refusal", n)
		}
	}
	// A valid name is still accepted (no false positive).
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"good_agent-1","overlay":{"provider":"openai","system_prompt":"x"}}`))
	if res.IsError {
		t.Errorf("create with a valid name was wrongly refused: %s", res.Text)
	}
}
