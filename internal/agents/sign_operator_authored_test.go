package agents

import (
	"reflect"
	"strings"
	"testing"
)

// TestSign_AuthorshipIsNotContent pins the promise the operator_authored
// migration was written under: adding the flag must not move a single existing
// agent's content hash.
//
// The hash is what a fork verifies across deployments, so a change would make
// every stored agent report as drifted the moment the column landed — and an
// operator would have no way to tell a real edit from the migration.
//
// The literal was computed on origin/main BEFORE this change and re-computed
// after it — identical both times. It is the hash of this exact content. If a change to AgentContent
// moves it, that is a real fork of EVERY existing row and the diff must say so.
// The number is the assertion, not decoration.
func TestSign_AuthorshipIsNotContent(t *testing.T) {
	const want = "sha256:0d5ea67cfbfa7a386e9b8eedcd3739a7cc779b8f62f5be9121787a55d076519d"
	got := Sign(AgentContent{Name: "reviewer", SystemPrompt: "You review.", Tier: "middle"})
	if got != want {
		t.Errorf("the agent content hash MOVED:\n  want %s\n  got  %s\n"+
			"If this was deliberate, every existing agent_defs row now reports as drifted — "+
			"say so in the change and update this literal. If it was not, something joined "+
			"the hash input that should not have.", want, got)
	}
}

// TestAgentContent_CarriesNoAuthorshipField: the hash input is a WHITELIST, and
// authorship must never join it. Derived from the struct so a future field
// named for authorship reds here rather than silently forking every row.
func TestAgentContent_CarriesNoAuthorshipField(t *testing.T) {
	rt := reflect.TypeOf(AgentContent{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if strings.Contains(strings.ToLower(name), "author") || strings.Contains(strings.ToLower(name), "operator") {
			t.Errorf("AgentContent declares %q — authorship is AUTHORITY, not content; "+
				"hashing it would fork every existing agent row", name)
		}
	}
}
