package lookup

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A definition stored before `descendants` was refused still resolves — the
// write refusal must not brick a running agent — and carries the value as
// written, which the scope checks then treat as authorising nothing.
func TestAgentFromDefRow_AStoredDescendantsGrantStillResolves(t *testing.T) {
	def, ok := AgentFromDefRow(store.AgentDefRow{Name: "old", Definition: []byte(`{"system_prompt":"x","agent_def_scopes":["descendants"],"schedule_def_scopes":["descendants"]}`)})
	if !ok {
		t.Fatal("a stored definition carrying descendants did not resolve")
	}
	if len(def.AgentDefScopes) != 1 || def.AgentDefScopes[0] != "descendants" {
		t.Errorf("agent_def_scopes = %v, want the stored value", def.AgentDefScopes)
	}
}
