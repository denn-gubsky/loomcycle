package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSpawnSchemas_AdmissionTextStatesTheQueueBound pins RFC GK D6: both spawn
// tools tell the model that "wait" is bounded per user and that a refusal
// names the bound. The text once said a waiting run "never times out" and
// nothing about the bound, and a 30-run batch lost six children to it.
func TestSpawnSchemas_AdmissionTextStatesTheQueueBound(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		props func(*testing.T, map[string]json.RawMessage) map[string]json.RawMessage
	}{
		{"spawn_run", topLevelProperties},
		{"spawn_runs", childSpecProperties},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			raw, ok := tc.props(t, schemaObject(t, toolInputSchema(t, tc.tool)))["admission"]
			if !ok {
				t.Fatal("no admission property")
			}
			var p struct {
				Description string `json:"description"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"max_queued_runs_per_user", "queue_capacity", "queued_now"} {
				if !strings.Contains(p.Description, want) {
					t.Errorf("admission description does not mention %s: %q", want, p.Description)
				}
			}
			if strings.Contains(p.Description, "never times out") {
				t.Errorf("admission description still promises a wait that never refuses: %q", p.Description)
			}
		})
	}
}
