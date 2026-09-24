package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// What a Claude model is told about the Agent tool: every operation, and no
// field required on every call. When Agent's schema was a top-level oneOf,
// this sanitizer flattened it to `op: ["spawn"]` with name, prompt, op, spawns
// and child_run_id all required.
func TestAgentSchema_ReachesClaudeWithEveryOperation(t *testing.T) {
	in := (&builtin.AgentTool{}).InputSchema()
	out := sanitizeAnthropicToolSchema(in)
	if string(out) != string(in) {
		t.Errorf("the sanitizer rewrote Agent's schema; it should pass a flat schema through unchanged")
	}
	var s struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Properties.Op.Enum, ","); got != "spawn,parallel_spawn,open,send,poll,cancel,close" {
		t.Errorf("op enum sent to Claude = %q", got)
	}
	if len(s.Required) != 0 {
		t.Errorf("required sent to Claude = %v; each op's fields are required only for that op", s.Required)
	}
}
