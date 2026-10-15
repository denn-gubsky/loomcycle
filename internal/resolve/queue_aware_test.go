package resolve

import (
	"strings"
	"testing"
	"time"
)

// waits is a QueueWait that reports a fixed wait per provider.
func waits(m map[string]time.Duration) func(provider, model string) time.Duration {
	return func(provider, _ string) time.Duration { return m[provider] }
}

func queueAwareRequest(w map[string]time.Duration) AgentRequest {
	return AgentRequest{Name: "a", Tier: "middle", QueueWait: waits(w), SwitchAfter: 20 * time.Second}
}

// A candidate whose provider would make the first call wait past SwitchAfter
// is passed over for the next available one, and the decision names what was
// passed over.
func TestResolve_QueueAwarePassesOverABusyCandidate(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(queueAwareRequest(map[string]time.Duration{"deepseek": 45 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "ollama-local" || dec.Model != "qwen3.6:27b" {
		t.Fatalf("decision = %s/%s, want the next candidate ollama-local/qwen3.6:27b", dec.Provider, dec.Model)
	}
	if len(dec.PassedOver) != 1 || !strings.HasPrefix(dec.PassedOver[0], "deepseek/deepseek-v4-pro ~45s") {
		t.Errorf("passed over = %v, want deepseek with its wait", dec.PassedOver)
	}
}

// A wait at or below SwitchAfter keeps the cascade's first choice.
func TestResolve_QueueAwareKeepsACandidateWithinSwitchAfter(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(queueAwareRequest(map[string]time.Duration{"deepseek": 20 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "deepseek" || len(dec.PassedOver) != 0 {
		t.Fatalf("decision = %+v, want deepseek with nothing passed over", dec)
	}
}

// When every available candidate is over SwitchAfter, the shortest wait wins,
// and the chosen one is not listed as passed over.
func TestResolve_QueueAwareTakesTheShortestWaitWhenAllAreBusy(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(queueAwareRequest(map[string]time.Duration{
		"deepseek": 90 * time.Second, "ollama-local": 60 * time.Second,
		"openai": 30 * time.Second, "anthropic": 30 * time.Second,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "openai" {
		t.Fatalf("decision = %s, want openai (shortest wait, earliest on the tie with anthropic)", dec.Provider)
	}
	for _, p := range dec.PassedOver {
		if strings.HasPrefix(p, "openai/") {
			t.Errorf("passed over %v lists the chosen candidate", dec.PassedOver)
		}
	}
	if len(dec.PassedOver) != 3 {
		t.Errorf("passed over = %v, want the three others", dec.PassedOver)
	}
}

// An unavailable candidate is never chosen, however short its wait; and a
// request with no QueueWait resolves exactly as before.
func TestResolve_QueueAwareOnlyChoosesAvailableCandidates(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	r.MarkStalled("ollama-local", "qwen3.6:27b", "down")

	dec, err := r.Resolve(queueAwareRequest(map[string]time.Duration{"deepseek": time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "openai" {
		t.Fatalf("decision = %s, want openai (ollama-local is stalled)", dec.Provider)
	}

	plain, err := r.Resolve(AgentRequest{Name: "a", Tier: "middle"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Provider != "deepseek" || plain.PassedOver != nil {
		t.Errorf("a request without QueueWait = %+v, want deepseek as always", plain)
	}
}
