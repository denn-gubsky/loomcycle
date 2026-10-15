package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestProviderResolver_HandsOutEveryProviderInItsCallLane — the registry is the
// ONE place a provider's cap is applied: whatever Get returns is the driver
// wrapped in its call lane, on the scheduler the resolver exposes (which the
// server reports and reloads). That holds for a capped provider and an uncapped
// one, and for the set a Reload builds — so no holder of a registry provider
// can make a call max_concurrent does not count.
//
// The wrapper must not cost the driver's optional interfaces: they stay
// reachable through providers.As.
func TestProviderResolver_HandsOutEveryProviderInItsCallLane(t *testing.T) {
	pr, err := newProviderResolver(&config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm-local":     {Driver: "vllm", BaseURL: "http://a:8000/v1", MaxConcurrent: 2},
			"llamacpp-local": {Driver: "llamacpp", BaseURL: "http://c:8080/v1"},
		},
	})
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	if pr.sched == nil {
		t.Fatal("the resolver built no call scheduler")
	}
	if got := pr.sched.LaneCap("vllm-local", config.ModelKindChat); got != 2 {
		t.Errorf("vllm-local chat lane cap = %d, want 2 (providers.vllm-local.max_concurrent)", got)
	}
	if pr.sched.Gated("llamacpp-local", config.ModelKindChat) {
		t.Error("an uncapped provider is gated")
	}
	assertWrapped := func(when string, ids ...string) {
		t.Helper()
		for _, id := range ids {
			p, err := pr.Get(id)
			if err != nil {
				t.Fatalf("%s: Get(%s): %v", when, id, err)
			}
			if _, ok := p.(*lanegate.Provider); !ok {
				t.Errorf("%s: Get(%s) returned a bare %T — its calls would escape the provider's cap", when, id, p)
			}
			if _, ok := providers.As[providers.KeyedProvider](p); !ok {
				t.Errorf("%s: %s lost KeyedProvider behind the wrapper", when, id)
			}
		}
	}
	assertWrapped("at boot", "vllm-local", "llamacpp-local")

	sched := pr.sched
	if err := pr.Reload(&config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm-local": {Driver: "vllm", BaseURL: "http://b:8000/v1", MaxConcurrent: 2},
			"vllm-two":   {Driver: "vllm", BaseURL: "http://d:8000/v1"},
		},
	}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if pr.sched != sched {
		t.Fatal("Reload replaced the scheduler — the server would report and reload lanes no call goes through")
	}
	assertWrapped("after reload", "vllm-local", "vllm-two")
}

// TestDescribeCallCaps_NamesEveryCapInAStableOrder pins the boot line an
// operator reads to confirm what is capped.
func TestDescribeCallCaps_NamesEveryCapInAStableOrder(t *testing.T) {
	caps := lanegate.SchedulerConfig(map[string]config.ProviderConfig{
		"openai": {MaxConcurrent: 8},
		"ollama-local": {
			MaxConcurrent:     2,
			MaxModelsInFlight: 1,
			Lanes:             map[string]config.ProviderLane{config.ModelKindDecision: {MaxConcurrent: 1}},
		},
		"uncapped": {},
	}, nil)
	if got, want := describeCallCaps(caps), "ollama-local[chat=2 decision=1 models=1], openai[chat=8]"; got != want {
		t.Errorf("describeCallCaps = %q, want %q", got, want)
	}
}
