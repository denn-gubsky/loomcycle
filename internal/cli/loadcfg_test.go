package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
	prov, model, _, err := cfg.ResolveAgentModel("probe")
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

// TestLoadLayeredConfig_OperatorLanesMergeOverBuiltinProvider: an operator entry
// that sets only the lane keys lands on the embedded built-in provider — the
// built-in's driver survives, and the max_concurrent shorthand caps the chat
// lane whose entry sets only tuning.
func TestLoadLayeredConfig_OperatorLanesMergeOverBuiltinProvider(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "")
	t.Setenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS", "")
	overlay := filepath.Join(t.TempDir(), "overlay.yaml")
	src := "providers:\n" +
		"  ollama-local:\n" +
		"    max_concurrent: 4\n" +
		"    max_models_in_flight: 1\n" +
		"    lanes:\n" +
		"      chat: { quantum_calls: 4 }\n" +
		"      embedder: { max_concurrent: 2, background_share: 0.25 }\n"
	if err := os.WriteFile(overlay, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLayeredConfig(overlay)
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	pc := cfg.Providers["ollama-local"]
	if pc.Driver != "ollama" {
		t.Errorf("driver = %q, want the built-in's ollama", pc.Driver)
	}
	want := map[string]config.ProviderLane{
		config.ModelKindChat:     {MaxConcurrent: 4, QuantumCalls: 4},
		config.ModelKindEmbedder: {MaxConcurrent: 2, BackgroundShare: 0.25},
	}
	if got := pc.EffectiveLanes(); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveLanes() = %+v, want %+v", got, want)
	}
	if got := pc.ModelSwitchMaxWaitMSOrDefault(); got != 30000 {
		t.Errorf("ModelSwitchMaxWaitMSOrDefault() = %d, want 30000", got)
	}
}
