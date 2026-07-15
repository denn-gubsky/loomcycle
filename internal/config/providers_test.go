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
		// RFC BF P2a registry-aware checks (drivers_registry_test.go populates the
		// registry so these exercise the real registered factories).
		{"unknown driver",
			base(map[string]ProviderConfig{"bad": {Driver: "not-a-driver"}}), "unknown driver"},
		{"unsupported dialect",
			base(map[string]ProviderConfig{"bad": {Driver: "anthropic", Dialect: "openai-chat"}}), "does not speak dialect"},
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

// TestValidate_Providers_DialectAccepted proves a driver's canonical dialect is
// accepted when explicitly set (the counterpart to the "unsupported dialect"
// rejection above).
func TestValidate_Providers_DialectAccepted(t *testing.T) {
	cfg := &Config{
		Defaults:    Defaults{Provider: "anthropic", Model: "x"},
		Concurrency: Concurrency{MaxConcurrentRuns: 1},
		Providers: map[string]ProviderConfig{
			"anthropic": {Driver: "anthropic", Dialect: "anthropic-messages"},
		},
	}
	if err := validate(cfg); err != nil {
		t.Errorf("canonical dialect rejected: %v", err)
	}
}

// TestValidate_ThirdPartyProvider_AcceptedAsReference proves RFC BF P2a's headline
// capability: a config-declared 3rd-party provider id (not in the built-in floor)
// is a valid provider reference in tiers / agent pins / provider_priority, while a
// truly undeclared id is still rejected.
func TestValidate_ThirdPartyProvider_AcceptedAsReference(t *testing.T) {
	base := func() *Config {
		return &Config{
			Defaults:    Defaults{Provider: "anthropic", Model: "x"},
			Concurrency: Concurrency{MaxConcurrentRuns: 1},
			Providers: map[string]ProviderConfig{
				// A self-hosted OpenAI-compatible mirror declared by the operator.
				"my-vllm": {Driver: "openai", BaseURL: "http://vllm.local", APIKeyEnv: "MY_VLLM_KEY"},
			},
			ProviderPriority: []string{"my-vllm", "anthropic"},
			Tiers:            map[string][]TierCandidate{"middle": {{Provider: "my-vllm", Model: "qwen"}}},
			Agents:           map[string]AgentDef{"a": {Provider: "my-vllm", Model: "qwen"}},
		}
	}
	if err := validate(base()); err != nil {
		t.Errorf("declared 3rd-party provider rejected as reference: %v", err)
	}

	// An UNDECLARED id must still fail — the floor + declared set is the ceiling.
	bad := base()
	bad.ProviderPriority = []string{"ghost"}
	if err := validate(bad); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("undeclared provider accepted: %v", err)
	}
}

// TestValidate_ModelAliasProvider covers the new models[*].provider check: a
// built-in provider on an alias passes (floor), a declared 3rd-party passes, and
// a bogus provider is rejected. An empty-provider alias is left to the pin/tier
// that names it, so it must NOT be rejected here.
func TestValidate_ModelAliasProvider(t *testing.T) {
	base := func(models map[string]ModelRef, providers map[string]ProviderConfig) *Config {
		return &Config{
			Defaults:    Defaults{Provider: "anthropic", Model: "x"},
			Concurrency: Concurrency{MaxConcurrentRuns: 1},
			Models:      models,
			Providers:   providers,
		}
	}
	if err := validate(base(map[string]ModelRef{"a": {Provider: "anthropic", Model: "claude"}}, nil)); err != nil {
		t.Errorf("built-in alias provider rejected: %v", err)
	}
	if err := validate(base(
		map[string]ModelRef{"a": {Provider: "my-vllm", Model: "qwen"}},
		map[string]ProviderConfig{"my-vllm": {Driver: "openai", APIKeyEnv: "MY_VLLM_KEY"}},
	)); err != nil {
		t.Errorf("declared 3rd-party alias provider rejected: %v", err)
	}
	if err := validate(base(map[string]ModelRef{"a": {Model: "just-a-model"}}, nil)); err != nil {
		t.Errorf("empty-provider alias must not be validated here: %v", err)
	}
	err := validate(base(map[string]ModelRef{"a": {Provider: "bogus", Model: "x"}}, nil))
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("bogus alias provider accepted: %v", err)
	}
}
