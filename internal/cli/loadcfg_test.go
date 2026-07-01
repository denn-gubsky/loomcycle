package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLayeredConfig_PresetAliasResolvesInOverlayAgent is the regression for
// the CLI-doesn't-layer-presets bug: an overlay agent whose model is a
// preset-defined alias (deepseek-pro, from the base preset) must resolve —
// matching the running server — instead of the old false "no provider
// resolved". Fail-before: with config.Load (single file) the base preset's
// models map is absent and ResolveAgentModel errors.
func TestLoadLayeredConfig_PresetAliasResolvesInOverlayAgent(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "base")
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(overlay, []byte("agents:\n  probe: { model: deepseek-pro }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLayeredConfig(overlay)
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	prov, model, err := cfg.ResolveAgentModel("probe")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if prov != "deepseek" || model != "deepseek-v4-pro" {
		t.Errorf("probe resolved to %s/%s, want deepseek/deepseek-v4-pro", prov, model)
	}
}

// TestLoadLayeredConfig_NoPresetsUnchanged pins that without LOOMCYCLE_PRESETS
// (and no CONFIG_* env), loading is byte-equivalent to the old single-file path:
// an alias defined nowhere stays unresolved. Guards against the layering
// silently changing behaviour for the common no-presets case.
func TestLoadLayeredConfig_NoPresetsUnchanged(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "")
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.yaml")
	// A self-contained agent (explicit provider+model) so the load succeeds;
	// then assert an UNDEFINED alias still fails to resolve.
	if err := os.WriteFile(overlay, []byte("agents:\n  probe: { provider: deepseek, model: deepseek-v4-pro }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLayeredConfig(overlay)
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	if _, ok := cfg.Models["deepseek-pro"]; ok {
		t.Error("deepseek-pro alias present without presets — layering leaked the base preset")
	}
}
