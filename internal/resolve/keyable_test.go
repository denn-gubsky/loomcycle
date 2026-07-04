package resolve

import (
	"errors"
	"testing"
)

// RFC AX Layer 1: a restricted run whose only keyable provider is deepseek must
// resolve to deepseek even though the tier's priority puts other providers
// ahead of it — the KeyableProviders filter skips the un-keyable candidates.
func TestResolve_KeyableFilter_RoutesToTheOnlyKeyableProvider(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{
		Name:             "ats-filter",
		Tier:             "low",
		KeyableProviders: map[string]bool{"deepseek": true},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "deepseek" {
		t.Errorf("decision.Provider = %q, want deepseek (the only keyable provider)", dec.Provider)
	}
}

// When the KeyableProviders filter removes EVERY candidate, Resolve returns the
// ErrOperatorKeyRestricted policy sentinel — distinct from a transient
// availability outage (ErrTierUnavailable).
func TestResolve_KeyableFilter_EmptyViableSetIsRestricted(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	_, err := r.Resolve(AgentRequest{
		Name:             "ats-filter",
		Tier:             "low",
		KeyableProviders: map[string]bool{"nonexistent": true}, // none of the tier's providers
	})
	if !errors.Is(err, ErrOperatorKeyRestricted) {
		t.Fatalf("err = %v, want ErrOperatorKeyRestricted", err)
	}
	if errors.Is(err, ErrTierUnavailable) {
		t.Error("restriction must be distinct from an availability outage")
	}
}

// An EMPTY (non-nil) filter — a restricted run whose tenant can key nothing — is
// also a restriction refusal, not an outage.
func TestResolve_KeyableFilter_EmptyMapIsRestricted(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	_, err := r.Resolve(AgentRequest{
		Name:             "ats-filter",
		Tier:             "low",
		KeyableProviders: map[string]bool{}, // non-nil + empty ⇒ nothing keyable
	})
	if !errors.Is(err, ErrOperatorKeyRestricted) {
		t.Fatalf("err = %v, want ErrOperatorKeyRestricted", err)
	}
}

// A keyless provider (e.g. ollama-local) is always keyable: a restricted run
// with ollama-local in its set routes there when priority reaches it.
func TestResolve_KeyableFilter_KeylessProviderRoutable(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{
		Name:             "ats-filter",
		Tier:             "low",
		KeyableProviders: map[string]bool{"ollama-local": true},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "ollama-local" {
		t.Errorf("decision.Provider = %q, want ollama-local", dec.Provider)
	}
}

// A PINNED agent bypasses the KeyableProviders filter entirely (resolvePin skips
// candidate selection). This documents the Layer-2 requirement: a restricted
// pinned agent's guarantee lives in the driver backstop, NOT in routing — so
// resolution still returns the pin even for an un-keyable provider.
func TestResolve_KeyableFilter_PinNotFiltered(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{
		Name:             "cv-generator",
		PinProvider:      "anthropic",
		PinModel:         "claude-opus-4-7",
		KeyableProviders: map[string]bool{"deepseek": true}, // anthropic NOT keyable
	})
	if err != nil {
		t.Fatalf("pinned agent must not be filtered by KeyableProviders: %v", err)
	}
	if dec.Provider != "anthropic" || dec.Model != "claude-opus-4-7" {
		t.Errorf("decision = %+v, want anthropic/claude-opus-4-7 (pin bypasses routing)", dec)
	}
}

// A nil KeyableProviders (the unrestricted / gate-off path) is byte-identical to
// pre-RFC-AX resolution — the top library-priority candidate wins.
func TestResolve_KeyableFilter_NilIsUnrestricted(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"}) // KeyableProviders nil
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "deepseek" { // deepseek is first in fixture priority
		t.Errorf("decision.Provider = %q, want deepseek (unrestricted top priority)", dec.Provider)
	}
}

// Cascade MUST apply the IDENTICAL filter so the routing view a restricted
// consumer sees matches what actually runs.
func TestCascade_AppliesKeyableFilter(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	// Unfiltered cascade lists every low-tier provider in priority order.
	full := r.Cascade(AgentRequest{Name: "ats-filter", Tier: "low"})
	if len(full) < 2 {
		t.Fatalf("unfiltered cascade too short: %+v", full)
	}

	// Filtered cascade lists ONLY the keyable providers.
	filtered := r.Cascade(AgentRequest{
		Name:             "ats-filter",
		Tier:             "low",
		KeyableProviders: map[string]bool{"deepseek": true},
	})
	for _, c := range filtered {
		if c.Provider != "deepseek" {
			t.Errorf("filtered cascade contains un-keyable provider %q", c.Provider)
		}
	}
	if len(filtered) == 0 {
		t.Error("filtered cascade should still contain the keyable deepseek candidate")
	}
}
