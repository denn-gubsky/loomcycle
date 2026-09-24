package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// What a Gemini model is told about the Agent tool: every operation, and no
// field required on every call. When Agent's schema was a top-level oneOf,
// this sanitizer flattened it to `op: ["spawn"]` with name, prompt, op, spawns
// and child_run_id all required.
func TestAgentSchema_ReachesGeminiWithEveryOperation(t *testing.T) {
	in := (&builtin.AgentTool{}).InputSchema()
	// Gemini's sanitizer always rewrites (it drops keywords Gemini rejects), so
	// what matters is what survives: the ops and the required list.
	out := sanitizeGeminiSchema(in)
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
		t.Errorf("op enum sent to Gemini = %q", got)
	}
	if len(s.Required) != 0 {
		t.Errorf("required sent to Gemini = %v; each op's fields are required only for that op", s.Required)
	}
}
