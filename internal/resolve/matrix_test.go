package resolve

import (
	"errors"
	"testing"
)

// fixtureLibrary returns the May-2026 default matrix — same shape the
// example yaml ships, adapted to the resolver's Candidate type. Used
// across the tests so each one starts from the realistic operator
// config rather than a contrived two-row table.
func fixtureLibrary() (priority []string, tiers map[string][]Candidate) {
	priority = []string{"deepseek", "ollama", "openai", "anthropic"}
	tiers = map[string][]Candidate{
		"low": {
			{Provider: "deepseek", Model: "deepseek-v4-flash"},
			{Provider: "ollama", Model: "gemma4:9b"},
			{Provider: "openai", Model: "gpt-5.4-mini"},
			{Provider: "anthropic", Model: "claude-haiku-4-5"},
		},
		"middle": {
			{Provider: "deepseek", Model: "deepseek-v4-pro"},
			{Provider: "ollama", Model: "qwen3.6:27b"},
			{Provider: "openai", Model: "gpt-5.4"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
		"high": {
			// DeepSeek deliberately absent at high tier per the
			// May-2026 matrix.
			{Provider: "ollama", Model: "kimi-k2.6"},
			{Provider: "openai", Model: "gpt-5.5"},
			{Provider: "anthropic", Model: "claude-opus-4-7"},
		},
	}
	return
}

// seedAll marks every (provider, model) in the library as listed +
// every provider as reachable. Equivalent to "every provider has its
// API key set, stub-probe pre-seeded everything" — the steady state
// the resolver is designed for. Tests then individually flip flags
// via SetProviderReachable/MarkStalled to exercise edge cases.
func seedAll(t *testing.T, r *Resolver, tiers map[string][]Candidate) {
	t.Helper()
	for _, cands := range tiers {
		for _, c := range cands {
			r.SeedModel(c.Provider, c.Model)
		}
	}
	for _, providerID := range []string{"anthropic", "openai", "deepseek", "ollama"} {
		r.SetProviderReachable(providerID, true)
	}
}

// ---- Pin-path tests ----

func TestResolve_ExplicitPinReturnsAsRequested(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{
		Name:        "cv-generator",
		PinProvider: "anthropic",
		PinModel:    "claude-opus-4-7",
		Effort:      "medium",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "anthropic" || dec.Model != "claude-opus-4-7" || dec.Effort != "medium" {
		t.Errorf("decision = %+v, want anthropic/claude-opus-4-7/medium", dec)
	}
}

func TestResolve_PinUnavailableWhenStalled(t *testing.T) {
	// Pin path consults the matrix — a stalled pinned model surfaces
	// ErrPinUnavailable instead of leaking the driver's 5xx. The
	// canonical use case: an operator pinned an agent to a specific
	// model that's just been deprecated by the provider.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	r.MarkStalled("anthropic", "claude-opus-4-7", "404 model deprecated")

	_, err := r.Resolve(AgentRequest{
		Name:        "cv-generator",
		PinProvider: "anthropic",
		PinModel:    "claude-opus-4-7",
	})
	if !errors.Is(err, ErrPinUnavailable) {
		t.Errorf("err = %v, want ErrPinUnavailable", err)
	}
}

func TestResolve_PinUnavailableWhenProviderUnreachable(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	r.SetProviderReachable("anthropic", false)

	_, err := r.Resolve(AgentRequest{
		Name:        "pin-agent",
		PinProvider: "anthropic",
		PinModel:    "claude-opus-4-7",
	})
	if !errors.Is(err, ErrPinUnavailable) {
		t.Errorf("err = %v, want ErrPinUnavailable", err)
	}
}

// ---- Tier-path tests ----

func TestResolve_LibraryPriorityWalk(t *testing.T) {
	// All four providers reachable + listed → resolver should pick
	// the first candidate in library priority order (deepseek for
	// the low tier, given the May-2026 matrix).
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "deepseek" || dec.Model != "deepseek-v4-flash" {
		t.Errorf("decision = %+v, want deepseek/deepseek-v4-flash", dec)
	}
}

func TestResolve_FallthroughWhenFirstCandidateStalled(t *testing.T) {
	// DeepSeek's low-tier model marked stalled → resolver should
	// fall through to the next provider in library priority
	// (Ollama). Demonstrates the central feature: a stalled
	// candidate doesn't block a tier-using agent.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	r.MarkStalled("deepseek", "deepseek-v4-flash", "5xx after retry")

	dec, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "ollama" {
		t.Errorf("decision provider = %q, want ollama (fallthrough from stalled deepseek)", dec.Provider)
	}
}

func TestResolve_FallthroughWhenProviderUnreachable(t *testing.T) {
	// DeepSeek entirely unreachable (provider-level) → resolver
	// should skip ALL DeepSeek candidates regardless of model
	// listing, fall through to Ollama. Distinct from per-model
	// stall: this simulates "DeepSeek is down" rather than "this
	// specific model is broken".
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	r.SetProviderReachable("deepseek", false)

	dec, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "ollama" {
		t.Errorf("decision provider = %q, want ollama (fallthrough from unreachable deepseek)", dec.Provider)
	}
}

func TestResolve_AgentProvidersFullOverride(t *testing.T) {
	// Per-agent providers list FULLY REPLACES library priority.
	// This agent says [anthropic, openai] — DeepSeek and Ollama
	// must be skipped even though library priority lists them
	// first. The first candidate matching anthropic in the middle
	// tier is claude-sonnet-4-6.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	dec, err := r.Resolve(AgentRequest{
		Name:      "ranker",
		Tier:      "middle",
		Providers: []string{"anthropic", "openai"},
		Effort:    "high",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "anthropic" || dec.Model != "claude-sonnet-4-6" {
		t.Errorf("decision = %+v, want anthropic/claude-sonnet-4-6", dec)
	}
	if dec.Effort != "high" {
		t.Errorf("effort = %q, want high (effort plumbed through)", dec.Effort)
	}
}

func TestResolve_AgentModelsFullOverride(t *testing.T) {
	// Per-agent models map FULLY REPLACES library tier definition
	// for the named tier. cv-generator restricts itself to
	// Anthropic only at high tier — even though library high
	// includes Ollama and OpenAI candidates, this agent never
	// picks them.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	// Make sure the per-agent override candidate is in the matrix.
	r.SeedModel("anthropic", "claude-opus-4-7")

	dec, err := r.Resolve(AgentRequest{
		Name: "cv-generator",
		Tier: "high",
		Models: map[string][]Candidate{
			"high": {{Provider: "anthropic", Model: "claude-opus-4-7"}},
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "anthropic" || dec.Model != "claude-opus-4-7" {
		t.Errorf("decision = %+v, want anthropic/claude-opus-4-7", dec)
	}
}

func TestResolve_TierUnavailableWhenAllStalled(t *testing.T) {
	// Mark every low-tier candidate stalled → resolver returns
	// ErrTierUnavailable. The HTTP/gRPC layer translates this to
	// 503 / codes.Unavailable.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	for _, c := range tiers["low"] {
		r.MarkStalled(c.Provider, c.Model, "stalled in test")
	}

	_, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"})
	if !errors.Is(err, ErrTierUnavailable) {
		t.Errorf("err = %v, want ErrTierUnavailable", err)
	}
}

func TestResolve_TierUnavailableWhenNoProviderReachable(t *testing.T) {
	// Cold-start degraded mode: every provider unreachable → tier
	// resolution returns ErrTierUnavailable. Server stays up; only
	// individual agent runs fail (the documented behaviour).
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	for _, p := range []string{"anthropic", "openai", "deepseek", "ollama"} {
		r.SetProviderReachable(p, false)
	}

	_, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"})
	if !errors.Is(err, ErrTierUnavailable) {
		t.Errorf("err = %v, want ErrTierUnavailable", err)
	}
}

// ---- Validation tests ----

func TestResolve_RequiresAgentName(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	_, err := r.Resolve(AgentRequest{Tier: "low"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument (missing agent name)", err)
	}
}

func TestResolve_RequiresPinOrTier(t *testing.T) {
	// Neither pin nor tier set → ErrInvalidArgument. Validation in
	// internal/config catches this at load time, but the resolver
	// guards anyway in case AgentRequest is built directly (tests).
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	_, err := r.Resolve(AgentRequest{Name: "broken"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument (no pin, no tier)", err)
	}
}

func TestResolve_RejectsHalfPin(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	_, err := r.Resolve(AgentRequest{
		Name:        "broken",
		PinProvider: "anthropic",
		// PinModel deliberately empty.
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument (half-pin rejected)", err)
	}
}

func TestResolve_TierWithNoCandidates(t *testing.T) {
	// Library tier definition empty for the requested tier (e.g.
	// operator hasn't configured `tiers.high:`). Returns
	// ErrTierUnavailable so the operator gets a clear 503 rather
	// than a confusing "unknown agent" or panic.
	r := NewResolver([]string{"anthropic"}, map[string][]Candidate{
		"low": {{Provider: "anthropic", Model: "claude-haiku-4-5"}},
		// "high" not configured.
	})
	r.SeedModel("anthropic", "claude-haiku-4-5")
	r.SetProviderReachable("anthropic", true)

	_, err := r.Resolve(AgentRequest{Name: "needs-high", Tier: "high"})
	if !errors.Is(err, ErrTierUnavailable) {
		t.Errorf("err = %v, want ErrTierUnavailable", err)
	}
}

// ---- Probe / matrix-update tests ----

func TestSetReachable_ClearsStallOnRelistedModel(t *testing.T) {
	// A model marked stalled should come back to "available" once
	// the next probe relists it. Models NOT in the relist keep
	// their prior status (so a transient probe miss doesn't blank
	// availability).
	r := NewResolver([]string{"deepseek"}, map[string][]Candidate{
		"low": {{Provider: "deepseek", Model: "deepseek-v4-flash"}},
	})
	r.SeedModel("deepseek", "deepseek-v4-flash")
	r.SetProviderReachable("deepseek", true)
	r.MarkStalled("deepseek", "deepseek-v4-flash", "transient 5xx")

	// Probe relists the model.
	r.SetReachable("deepseek", true, []string{"deepseek-v4-flash"}, "")

	dec, err := r.Resolve(AgentRequest{Name: "test", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve after re-probe: %v", err)
	}
	if dec.Provider != "deepseek" {
		t.Errorf("decision = %+v, want deepseek (stall cleared by re-probe)", dec)
	}
}

func TestSetReachable_NilModelsKeepsPriorList(t *testing.T) {
	// Transient probe failure: SetReachable(provider, false, nil)
	// should mark unreachable but NOT blank the model list. The
	// next successful probe will re-populate; meanwhile the
	// reachability flag alone gates Resolve.
	r := NewResolver([]string{"deepseek"}, map[string][]Candidate{
		"low": {{Provider: "deepseek", Model: "deepseek-v4-flash"}},
	})
	r.SeedModel("deepseek", "deepseek-v4-flash")
	r.SetProviderReachable("deepseek", true)

	// Transient failure — pass nil for listed models.
	r.SetReachable("deepseek", false, nil, "EOF on /v1/models")

	snap := r.Snapshot()["deepseek"]
	if snap.Reachable {
		t.Error("provider should be unreachable after probe failure")
	}
	if _, ok := snap.Models["deepseek-v4-flash"]; !ok {
		t.Error("model list should be preserved on transient probe failure (nil listed)")
	}
}

func TestSnapshot_IsACopy(t *testing.T) {
	// Snapshot's job is operator observability — callers should
	// not be able to mutate resolver state by writing to the
	// returned map. Verifies the deep-copy behaviour.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	snap := r.Snapshot()
	snap["fake"] = Availability{Reachable: true}
	if _, ok := r.Snapshot()["fake"]; ok {
		t.Error("Snapshot mutation leaked back into resolver state")
	}
}
