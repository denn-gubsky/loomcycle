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
// headline: selecting `base,document-agent` registers doc-manager AND bakes its
// four inline skills into the system prompt with NO LOOMCYCLE_SKILLS_ROOT set —
// the bundle is a pure config layer.
func TestEmbedded_DocumentAgentResolvesWithInlineSkills(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // no skills directory — inline only

	cfg, err := config.LoadLayers(layersFor(t, "base", "document-agent")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, document-agent): %v", err)
	}
	dm, ok := cfg.Agents["doc-manager"]
	if !ok {
		t.Fatalf("doc-manager not registered (agents: %v)", agentNames(cfg))
	}
	// All four skill bodies must be baked into the prompt.
	for _, marker := range []string{"Semantic chunking", "Edge linking", "Restructuring", "Markdown import"} {
		if !strings.Contains(dm.SystemPrompt, marker) {
			t.Errorf("doc-manager system prompt missing skill content %q", marker)
		}
	}
	// base supplied the middle tier the agent declares.
	if _, ok := cfg.Tiers["middle"]; !ok {
		t.Errorf("base preset should supply the middle tier doc-manager needs")
	}
}

// TestEmbedded_OperatorOverridesBundleSkill: an operator's later inline skill of
// the same name wins (RFC AN merge-by-key), so the override is just re-declaring
// the key — no skills root, no fork.
func TestEmbedded_OperatorOverridesBundleSkill(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")

	overlay := config.Layer{Name: "operator", Data: []byte(`
skills:
  restructuring:
    allowed_tools: [Document]
    body: |
      OVERRIDDEN RESTRUCTURING BODY
`)}
	layers := append(layersFor(t, "base", "document-agent"), overlay)
	cfg, err := config.LoadLayers(layers...)
	if err != nil {
		t.Fatalf("LoadLayers with override: %v", err)
	}
	dm := cfg.Agents["doc-manager"]
	if !strings.Contains(dm.SystemPrompt, "OVERRIDDEN RESTRUCTURING BODY") {
		t.Errorf("operator override of the restructuring skill did not win")
	}
	if strings.Contains(dm.SystemPrompt, "deliberately has no drag-edit") {
		t.Errorf("the original restructuring body should be gone after override")
	}
}

// TestEmbedded_BundleAloneDegradesGracefully: document-agent WITHOUT base still
// registers doc-manager (no load error) — it's a registered-but-idle def absent a
// middle tier, per the RFC's graceful-degradation note.
func TestEmbedded_BundleAloneDegradesGracefully(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")

	cfg, err := config.LoadLayers(layersFor(t, "document-agent")...)
	if err != nil {
		t.Fatalf("LoadLayers(document-agent) alone should not error: %v", err)
	}
	if _, ok := cfg.Agents["doc-manager"]; !ok {
		t.Fatalf("doc-manager should still be registered without base")
	}
}

func agentNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		out = append(out, n)
	}
	return out
}
