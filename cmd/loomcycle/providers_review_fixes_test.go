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
