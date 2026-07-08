package main

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// layersFor resolves embedded unit names to config layers, the same mapping
// main() does for LOOMCYCLE_PRESETS (kept in lockstep with selectPresetLayers).
func layersFor(t *testing.T, names ...string) []config.Layer {
	t.Helper()
	units, err := embedded.ResolveUnits(names)
	if err != nil {
		t.Fatalf("ResolveUnits(%v): %v", names, err)
	}
	layers := make([]config.Layer, len(units))
	for i, u := range units {
		layers[i] = config.Layer{Name: u.Name, Data: u.Data}
	}
	return layers
}

// TestEmbedded_DocumentAgentResolvesWithInlineSkills is the RFC AQ §7 Phase-1
// headline, updated for RFC BA on-demand skills: selecting `base,document-agent`
// registers doc/manager AND carries its four inline skills in cfg.Skills (the
// on-demand catalog) with NO LOOMCYCLE_SKILLS_ROOT — the bundle is a pure config
// layer. The bodies are loaded via the Skill tool at runtime, NOT baked into the
// prompt; the agent gets the auto-added Skill tool.
func TestEmbedded_DocumentAgentResolvesWithInlineSkills(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // no skills directory — inline only

	cfg, err := config.LoadLayers(layersFor(t, "base", "document-agent")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, document-agent): %v", err)
	}
	dm, ok := cfg.Agents["doc/manager"]
	if !ok {
		t.Fatalf("doc/manager not registered (agents: %v)", agentNames(cfg))
	}
	// All four skills are registered in the on-demand catalog (cfg.Skills),
	// with their bodies intact for the Skill tool to load.
	for _, name := range []string{"doc/semantic-chunking", "doc/edge-linking", "doc/restructuring", "doc/md-import"} {
		sk, ok := cfg.Skills[name]
		if !ok {
			t.Errorf("inline skill %q missing from cfg.Skills", name)
			continue
		}
		if strings.TrimSpace(sk.Body) == "" {
			t.Errorf("inline skill %q has an empty body", name)
		}
	}
	// On-demand: the skill bodies are NOT baked into the agent prompt.
	for _, marker := range []string{"Semantic chunking", "Edge linking", "Markdown import"} {
		if strings.Contains(dm.SystemPrompt, marker) {
			t.Errorf("skill body %q must not be baked into the prompt (RFC BA on-demand)", marker)
		}
	}
	// The agent's whitelist gets the auto-added Skill tool for on-demand loading.
	if !hasToolPreset(dm.Tools, "Skill") {
		t.Errorf("doc/manager should get the auto-added Skill tool; tools=%v", dm.Tools)
	}
	// base supplied the middle tier the agent declares.
	if _, ok := cfg.Tiers["middle"]; !ok {
		t.Errorf("base preset should supply the middle tier doc/manager needs")
	}
}

// TestEmbedded_OperatorOverridesBundleSkill: an operator's later inline skill of
// the same name wins (RFC AN merge-by-key) in the on-demand catalog cfg.Skills,
// so the override is just re-declaring the key — no skills root, no fork.
func TestEmbedded_OperatorOverridesBundleSkill(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")

	overlay := config.Layer{Name: "operator", Data: []byte(`
skills:
  doc/restructuring:
    tools: [Document]
    body: |
      OVERRIDDEN RESTRUCTURING BODY
`)}
	layers := append(layersFor(t, "base", "document-agent"), overlay)
	cfg, err := config.LoadLayers(layers...)
	if err != nil {
		t.Fatalf("LoadLayers with override: %v", err)
	}
	sk, ok := cfg.Skills["doc/restructuring"]
	if !ok {
		t.Fatalf("restructuring skill missing from cfg.Skills")
	}
	if !strings.Contains(sk.Body, "OVERRIDDEN RESTRUCTURING BODY") {
		t.Errorf("operator override of the restructuring skill did not win; body=%q", sk.Body)
	}
	if strings.Contains(sk.Body, "deliberately has no drag-edit") {
		t.Errorf("the original restructuring body should be gone after override; body=%q", sk.Body)
	}
}

// hasToolPreset reports whether tools contains name.
func hasToolPreset(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

// TestEmbedded_BundleAloneDegradesGracefully: document-agent WITHOUT base still
// registers doc/manager (no load error) — it's a registered-but-idle def absent a
// middle tier, per the RFC's graceful-degradation note.
func TestEmbedded_BundleAloneDegradesGracefully(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")

	cfg, err := config.LoadLayers(layersFor(t, "document-agent")...)
	if err != nil {
		t.Fatalf("LoadLayers(document-agent) alone should not error: %v", err)
	}
	if _, ok := cfg.Agents["doc/manager"]; !ok {
		t.Fatalf("doc/manager should still be registered without base")
	}
}

// TestSelectPresetNames: --preset flags override LOOMCYCLE_PRESETS; an
// unset/empty env yields no presets (the opt-in default); order is preserved.
func TestSelectPresetNames(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		env   string
		want  []string
	}{
		{"unset is opt-in none", nil, "", nil},
		{"env comma-split ordered", nil, "base, document-agent", []string{"base", "document-agent"}},
		{"env trims blanks", nil, " base , , local ", []string{"base", "local"}},
		{"flags override env", []string{"local"}, "base,document-agent", []string{"local"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectPresetNames(tc.flags, tc.env)
			if len(got) != len(tc.want) {
				t.Fatalf("selectPresetNames(%v, %q) = %v, want %v", tc.flags, tc.env, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestEmbedded_OAuthPrependsWithoutRestatement is the RFC AQ §3 headline: the
// one-provider-per-file oauth preset composes onto base via !prepend — OAuth on
// top of provider_priority AND each tier, with base's providers retained as
// fallback and NO restatement.
func TestEmbedded_OAuthPrependsWithoutRestatement(t *testing.T) {
	cfg, err := config.LoadLayers(layersFor(t, "base", "oauth")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, oauth): %v", err)
	}
	if len(cfg.ProviderPriority) == 0 || cfg.ProviderPriority[0] != "anthropic-oauth-dev" {
		t.Errorf("provider_priority[0] = %v, want anthropic-oauth-dev on top", cfg.ProviderPriority)
	}
	// base's matrix is retained (fallback) — the prepend didn't restate/replace it.
	for _, want := range []string{"deepseek", "openai", "anthropic"} {
		if !sliceHas(cfg.ProviderPriority, want) {
			t.Errorf("base provider %q dropped from provider_priority: %v", want, cfg.ProviderPriority)
		}
	}
	// Each tier has the OAuth alias on top (a bare alias parses to {Model: alias}).
	if len(cfg.Tiers["middle"]) == 0 || cfg.Tiers["middle"][0].Model != "oauth-sonnet" {
		t.Errorf("tiers.middle = %+v, want oauth-sonnet first", cfg.Tiers["middle"])
	}
	if len(cfg.Tiers["high"]) == 0 || cfg.Tiers["high"][0].Model != "oauth-opus" {
		t.Errorf("tiers.high = %+v, want oauth-opus first", cfg.Tiers["high"])
	}
}

// TestEmbedded_LocalPrepends: base + local puts ollama-local + the local tier
// candidates on top while keeping base's cloud providers as fallback.
func TestEmbedded_LocalPrepends(t *testing.T) {
	cfg, err := config.LoadLayers(layersFor(t, "base", "local")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, local): %v", err)
	}
	if len(cfg.ProviderPriority) == 0 || cfg.ProviderPriority[0] != "ollama-local" {
		t.Errorf("provider_priority[0] = %v, want ollama-local on top", cfg.ProviderPriority)
	}
	if !sliceHas(cfg.ProviderPriority, "anthropic") {
		t.Errorf("base cloud fallback dropped from provider_priority: %v", cfg.ProviderPriority)
	}
	if len(cfg.Tiers["low"]) == 0 || cfg.Tiers["low"][0].Model != "local-fast" {
		t.Errorf("tiers.low = %+v, want local-fast first", cfg.Tiers["low"])
	}
}

func sliceHas(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func agentNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		out = append(out, n)
	}
	return out
}
