package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// TestConvertConfigCandidates_ExpandsAlias locks the resolver-boundary
// expansion for per-agent / user-tier candidates: a candidate whose model is
// an alias in the top-level models: map is rewritten to the alias's concrete
// provider/model before it reaches the resolver (which matches literal model
// strings). A literal (non-alias) candidate passes through unchanged.
func TestConvertConfigCandidates_ExpandsAlias(t *testing.T) {
	models := map[string]config.ModelRef{
		"local-gemma": {Provider: "ollama-local", Model: "gemma4:max"},
	}
	in := map[string][]config.TierCandidate{
		"low": {
			{Provider: "ollama-local", Model: "local-gemma"},   // alias
			{Provider: "anthropic", Model: "claude-haiku-4-5"}, // literal
		},
	}
	got := convertConfigCandidates(in, models)
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

// TestResolveAgentDef_TierCandidateAliasExpands is the end-to-end repro of the
// 503 "no provider available for requested tier": a tier-based agent whose
// candidate names a models: alias must resolve to the alias's concrete
// provider/model. Fail-before: drop the ExpandModelAlias call in
// convertConfigCandidates and the candidate model stays "local-gemma", which
// the resolver never marks available → this test fails with a resolve error.
func TestResolveAgentDef_TierCandidateAliasExpands(t *testing.T) {
	r := resolve.NewResolver([]string{"ollama-local"}, nil)
	// Only the concrete model is ever reachable — the alias name is not.
	r.SetReachable("ollama-local", true, []string{"gemma4:max"}, "")

	s := minimalServerWithResolver(t, r)
	s.cfg.Models = map[string]config.ModelRef{
		"local-gemma": {Provider: "ollama-local", Model: "gemma4:max"},
	}

	def := config.AgentDef{
		Tier: "low",
		Models: map[string][]config.TierCandidate{
			"low": {{Provider: "ollama-local", Model: "local-gemma"}},
		},
	}
	prov, model, _, err := s.resolveAgentDef(context.Background(), def, "", "", "code-reviewer", "", false)
	if err != nil {
		t.Fatalf("resolveAgentDef returned error: %v", err)
	}
	if prov != "ollama-local" || model != "gemma4:max" {
		t.Fatalf("resolved (%q, %q), want (ollama-local, gemma4:max)", prov, model)
	}
}
