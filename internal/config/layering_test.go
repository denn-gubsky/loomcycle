package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestMergeConfigFiles_Matrix exercises the one recursive merge rule across every
// YAML kind (RFC AN §3): mapping ⊕ mapping → merge keys; scalar/sequence → replace.
func TestMergeConfigFiles_Matrix(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", `
listen_addr: "127.0.0.1:8787"
provider_priority: [anthropic, openai]
defaults:
  model: base-model
  effort: low
agents:
  doc-manager:
    system_prompt: from-base
    tier: middle
`)
	over := writeYAML(t, dir, "over.yaml", `
listen_addr: "0.0.0.0:9000"
provider_priority: [deepseek]
defaults:
  model: over-model
agents:
  doc-manager:
    system_prompt: from-over
  code-guru:
    tier: high
`)
	merged, overrides, err := mergeConfigFiles([]string{base, over})
	if err != nil {
		t.Fatalf("mergeConfigFiles: %v", err)
	}

	// scalar → last wins.
	if merged["listen_addr"] != "0.0.0.0:9000" {
		t.Errorf("listen_addr = %v, want the later layer's value", merged["listen_addr"])
	}
	// sequence → replaced wholesale (NOT appended).
	pp, _ := merged["provider_priority"].([]any)
	if len(pp) != 1 || pp[0] != "deepseek" {
		t.Errorf("provider_priority = %v, want [deepseek] (sequences replace, not append)", merged["provider_priority"])
	}
	// struct mapping → field-by-field (model overridden, effort kept).
	d, _ := merged["defaults"].(map[string]any)
	if d["model"] != "over-model" {
		t.Errorf("defaults.model = %v, want over-model", d["model"])
	}
	if d["effort"] != "low" {
		t.Errorf("defaults.effort = %v, want low (kept from base — field merge)", d["effort"])
	}
	// mapping of entries → union by key; a same-named entry field-merges.
	ag, _ := merged["agents"].(map[string]any)
	if len(ag) != 2 {
		t.Fatalf("agents has %d entries, want 2 (union of doc-manager + code-guru)", len(ag))
	}
	dm, _ := ag["doc-manager"].(map[string]any)
	if dm["system_prompt"] != "from-over" {
		t.Errorf("doc-manager.system_prompt = %v, want from-over (overridden)", dm["system_prompt"])
	}
	if dm["tier"] != "middle" {
		t.Errorf("doc-manager.tier = %v, want middle (kept — field merge, matches mergeAgentDef)", dm["tier"])
	}
	if _, ok := ag["code-guru"]; !ok {
		t.Errorf("code-guru missing — a new key in a later layer must be added")
	}

	// Override records: exactly the REPLACED leaves — listen_addr,
	// provider_priority, defaults.model, agents.doc-manager.system_prompt.
	// NOT defaults.effort (kept), code-guru (new), doc-manager.tier (kept).
	for _, want := range []string{"listen_addr", "provider_priority", "defaults.model", "agents.doc-manager.system_prompt"} {
		if !containsSub(overrides, want) {
			t.Errorf("override for %q not recorded; got %v", want, overrides)
		}
	}
	for _, notWant := range []string{"defaults.effort", "code-guru", "doc-manager.tier"} {
		if containsSub(overrides, notWant) {
			t.Errorf("%q must NOT be an override (added/kept, not replaced); got %v", notWant, overrides)
		}
	}
}

// TestMergeConfigFiles_SameValueNotAConflict: re-setting a key to the SAME value
// across layers is not a conflict (no override record, no strict-mode trip).
func TestMergeConfigFiles_SameValueNotAConflict(t *testing.T) {
	dir := t.TempDir()
	a := writeYAML(t, dir, "a.yaml", "listen_addr: \"127.0.0.1:8787\"\n")
	b := writeYAML(t, dir, "b.yaml", "listen_addr: \"127.0.0.1:8787\"\n")
	_, overrides, err := mergeConfigFiles([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 0 {
		t.Errorf("identical value across layers must not record an override; got %v", overrides)
	}
}

// TestLoad_LayeringMotivatingCase is the RFC AN headline: a bundle file
// contributes its agent; the operator's file (LAST) wins on
// providers/tiers/defaults — so the bundle agent runs on the operator's routing.
func TestLoad_LayeringMotivatingCase(t *testing.T) {
	dir := t.TempDir()
	bundle := writeYAML(t, dir, "bundle.yaml", `
agents:
  doc-manager:
    system_prompt: manage documents
    tier: middle
`)
	operator := writeYAML(t, dir, "operator.yaml", `
defaults:
  model: op-model
provider_priority: [mock]
tiers:
  middle:
    - provider: mock
      model: mock-model
agents:
  code-guru:
    system_prompt: review code
`)
	cfg, err := Load(bundle, operator)
	if err != nil {
		t.Fatalf("Load(bundle, operator): %v", err)
	}
	if _, ok := cfg.Agents["doc-manager"]; !ok {
		t.Errorf("doc-manager (from the bundle) missing in the merged config")
	}
	if _, ok := cfg.Agents["code-guru"]; !ok {
		t.Errorf("code-guru (from the operator) missing in the merged config")
	}
	if cfg.Defaults.Model != "op-model" {
		t.Errorf("defaults.model = %q, want op-model (operator file last → wins)", cfg.Defaults.Model)
	}
	if len(cfg.ProviderPriority) != 1 || cfg.ProviderPriority[0] != "mock" {
		t.Errorf("provider_priority = %v, want [mock] (operator's)", cfg.ProviderPriority)
	}
}

// TestLoad_StrictModeFatalOnConflict: a cross-layer conflict is a FATAL load
// error under LOOMCYCLE_CONFIG_STRICT=1; without it, Load succeeds + warns
// (last-wins). The without-strict half is the fail-before for the gate.
func TestLoad_StrictModeFatalOnConflict(t *testing.T) {
	dir := t.TempDir()
	f1 := writeYAML(t, dir, "f1.yaml", "defaults:\n  model: m1\n")
	f2 := writeYAML(t, dir, "f2.yaml", "defaults:\n  model: m2\n")

	// Non-strict: succeeds, last wins, the override is surfaced as a warning.
	cfg, err := Load(f1, f2)
	if err != nil {
		t.Fatalf("non-strict Load: %v", err)
	}
	if cfg.Defaults.Model != "m2" {
		t.Errorf("defaults.model = %q, want m2 (last wins)", cfg.Defaults.Model)
	}
	if !containsSub(cfg.Warnings, "defaults.model") {
		t.Errorf("expected a layer-override warning for defaults.model; got %v", cfg.Warnings)
	}

	// Strict: the same conflict is fatal.
	t.Setenv("LOOMCYCLE_CONFIG_STRICT", "1")
	if _, err := Load(f1, f2); err == nil || !strings.Contains(err.Error(), "STRICT") {
		t.Errorf("strict Load err = %v, want a fatal STRICT conflict error", err)
	}
}

// TestLoad_PerLayerEnvExpansion: each layer's ${ENV} is expanded against its own
// raw text before merge — a later layer can't inject into an earlier one.
func TestLoad_PerLayerEnvExpansion(t *testing.T) {
	t.Setenv("LOOMCYCLE_TEST_PROVIDER", "mock")
	dir := t.TempDir()
	f1 := writeYAML(t, dir, "f1.yaml", "defaults:\n  model: m\n")
	f2 := writeYAML(t, dir, "f2.yaml", "provider_priority: [\"${LOOMCYCLE_TEST_PROVIDER}\"]\n")
	cfg, err := Load(f1, f2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ProviderPriority) != 1 || cfg.ProviderPriority[0] != "mock" {
		t.Errorf("provider_priority = %v, want [mock] (per-layer ${ENV} expanded)", cfg.ProviderPriority)
	}
}
