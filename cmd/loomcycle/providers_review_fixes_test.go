package main

import (
	"os"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestProviderEnabled_KeylessThirdParty is the RFC BF review-finding regression:
// a declared 3rd-party provider with NO api_key_env (a keyless self-hosted
// OpenAI-compatible endpoint — vLLM/llama.cpp behind base_url) must be ENABLED,
// because declaring it in `providers:` IS the opt-in. Before the fix,
// providerEnabled's default case required api_key_env, so a keyless provider was
// silently treated as "not configured" and could never be used.
func TestProviderEnabled_KeylessThirdParty(t *testing.T) {
	cfg := &config.Config{}
	if !providerEnabled("my-vllm", config.ProviderConfig{Driver: "openai", BaseURL: "http://vllm:8000"}, cfg) {
		t.Error("a keyless declared 3rd-party provider should be enabled")
	}
}

// TestProviderEnabled_KeyedUnsetDisabled confirms the other half stays
// byte-identical: a keyed provider whose api_key_env names an UNSET variable is
// disabled, exactly as the built-ins were pre-fix.
func TestProviderEnabled_KeyedUnsetDisabled(t *testing.T) {
	const env = "LOOMCYCLE_TEST_UNSET_PROVIDER_KEY"
	os.Unsetenv(env)
	cfg := &config.Config{}
	if providerEnabled("my-keyed", config.ProviderConfig{Driver: "openai", APIKeyEnv: env}, cfg) {
		t.Error("a keyed provider with an unset api_key_env should be disabled")
	}
}

// TestProviderEnabled_OllamaLocalYAMLBaseURL is the RFC CK regression: a
// `providers.ollama-local.base_url` set in YAML enables ollama-local even when
// the OLLAMA_BASE_URL env is empty, so a `local` preset/bundle can carry the
// endpoint without the env var. (The env still defaults to localhost, so an
// existing env deployment with an empty pc.BaseURL is unchanged.)
func TestProviderEnabled_OllamaLocalYAMLBaseURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.OllamaBaseURL = "" // simulate the env explicitly cleared
	if !providerEnabled("ollama-local", config.ProviderConfig{Driver: "ollama", BaseURL: "http://truenas.local:11434"}, cfg) {
		t.Error("ollama-local with a YAML base_url should be enabled even when the env is empty")
	}
	// No base_url anywhere → disabled (nothing tells it where the host is).
	if providerEnabled("ollama-local", config.ProviderConfig{Driver: "ollama"}, cfg) {
		t.Error("ollama-local with neither env nor YAML base_url should be disabled")
	}
}

// TestProviderEnabled_OllamaLocalDisabledSentinelWins confirms the opt-out is
// absolute: OLLAMA_BASE_URL=disabled disables ollama-local even if a YAML
// base_url is present.
func TestProviderEnabled_OllamaLocalDisabledSentinelWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.OllamaBaseURL = "disabled"
	if providerEnabled("ollama-local", config.ProviderConfig{Driver: "ollama", BaseURL: "http://truenas.local:11434"}, cfg) {
		t.Error("OLLAMA_BASE_URL=disabled must win over a YAML base_url")
	}
}

// TestNewProviderResolver_KeylessThirdPartyResolvable is the end-to-end #1
// regression: a keyless declared provider is constructed via the driver registry
// and resolvable through Get (before the fix it never entered byID, so Get
// returned "not configured").
func TestNewProviderResolver_KeylessThirdPartyResolvable(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"my-vllm": {Driver: "openai", BaseURL: "http://vllm.local:8000"},
		},
	}
	pr, err := newProviderResolver(cfg)
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	if _, err := pr.Get("my-vllm"); err != nil {
		t.Errorf("keyless 3rd-party provider not resolvable: %v", err)
	}
}

// TestNewProviderResolver_DedicatedLocalDrivers is the RFC CK regression: the
// new `vllm` / `llamacpp` drivers are registered (blank-imported in main.go),
// so a `providers:` entry naming them constructs and resolves through Get. A
// capabilities.max_context_tokens override reaches the driver (the RFC's way to
// advertise a server-launch-fixed context window).
func TestNewProviderResolver_DedicatedLocalDrivers(t *testing.T) {
	win := 32768
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm-local":     {Driver: "vllm", BaseURL: "http://localhost:8000/v1", Capabilities: &config.CapabilityOverride{MaxContextTokens: &win}},
			"llamacpp-local": {Driver: "llamacpp", BaseURL: "http://localhost:8080/v1"},
		},
	}
	pr, err := newProviderResolver(cfg)
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	for _, id := range []string{"vllm-local", "llamacpp-local"} {
		p, err := pr.Get(id)
		if err != nil {
			t.Errorf("%s not resolvable: %v", id, err)
			continue
		}
		if p.ID() != id {
			t.Errorf("%s: ID() = %q, want %q", id, p.ID(), id)
		}
	}
	if p, _ := pr.Get("vllm-local"); p != nil && p.Capabilities().MaxContextTokens != 32768 {
		t.Errorf("vllm-local capabilities.max_context_tokens not applied: got %d", p.Capabilities().MaxContextTokens)
	}
}
