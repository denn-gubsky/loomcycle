package config

import (
	"strings"
	"testing"
)

func writeLayer(t *testing.T, name, body string) Layer {
	t.Helper()
	return Layer{Name: name, Data: []byte(body)}
}

const patternBase = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  local-medium: { provider: ollama-local, model_pattern: "qwen3.6*" }
agents:
  a: { tier: middle }
tiers:
  middle: [local-medium]
`

// TestModelAlias_ConcreteModelOverridesAPatternAcrossLayers is the operator path:
// a preset ships a glob alias, the operator pins an exact model in their own layer.
//
// Before the sibling-drop, the merged mapping carried BOTH keys and config load
// failed "exactly one of model or model_pattern is required" — an error naming
// neither layer, so an operator who wrote only `model:` had nothing to go on.
func TestModelAlias_ConcreteModelOverridesAPatternAcrossLayers(t *testing.T) {
	base := writeLayer(t, "base.yaml", patternBase)
	overlay := writeLayer(t, "overlay.yaml", `
models:
  local-medium: { provider: ollama-local, model: qwen3.6:latest }
`)
	cfg, err := LoadLayers(base, overlay)
	if err != nil {
		t.Fatalf("a later layer pinning an exact model must win, not conflict: %v", err)
	}
	ref := cfg.Models["local-medium"]
	if ref.Model != "qwen3.6:latest" {
		t.Errorf("model = %q, want the overlay's exact model", ref.Model)
	}
	if ref.ModelPattern != "" {
		t.Errorf("model_pattern = %q, want it cleared by the override", ref.ModelPattern)
	}
}

// TestModelAlias_PatternOverridesAConcreteModelAcrossLayers is the same rule in
// the other direction: a preset pins a tag, the operator wants whatever their
// Ollama actually serves.
func TestModelAlias_PatternOverridesAConcreteModelAcrossLayers(t *testing.T) {
	base := writeLayer(t, "base.yaml", `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  local-medium: { provider: ollama-local, model: qwen3.6:27b }
agents:
  a: { tier: middle }
tiers:
  middle: [local-medium]
`)
	overlay := writeLayer(t, "overlay.yaml", `
models:
  local-medium: { provider: ollama-local, model_pattern: "qwen3.6*" }
`)
	cfg, err := LoadLayers(base, overlay)
	if err != nil {
		t.Fatalf("a later layer supplying a pattern must win: %v", err)
	}
	ref := cfg.Models["local-medium"]
	if ref.ModelPattern != "qwen3.6*" {
		t.Errorf("model_pattern = %q, want the overlay's glob", ref.ModelPattern)
	}
	if ref.Model != "" {
		t.Errorf("model = %q, want it cleared by the override", ref.Model)
	}
}

// TestModelAlias_BothInOneLayerStillFails: the sibling-drop must not swallow a
// genuine single-file mistake. Only a CROSS-LAYER override is later-wins; one
// layer naming both halves is still an error, and validation must keep saying so.
func TestModelAlias_BothInOneLayerStillFails(t *testing.T) {
	single := writeLayer(t, "one.yaml", `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  local-medium: { provider: ollama-local, model: qwen3.6:27b, model_pattern: "qwen3.6*" }
agents:
  a: { tier: middle }
tiers:
  middle: [local-medium]
`)
	if _, err := LoadLayers(single); err == nil {
		t.Fatal("one layer naming both model and model_pattern must still fail validation")
	} else if !strings.Contains(err.Error(), "exactly one of model or model_pattern") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestModelAlias_OverrideDoesNotDisturbSiblingAliases guards the node surgery:
// removing a key from one alias's mapping must not shift or drop another's.
func TestModelAlias_OverrideDoesNotDisturbSiblingAliases(t *testing.T) {
	base := writeLayer(t, "base.yaml", `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  local-small:  { provider: ollama-local, model_pattern: "gemma4*" }
  local-medium: { provider: ollama-local, model_pattern: "qwen3.6*" }
  local-coder:  { provider: ollama-local, model_pattern: "qwen3-coder*" }
agents:
  a: { tier: middle }
tiers:
  middle: [local-medium]
`)
	overlay := writeLayer(t, "overlay.yaml", `
models:
  local-medium: { provider: ollama-local, model: qwen3.6:latest }
`)
	cfg, err := LoadLayers(base, overlay)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Models["local-small"].ModelPattern; got != "gemma4*" {
		t.Errorf("local-small pattern = %q, want it untouched", got)
	}
	if got := cfg.Models["local-coder"].ModelPattern; got != "qwen3-coder*" {
		t.Errorf("local-coder pattern = %q, want it untouched", got)
	}
	if got := cfg.Models["local-medium"].Model; got != "qwen3.6:latest" {
		t.Errorf("local-medium model = %q", got)
	}
	if cfg.Models["local-medium"].Provider != "ollama-local" {
		t.Errorf("local-medium lost its provider: %+v", cfg.Models["local-medium"])
	}
}
