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

	// Note: `a/b` is NO LONGER rejected — RFC BA agent grouping makes an interior
	// `/` a legal nested name (agent_code/a/b/index.js). The floor now rejects
	// only true escapes: `..`/`.` (whole or as a segment), backslashes,
	// leading/trailing/double slash, and invalid runes.
	bad := []string{"../x", "../../etc/cron.d/x", "a/../b", `a\b`, "..", ".", "/lead", "trail/", "a//b", "has space", "name!"}
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
	// A valid flat name is still accepted (no false positive).
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"good_agent-1","overlay":{"provider":"openai","system_prompt":"x"}}`))
	if res.IsError {
		t.Errorf("create with a valid name was wrongly refused: %s", res.Text)
	}
	// RFC BA: a `/`-grouped name is now accepted.
	resG, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"doc/manager","overlay":{"provider":"openai","system_prompt":"x"}}`))
	if resG.IsError {
		t.Errorf("create with a `/`-grouped name was wrongly refused: %s", resG.Text)
	}
}
