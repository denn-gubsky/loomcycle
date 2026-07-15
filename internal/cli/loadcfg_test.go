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

// TestLoadLayeredConfig_PrependsDefaultProviders is the RFC BF P3 CLI/server-parity
// guard: a --config that references a built-in provider (anthropic) with NO
// providers: block validates through the CLI loader because loadLayeredConfig
// prepends the embedded default-providers layer — exactly as cmd/loomcycle/main.go
// does. P3 deleted the hardcoded validation floor, so without the prepend the CLI
// would falsely reject a config the server runs fine.
//
// Fail-before: drop the default-providers prepend in loadLayeredConfig → the first
// load fails "unknown provider anthropic".
func TestLoadLayeredConfig_PrependsDefaultProviders(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(cfgPath, []byte("provider_priority: [anthropic]\nagents:\n  a: { provider: anthropic, model: claude }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLayeredConfig(cfgPath); err != nil {
		t.Fatalf("a built-in ref without a providers: block was rejected — the default-providers layer isn't prepended: %v", err)
	}

	// LOOMCYCLE_NO_DEFAULT_PROVIDERS drops the layer → the undeclared built-in ref
	// must now fail validation (the operator opted into full provider control).
	t.Setenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS", "1")
	if _, err := loadLayeredConfig(cfgPath); err == nil {
		t.Error("with LOOMCYCLE_NO_DEFAULT_PROVIDERS=1 an undeclared built-in ref must fail validation")
	}
}
