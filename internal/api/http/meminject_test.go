package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
)

func labels(bs []config.CoreBlock) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Scope+"/"+b.Label)
	}
	return out
}

func has(bs []config.CoreBlock, scope, label string) bool {
	for _, b := range bs {
		if b.Scope == scope && b.Label == label {
			return true
		}
	}
	return false
}

// TestCoreBlock_NotInheritedByDefaultSubAgent pins RFC BL P1 spawn-tree rules:
// a sub-agent whose def leaves inherit_core_blocks unset (default false) gets
// ONLY its own declared blocks — the parent run's user/tenant blocks do not
// flow. With inherit_core_blocks:true it additionally receives the parent's
// user/tenant blocks, but NEVER the parent's agent-scope block.
func TestCoreBlock_NotInheritedByDefaultSubAgent(t *testing.T) {
	own := []config.CoreBlock{{Label: "notes", Scope: "agent"}}
	parent := []config.CoreBlock{
		{Label: "human", Scope: "user"},    // inheritable
		{Label: "policy", Scope: "tenant"}, // inheritable
		{Label: "secret", Scope: "agent"},  // NEVER inherited
	}

	// Default: no inheritance.
	got := effectiveCoreBlocks(own, false, parent)
	if len(got) != 1 || !has(got, "agent", "notes") {
		t.Fatalf("default sub-agent should get only its own blocks, got %v", labels(got))
	}
	if has(got, "user", "human") || has(got, "tenant", "policy") {
		t.Errorf("parent blocks leaked into a non-inheriting sub-agent: %v", labels(got))
	}

	// Opt-in: parent's user/tenant blocks flow; parent's agent block does not.
	got = effectiveCoreBlocks(own, true, parent)
	if !has(got, "agent", "notes") || !has(got, "user", "human") || !has(got, "tenant", "policy") {
		t.Errorf("inherit_core_blocks should add parent user/tenant blocks: %v", labels(got))
	}
	if has(got, "agent", "secret") {
		t.Errorf("parent AGENT-scope block must never be inherited: %v", labels(got))
	}
}

// TestEffectiveCoreBlocks_OwnWinsOnCollision pins that the agent's own block
// wins over an inherited block with the same (scope,label).
func TestEffectiveCoreBlocks_OwnWinsOnCollision(t *testing.T) {
	own := []config.CoreBlock{{Label: "human", Scope: "user", ReadOnly: true}}
	parent := []config.CoreBlock{{Label: "human", Scope: "user", ReadOnly: false}}
	got := effectiveCoreBlocks(own, true, parent)
	if len(got) != 1 {
		t.Fatalf("collision should collapse to one block, got %v", labels(got))
	}
	if !got[0].ReadOnly {
		t.Errorf("own block (read_only) should win over inherited: %+v", got[0])
	}
}

// TestEffectiveCoreBlocks_EmptyIsNil keeps a no-blocks agent byte-clean (nil,
// not an empty slice) so the fast path + policy stay zero-cost.
func TestEffectiveCoreBlocks_EmptyIsNil(t *testing.T) {
	if got := effectiveCoreBlocks(nil, true, nil); got != nil {
		t.Errorf("expected nil for no blocks, got %v", got)
	}
}

// TestFirstUserText extracts the initial user input used for search_request.
func TestFirstUserText(t *testing.T) {
	segs := []loop.PromptSegment{
		{Role: "system", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "sys"}}},
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "text", Text: "find my "}, {Type: "text", Text: "prefs"}}},
	}
	if got := firstUserText(segs); got != "find my  prefs" {
		t.Errorf("firstUserText = %q", got)
	}
	if got := firstUserText(nil); got != "" {
		t.Errorf("firstUserText(nil) = %q, want empty", got)
	}
}
