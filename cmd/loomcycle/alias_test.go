package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// TestConvertTiers_ExpandsAlias locks the resolver-boundary expansion for the
// library tiers: fallback (cfg.Tiers). A candidate whose model is an alias in
// the top-level models: map is rewritten to its concrete provider/model
// before the resolver sees it; a literal candidate is unchanged.
func TestConvertTiers_ExpandsAlias(t *testing.T) {
	models := map[string]config.ModelRef{
		"local-gemma": {Provider: "ollama-local", Model: "gemma4:max"},
	}
	in := map[string][]config.TierCandidate{
		"low": {
			{Provider: "ollama-local", Model: "local-gemma"},   // alias
			{Provider: "anthropic", Model: "claude-haiku-4-5"}, // literal
		},
	}
	got := convertTiers(in, models)
	want := []resolve.Candidate{
		{Provider: "ollama-local", Model: "gemma4:max"},
		{Provider: "anthropic", Model: "claude-haiku-4-5"},
	}
	low := got["low"]
	if len(low) != len(want) {
		t.Fatalf("low candidates = %d, want %d (%+v)", len(low), len(want), low)
	}
	for i := range want {
		if low[i] != want[i] {
			t.Errorf("candidate[%d] = %+v, want %+v", i, low[i], want[i])
		}
	}
}
