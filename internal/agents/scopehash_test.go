package agents

import "testing"

// The content hash must not notice the scope defaults.
//
// memory_scopes / history_scope are content-identifying and carry `omitempty`,
// so an EMPTY list is absent from the hashed JSON. That is the whole reason the
// defaults resolve at policy-resolution time instead of being written into the
// definition: materialising `[]` → `["user"]` at config-load would change
// content_sha256 for every agent that never set the field, forking each of them
// on upgrade — a migration event disguised as a default.
//
// This pins the property directly. If someone later "simplifies" the design by
// defaulting in the loader, this is what stops it.
func TestScopeDefaults_ContentHashIgnoresAnAbsentScopeGate(t *testing.T) {
	base := func() *Agent {
		return &Agent{Name: "researcher", SystemPrompt: "hello", Tools: []string{"Memory", "History"}}
	}

	absent := Sign(FromYAMLAgent(base()))

	empty := base()
	empty.MemoryScopes = []string{}
	empty.HistoryScope = []string{}
	if got := Sign(FromYAMLAgent(empty)); got != absent {
		t.Errorf("an explicitly EMPTY scope list hashes differently from an absent one\n"+
			"  absent: %s\n  empty:  %s\nomitempty is supposed to make these identical; if it "+
			"no longer does, the defaults cannot stay invisible to the hash", absent, got)
	}

	// And the guard that matters: a POPULATED list must still change the hash,
	// or the assertion above is passing because nothing is hashed at all.
	populated := base()
	populated.MemoryScopes = []string{"user"}
	if got := Sign(FromYAMLAgent(populated)); got == absent {
		t.Fatalf("memory_scopes: [user] hashed the same as no memory_scopes at all (%s) — "+
			"the field has dropped out of the content hash, and this test is vacuous", got)
	}
}
