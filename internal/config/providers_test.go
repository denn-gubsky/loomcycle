package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestProviders_UnmarshalYAML locks the RFC BF `providers:` block round-trip:
// every field (driver/dialect/base_url/api_key_env/max_concurrent/options/
// capabilities) parses into ProviderConfig, and the pointer capability fields
// distinguish an explicit false from an unset field.
func TestProviders_UnmarshalYAML(t *testing.T) {
	var doc struct {
		Providers map[string]ProviderConfig `yaml:"providers"`
	}
	src := "providers:\n" +
		"  ollama-local:\n" +
		"    driver: ollama\n" +
		"    dialect: ollama-chat\n" +
		"    base_url: http://localhost:11434\n" +
		"    max_concurrent: 4\n" +
		"    options:\n" +
		"      num_ctx: 131072\n" +
		"      num_gpu: 99\n" +
		"    capabilities:\n" +
		"      supports_vision: false\n" +
		"      max_context_tokens: 131072\n" +
		"  anthropic:\n" +
		"    driver: anthropic\n" +
		"    api_key_env: ANTHROPIC_API_KEY\n"
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	ol, ok := doc.Providers["ollama-local"]
	if !ok {
		t.Fatalf("ollama-local entry missing (%+v)", doc.Providers)
	}
	if ol.Driver != "ollama" || ol.Dialect != "ollama-chat" || ol.BaseURL != "http://localhost:11434" {
		t.Errorf("ollama-local core fields = %+v", ol)
	}
	if ol.MaxConcurrent != 4 {
		t.Errorf("max_concurrent = %d, want 4", ol.MaxConcurrent)
	}
	if n, ok := ol.Options["num_ctx"]; !ok || n != 131072 {
		t.Errorf("options.num_ctx = %v (%T), want 131072", n, n)
	}
	if ol.Capabilities == nil {
		t.Fatal("capabilities block did not parse")
	}
	// Explicit false must be a non-nil *bool pointing at false (distinct from
	// unset) — this is the load-bearing reason the override fields are pointers.
	if ol.Capabilities.SupportsVision == nil || *ol.Capabilities.SupportsVision != false {
		t.Errorf("supports_vision = %v, want a non-nil *false", ol.Capabilities.SupportsVision)
	}
	if ol.Capabilities.SupportsThinking != nil {
		t.Errorf("supports_thinking should be nil (unset), got %v", *ol.Capabilities.SupportsThinking)
	}
	if ol.Capabilities.MaxContextTokens == nil || *ol.Capabilities.MaxContextTokens != 131072 {
		t.Errorf("max_context_tokens = %v, want *131072", ol.Capabilities.MaxContextTokens)
	}

	an := doc.Providers["anthropic"]
	if an.Driver != "anthropic" || an.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic entry = %+v", an)
	}
}

// TestValidate_Providers covers the P1 light validation: a good block passes,
// an empty driver or negative max_concurrent fails, and an absent block never
// changes the outcome (every existing config validates identically).
func TestValidate_Providers(t *testing.T) {
	base := func(p map[string]ProviderConfig) *Config {
		return &Config{
			Defaults:    Defaults{Provider: "anthropic", Model: "x"},
			Concurrency: Concurrency{MaxConcurrentRuns: 1},
			Providers:   p,
		}
	}

	if err := validate(base(map[string]ProviderConfig{
		"anthropic": {Driver: "anthropic"},
		"ol":        {Driver: "ollama", MaxConcurrent: 8, Options: map[string]any{"num_ctx": 131072}},
	})); err != nil {
		t.Errorf("valid providers block rejected: %v", err)
	}

	// Absence is a no-op: nil map validates exactly like a config without the key.
	if err := validate(base(nil)); err != nil {
		t.Errorf("absent providers block should validate: %v", err)
	}

	cases := []struct {
		name    string
		cfg     *Config
		wantSub string
	}{
		{"empty driver",
			base(map[string]ProviderConfig{"bad": {Driver: ""}}), "driver is required"},
		{"negative max_concurrent",
			base(map[string]ProviderConfig{"bad": {Driver: "openai", MaxConcurrent: -1}}), "max_concurrent must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("got %v, want error containing %q", err, tc.wantSub)
			}
		})
	}
}
