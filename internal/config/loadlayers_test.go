package config

import (
	"testing"
)

// TestLoadLayers_InMemoryBaseMergesDiskOverlay: an in-memory base layer (RFC AQ
// embedded preset) composes with a disk overlay exactly like two files — the
// overlay wins per-key, the base supplies what the overlay omits.
func TestLoadLayers_InMemoryBaseMergesDiskOverlay(t *testing.T) {
	dir := t.TempDir()
	overlay := writeYAML(t, dir, "operator.yaml", `
provider_priority: [deepseek]
`)
	base := []byte(`
provider_priority: [anthropic, openai]
models:
  base-sonnet: { provider: anthropic, model: claude-sonnet-4-6 }
`)
	cfg, err := LoadLayers(Layer{Name: "base", Data: base}, Layer{Name: overlay})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	// Overlay wins on the keys it sets (a sequence replaces wholesale, RFC AN).
	if len(cfg.ProviderPriority) != 1 || cfg.ProviderPriority[0] != "deepseek" {
		t.Errorf("provider_priority = %v, want [deepseek] (overlay replaces)", cfg.ProviderPriority)
	}
	// Base supplies what the overlay omitted (the model alias merges by key).
	if _, ok := cfg.Models["base-sonnet"]; !ok {
		t.Errorf("models missing base-sonnet (base layer should contribute it): %v", cfg.Models)
	}
}

// TestLoadLayers_SingleInMemoryLayer: a presets-only stack (one in-memory layer,
// no disk file) resolves — the bare-start case (RFC AQ §2.2).
func TestLoadLayers_SingleInMemoryLayer(t *testing.T) {
	base := []byte(`
provider_priority: [anthropic]
models:
  base-sonnet: { provider: anthropic, model: claude-sonnet-4-6 }
`)
	cfg, err := LoadLayers(Layer{Name: "base", Data: base})
	if err != nil {
		t.Fatalf("LoadLayers (presets-only): %v", err)
	}
	if len(cfg.ProviderPriority) != 1 || cfg.ProviderPriority[0] != "anthropic" {
		t.Errorf("provider_priority = %v, want [anthropic]", cfg.ProviderPriority)
	}
	// No disk file → no configDir set (relative prompts resolve against cwd).
	if cfg.configDir != "" {
		t.Errorf("configDir = %q, want empty for a presets-only stack", cfg.configDir)
	}
}

// TestLoadLayers_SingleFileMatchesLoad: Load(path) and LoadLayers(Layer{Name:path})
// take the same byte-identical single-file fast path and produce the same result.
func TestLoadLayers_SingleFileMatchesLoad(t *testing.T) {
	dir := t.TempDir()
	p := writeYAML(t, dir, "loomcycle.yaml", `
provider_priority: [anthropic, openai]
`)
	viaLoad, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	viaLayers, err := LoadLayers(Layer{Name: p})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if len(viaLoad.ProviderPriority) != len(viaLayers.ProviderPriority) {
		t.Fatalf("provider_priority length mismatch: Load=%v LoadLayers=%v", viaLoad.ProviderPriority, viaLayers.ProviderPriority)
	}
	for i := range viaLoad.ProviderPriority {
		if viaLoad.ProviderPriority[i] != viaLayers.ProviderPriority[i] {
			t.Errorf("provider_priority[%d] mismatch: Load=%q LoadLayers=%q", i, viaLoad.ProviderPriority[i], viaLayers.ProviderPriority[i])
		}
	}
	// Both must set configDir from the file's directory (the fast path).
	if viaLoad.configDir == "" || viaLoad.configDir != viaLayers.configDir {
		t.Errorf("configDir mismatch: Load=%q LoadLayers=%q", viaLoad.configDir, viaLayers.configDir)
	}
}
