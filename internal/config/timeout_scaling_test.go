package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const scalingBaseYAML = `
models:
  local-medium: {provider: ollama-local, model: "qwen3.6:latest"}
agents:
  a:
    provider: anthropic
    model: claude-haiku-4-5
`

func TestTimeoutScaling_DefaultsToMeasureWithTheReferenceMachine(t *testing.T) {
	cfg, err := loadYAMLString(t, scalingBaseYAML)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.TimeoutScaling
	want := TimeoutScaling{Mode: "measure", Reference: TimeoutScalingReference{DecodeTPS: 100, PrefillTPS: 2000, TTFTMs: 1000},
		MaxMultiplier: 8, MinSamples: 5, LocalPrior: 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("timeout_scaling = %+v, want %+v", got, want)
	}
}

func TestTimeoutScaling_EnvOverridesTheYAML(t *testing.T) {
	t.Setenv("LOOMCYCLE_TIMEOUT_SCALING", "OFF")
	t.Setenv("LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", "50")
	t.Setenv("LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER", "nope")
	cfg, err := loadYAMLString(t, scalingBaseYAML+"timeout_scaling: {mode: measure, max_multiplier: 6}\n")
	if err != nil {
		t.Fatal(err)
	}
	ts := cfg.TimeoutScaling
	if ts.Mode != "off" || ts.Reference.DecodeTPS != 50 || ts.MaxMultiplier != 6 {
		t.Fatalf("timeout_scaling = %+v, want mode off, decode 50 (env), max 6 (yaml; the bad env value ignored)", ts)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER") {
		t.Errorf("an unparseable env value was ignored silently; warnings: %v", cfg.Warnings)
	}
}

func TestTimeoutScaling_RefusesWhatItCannotMean(t *testing.T) {
	for name, block := range map[string]string{
		"on is not available yet":    "timeout_scaling: {mode: on}",
		"unknown mode":               "timeout_scaling: {mode: sometimes}",
		"max below 1":                "timeout_scaling: {max_multiplier: 0.5}",
		"prior above the max":        "timeout_scaling: {max_multiplier: 3, local_prior: 4}",
		"override names no model":    "timeout_scaling: {models: {just-a-name: {multiplier: 2}}}",
		"override sets both":         `timeout_scaling: {models: {"ollama-local/qwen3.6": {multiplier: 2, decode_tps: 20}}}`,
		"override sets neither":      `timeout_scaling: {models: {"ollama-local/qwen3.6": {}}}`,
		"override above the ceiling": `timeout_scaling: {models: {"ollama-local/qwen3.6": {multiplier: 9}}}`,
		"model cap below 1":          `timeout_scaling: {models: {"ollama-local/qwen3.6": {max_multiplier: 0.5}}}`,
		"model cap above 100":        `timeout_scaling: {models: {"ollama-local/qwen3.6": {max_multiplier: 101}}}`,
		"override above its own cap": `timeout_scaling: {models: {"ollama-local/qwen3.6": {multiplier: 30, max_multiplier: 24}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAMLString(t, scalingBaseYAML+block+"\n"); err == nil || !strings.Contains(err.Error(), "timeout_scaling") {
				t.Fatalf("err = %v, want a timeout_scaling refusal", err)
			}
		})
	}
}

func TestTimeoutScaling_OverrideKeysAcceptAModelsAlias(t *testing.T) {
	cfg, err := loadYAMLString(t, scalingBaseYAML+`timeout_scaling:
  models:
    local-medium: {decode_tps: 20}
    "ollama-local/hf.co/org/model:q4": {multiplier: 6}
`)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][2]string{
		"local-medium":                    {"ollama-local", "qwen3.6:latest"},
		"ollama-local/hf.co/org/model:q4": {"ollama-local", "hf.co/org/model:q4"},
	} {
		p, m, ok := cfg.ResolveModelKey(key)
		if !ok || p != want[0] || m != want[1] {
			t.Errorf("ResolveModelKey(%q) = %q, %q, %v; want %v", key, p, m, ok, want)
		}
	}
}

// A per-model cap may stand alone (a bound, not a prior), lifts that model's
// override ceiling past the global cap, and is keyed like any override.
func TestTimeoutScaling_AModelsOwnCapAcceptedAloneAndLiftsItsOverrideCeiling(t *testing.T) {
	cfg, err := loadYAMLString(t, scalingBaseYAML+`timeout_scaling:
  models:
    local-medium: {max_multiplier: 24}
    "ollama-local/qwen3.8:latest": {multiplier: 20, max_multiplier: 24}
`)
	if err != nil {
		t.Fatal(err)
	}
	ts := cfg.TimeoutScaling
	if got := ts.CapFor(ts.Models["local-medium"]); got != 24 {
		t.Errorf("cap for local-medium = %v, want its own 24", got)
	}
	if got := ts.CapFor(TimeoutScalingModel{DecodeTPS: 20}); got != 8 {
		t.Errorf("cap for an entry with no max_multiplier = %v, want the global 8", got)
	}
}

func TestConfig_ConcreteModelResolvesThisProvidersAliasOnly(t *testing.T) {
	cfg := &Config{Models: map[string]ModelRef{
		"deepseek-flash": {Provider: "deepseek", Model: "deepseek-v4-flash"},
		"any-fast":       {Model: "fast-1"},
		"haiku":          {Provider: "anthropic", ModelPattern: "claude-haiku-*"},
	}}
	for _, c := range []struct{ provider, model, want string }{
		{"deepseek", "deepseek-flash", "deepseek-v4-flash"}, // this provider's alias
		{"deepseek", "deepseek-v4-flash", "deepseek-v4-flash"},
		{"ollama", "deepseek-flash", "deepseek-flash"}, // another provider's alias: not its model
		{"ollama", "any-fast", "fast-1"},               // an alias naming no provider
		{"anthropic", "haiku", "haiku"},                // a pattern: needs the live catalog
		{"openai", "gpt-4o", "gpt-4o"},
	} {
		if got := cfg.ConcreteModel(c.provider, c.model); got != c.want {
			t.Errorf("ConcreteModel(%q, %q) = %q, want %q", c.provider, c.model, got, c.want)
		}
	}
}

// TestTimeoutScaling_MeasureModeLeavesEveryOtherSettingUnchanged pins the
// promise of this version: the block measures and reports, and NO timeout — or
// anything else the config resolves — differs because it is there.
func TestTimeoutScaling_MeasureModeLeavesEveryOtherSettingUnchanged(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	without, err := Load(write("a.yaml", scalingBaseYAML))
	if err != nil {
		t.Fatal(err)
	}
	with, err := Load(write("b.yaml", scalingBaseYAML+`timeout_scaling:
  mode: measure
  reference: {decode_tps: 20, prefill_tps: 300, ttft_ms: 5000}
  max_multiplier: 8
  local_prior: 6
  models:
    local-medium: {decode_tps: 10}
`))
	if err != nil {
		t.Fatal(err)
	}
	if with.TimeoutScaling.Reference.DecodeTPS != 20 {
		t.Fatalf("the block did not load: %+v", with.TimeoutScaling)
	}
	with.TimeoutScaling, without.TimeoutScaling = TimeoutScaling{}, TimeoutScaling{}
	if !reflect.DeepEqual(with.Env, without.Env) {
		t.Fatal("a timeout_scaling block changed the env-derived settings (every operator timeout lives there)")
	}
	if !reflect.DeepEqual(with, without) {
		t.Fatal("a timeout_scaling block changed a setting outside its own block")
	}
}
