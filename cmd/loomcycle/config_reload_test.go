package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestProviderResolver_Reload is the RFC CK provider-set reload: Reload rebuilds
// byID from a new config (changed base_url, added provider) and swaps it in place
// so Get returns the new set; currentCfg tracks the new config. A subsequent bad
// config is rejected and the previous set is kept.
func TestProviderResolver_Reload(t *testing.T) {
	pr, err := newProviderResolver(&config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm-local": {Driver: "vllm", BaseURL: "http://a:8000/v1"},
		},
	})
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	if _, err := pr.Get("vllm-local"); err != nil {
		t.Fatalf("vllm-local not resolvable pre-reload: %v", err)
	}
	if _, err := pr.Get("llamacpp-local"); err == nil {
		t.Fatal("llamacpp-local resolvable before it was declared")
	}

	// Reload: change vllm-local's base_url and ADD llamacpp-local.
	next := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm-local":     {Driver: "vllm", BaseURL: "http://b:8000/v1"},
			"llamacpp-local": {Driver: "llamacpp", BaseURL: "http://c:8080/v1"},
		},
	}
	if err := pr.Reload(next); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	for _, id := range []string{"vllm-local", "llamacpp-local"} {
		if _, err := pr.Get(id); err != nil {
			t.Errorf("%s not resolvable post-reload: %v", id, err)
		}
	}
	if pr.currentCfg() != next {
		t.Error("currentCfg did not advance to the reloaded config")
	}

	// A bad config (unconstructable — unknown driver) is rejected; the previous
	// set is kept intact.
	badErr := pr.Reload(&config.Config{
		Providers: map[string]config.ProviderConfig{"nope": {Driver: "does-not-exist"}},
	})
	if badErr == nil {
		t.Error("Reload with an unknown driver should error")
	}
	if _, err := pr.Get("vllm-local"); err != nil {
		t.Errorf("previous provider set lost after a rejected reload: %v", err)
	}
}

// TestProviderResolver_ReloadRaceWithGet runs concurrent Get against Reload —
// under -race this fails if the byID swap is not synchronized.
func TestProviderResolver_ReloadRaceWithGet(t *testing.T) {
	pr, err := newProviderResolver(&config.Config{
		Providers: map[string]config.ProviderConfig{"vllm-local": {Driver: "vllm", BaseURL: "http://a:8000/v1"}},
	})
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			_, _ = pr.Get("vllm-local")
			_ = pr.currentCfg()
		}
		close(done)
	}()
	for i := 0; i < 2000; i++ {
		_ = pr.Reload(&config.Config{
			Providers: map[string]config.ProviderConfig{"vllm-local": {Driver: "vllm", BaseURL: "http://b:8000/v1"}},
		})
	}
	<-done
}
