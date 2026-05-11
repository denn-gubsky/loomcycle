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
	priority = []string{"deepseek", "ollama-local", "openai", "anthropic"}
	tiers = map[string][]Candidate{
		"low": {
			{Provider: "deepseek", Model: "deepseek-v4-flash"},
			{Provider: "ollama-local", Model: "gemma4:9b"},
			{Provider: "openai", Model: "gpt-5.4-mini"},
			{Provider: "anthropic", Model: "claude-haiku-4-5"},
		},
		"middle": {
			{Provider: "deepseek", Model: "deepseek-v4-pro"},
			{Provider: "ollama-local", Model: "qwen3.6:27b"},
			{Provider: "openai", Model: "gpt-5.4"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
		"high": {
			// DeepSeek deliberately absent at high tier per the
			// May-2026 matrix.
			{Provider: "ollama-local", Model: "kimi-k2.6"},
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
	for _, providerID := range []string{"anthropic", "openai", "deepseek", "ollama-local"} {
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
	if dec.Provider != "ollama-local" {
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
	if dec.Provider != "ollama-local" {
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
	for _, p := range []string{"anthropic", "openai", "deepseek", "ollama-local"} {
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

// ---- Excluded-flag tests (PR 2) ----

func TestSetExcluded_SkipsProviderInResolution(t *testing.T) {
	// Provider with no API key gets SetExcluded; resolver must skip
	// it the same way it skips an unreachable provider. Per the
	// operator's directive: providers without keys are MARKED
	// excluded so Snapshot() shows the distinct state.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	// Simulate "no DEEPSEEK_API_KEY set" → SetExcluded.
	r.SetExcluded("deepseek", "DEEPSEEK_API_KEY not set")

	dec, err := r.Resolve(AgentRequest{Name: "ats-filter", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// DeepSeek is library priority's first choice for low tier;
	// with it excluded, resolver should fall through to Ollama.
	if dec.Provider != "ollama-local" {
		t.Errorf("decision provider = %q, want ollama (fallthrough from excluded deepseek)", dec.Provider)
	}
}

func TestSetExcluded_VisibleInSnapshot(t *testing.T) {
	// Operators reading Snapshot() must be able to distinguish
	// "deliberately excluded" from "probe failed". The Excluded
	// flag is the wire-stable signal for that.
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	r.SetExcluded("anthropic", "ANTHROPIC_API_KEY not set")
	r.SetReachable("openai", false, nil, "EOF on /v1/models")

	snap := r.Snapshot()
	if !snap["anthropic"].Excluded {
		t.Error("anthropic should be Excluded=true (no key)")
	}
	if snap["anthropic"].LastError == "" {
		t.Error("anthropic LastError should carry the exclusion reason")
	}
	if snap["openai"].Excluded {
		t.Error("openai should be Excluded=false (probe attempted, failed)")
	}
	if snap["openai"].Reachable {
		t.Error("openai Reachable should be false")
	}
}

func TestSetReachable_ClearsExcludedFlag(t *testing.T) {
	// Operator adds the API key after startup; periodic re-probe
	// runs and SetReachable is called. The Excluded flag should
	// clear so the operator can tell at a glance "this provider
	// is now actively probed, not deliberately excluded".
	r := NewResolver([]string{"anthropic"}, map[string][]Candidate{
		"low": {{Provider: "anthropic", Model: "claude-haiku-4-5"}},
	})
	r.SetExcluded("anthropic", "ANTHROPIC_API_KEY not set")

	// Re-probe succeeds.
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5"}, "")

	snap := r.Snapshot()["anthropic"]
	if snap.Excluded {
		t.Error("Excluded flag should clear after a successful probe (operator added the key)")
	}
	if !snap.Reachable {
		t.Error("Reachable should be true after successful probe")
	}
}

func TestSetExcluded_IsIdempotent(t *testing.T) {
	// Periodic probe sweeps SetExcluded every cycle for unconfigured
	// providers. The contract is that repeat calls don't churn other
	// state — only LastCheck and (idempotent) Excluded/LastError.
	r := NewResolver([]string{"anthropic"}, nil)
	r.SetExcluded("anthropic", "no key")
	t1 := r.Snapshot()["anthropic"].LastCheck
	r.SetExcluded("anthropic", "no key")
	t2 := r.Snapshot()["anthropic"].LastCheck
	if !t2.After(t1) && t2 != t1 {
		// LastCheck should advance OR stay the same; just sanity.
		t.Errorf("LastCheck went backwards: %v -> %v", t1, t2)
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

// ---- v0.8.2 user_tier overlay tests ----

// freeTierOverlay is a typical "free" user-tier policy: gemini-free +
// ollama-local only. Anthropic and DeepSeek are NOT in the priority,
// so any agent with agent.Providers including only those will refuse
// for free users (option A).
func freeTierOverlay() *UserTierOverlay {
	return &UserTierOverlay{
		Name:             "free",
		ProviderPriority: []string{"gemini", "ollama-local"},
		Tiers: map[string][]Candidate{
			"low":    {{Provider: "gemini", Model: "gemini-2.0-flash"}, {Provider: "ollama-local", Model: "llama-local"}},
			"middle": {{Provider: "gemini", Model: "gemini-2.0-flash"}, {Provider: "ollama-local", Model: "llama-local"}},
			"high":   {{Provider: "gemini", Model: "gemini-2.5-pro"}},
		},
		FallbackOnError: false, // free tier doesn't cascade — cost cap
	}
}

// highTierOverlay is the opposite end: anthropic-only, premium models.
func highTierOverlay() *UserTierOverlay {
	return &UserTierOverlay{
		Name:                "high",
		ProviderPriority:    []string{"anthropic"},
		Tiers:               nil, // falls through to library tiers for the anthropic candidates
		FallbackOnError:     true,
		MaxFallbackAttempts: 3,
	}
}

// TestResolve_UserTierOverlay_UsesUserTierPriorityWhenAgentHasNone:
// no per-agent Providers override + a user_tier overlay → resolver
// walks the user_tier's order, not the library default. Pins the
// headline overlay-precedence case.
func TestResolve_UserTierOverlay_UsesUserTierPriorityWhenAgentHasNone(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	// Seed gemini reachable, others NOT. Confirms the resolver walks
	// the user_tier's order (gemini first) rather than the library
	// order (deepseek first).
	r.SeedModel("gemini", "gemini-2.0-flash")
	r.SetProviderReachable("gemini", true)

	overlay := freeTierOverlay()
	overlay.Tiers["low"] = []Candidate{
		{Provider: "gemini", Model: "gemini-2.0-flash"},
		{Provider: "ollama-local", Model: "llama-local"},
	}
	dec, err := r.Resolve(AgentRequest{
		Name:     "any-agent",
		Tier:     "low",
		UserTier: overlay,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "gemini" || dec.Model != "gemini-2.0-flash" {
		t.Errorf("Decision = %+v; want gemini/gemini-2.0-flash", dec)
	}
}

// TestResolve_UserTierOverlay_NonEmptyIntersection: per-agent
// Providers AND user_tier set. Intersection must be walked in
// agent-Providers order (preserves operator intent inside the
// tier-restricted space).
func TestResolve_UserTierOverlay_NonEmptyIntersection(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	r.SeedModel("anthropic", "claude-sonnet-4-6")
	r.SetProviderReachable("anthropic", true)
	r.SeedModel("deepseek", "deepseek-v4-pro")
	r.SetProviderReachable("deepseek", true)

	overlay := highTierOverlay()
	overlay.ProviderPriority = []string{"anthropic", "deepseek"} // user has both

	dec, err := r.Resolve(AgentRequest{
		Name:      "cv-adapter",
		Tier:      "middle",
		Providers: []string{"anthropic"}, // agent constrains to anthropic only
		UserTier:  overlay,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Intersection = {anthropic}. Library "middle" candidates have anthropic;
	// resolver picks it.
	if dec.Provider != "anthropic" {
		t.Errorf("Decision.Provider = %q; want anthropic (the intersection)", dec.Provider)
	}
}

// TestResolve_UserTierOverlay_EmptyIntersectionRefuses: option-A
// refusal — per-agent Providers and user_tier overlay have NO
// providers in common. Headline scenario for "cv-adapter is anthropic-
// pinned for privacy + free user has no anthropic". Returns
// ErrTierAgentNotAvailable (operator policy refusal, distinct from a
// transient outage).
func TestResolve_UserTierOverlay_EmptyIntersectionRefuses(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)
	r.SetProviderReachable("gemini", true)
	r.SeedModel("gemini", "gemini-2.0-flash")

	_, err := r.Resolve(AgentRequest{
		Name:      "cv-adapter",
		Tier:      "middle",
		Providers: []string{"anthropic"}, // agent demands anthropic
		UserTier:  freeTierOverlay(),     // free has [gemini, ollama] — no anthropic
	})
	if err == nil {
		t.Fatal("expected refusal; got success")
	}
	if !errors.Is(err, ErrTierAgentNotAvailable) {
		t.Errorf("expected ErrTierAgentNotAvailable, got %v", err)
	}
	// Distinct from ErrTierUnavailable — operator policy refusal, not
	// matrix outage. The client must NOT retry; they need an upgrade.
	if errors.Is(err, ErrTierUnavailable) {
		t.Errorf("expected NOT-ErrTierUnavailable (policy refusal, not outage), got %v", err)
	}
}

// TestResolve_UserTierOverlay_AgentModelsExcludeTierProviders: the
// secondary refusal path — per-agent Models[tier] only lists providers
// the user_tier doesn't grant. Same option-A refusal shape as above
// but triggered by Models rather than Providers.
func TestResolve_UserTierOverlay_AgentModelsExcludeTierProviders(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	_, err := r.Resolve(AgentRequest{
		Name: "cv-adapter",
		Tier: "middle",
		// agent.Models pins this tier to anthropic-only candidates
		Models: map[string][]Candidate{
			"middle": {{Provider: "anthropic", Model: "claude-sonnet-4-6"}},
		},
		// free user_tier has no anthropic in provider_priority
		UserTier: freeTierOverlay(),
	})
	if err == nil {
		t.Fatal("expected refusal; got success")
	}
	if !errors.Is(err, ErrTierAgentNotAvailable) {
		t.Errorf("expected ErrTierAgentNotAvailable, got %v", err)
	}
}

// TestResolve_UserTierOverlay_NoOverlayPreservesV07Behaviour: nil
// UserTier on AgentRequest must produce IDENTICAL behaviour to the
// v0.7.x path. Critical regression guard: every v0.7 deployment that
// hasn't opted into user_tiers stays unchanged.
func TestResolve_UserTierOverlay_NoOverlayPreservesV07Behaviour(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	// With no overlay, resolver should walk the library priority
	// (deepseek first) for an agent with no overrides.
	dec, err := r.Resolve(AgentRequest{
		Name:     "default-agent",
		Tier:     "middle",
		UserTier: nil,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "deepseek" {
		t.Errorf("Decision.Provider = %q; want deepseek (library-priority first)", dec.Provider)
	}
}

// TestResolve_UserTierOverlay_TierFallthroughToLibrary: user_tier
// overlay defines no candidates for the requested task tier; resolver
// falls through to library tiers (after applying user_tier's provider
// priority filter).
func TestResolve_UserTierOverlay_TierFallthroughToLibrary(t *testing.T) {
	priority, tiers := fixtureLibrary()
	r := NewResolver(priority, tiers)
	seedAll(t, r, tiers)

	overlay := highTierOverlay() // Tiers map is nil
	dec, err := r.Resolve(AgentRequest{
		Name:     "any",
		Tier:     "high",
		UserTier: overlay,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// overlay.ProviderPriority = [anthropic]; library "high" candidates
	// include anthropic (claude-opus-4-7). Resolver should pick it.
	if dec.Provider != "anthropic" {
		t.Errorf("Decision.Provider = %q; want anthropic (library tier filtered through user_tier priority)", dec.Provider)
	}
}

// TestResolve_OllamaLocalAndOllamaAreDistinct pins the v0.8.3 split:
// the resolver treats "ollama" (hosted) and "ollama-local" (local)
// as independent priority slots. Mark hosted ollama stalled; the
// resolver must walk on to the next slot — not silently substitute
// the local registration just because both share the same driver
// internally.
func TestResolve_OllamaLocalAndOllamaAreDistinct(t *testing.T) {
	priority := []string{"ollama", "ollama-local", "anthropic"}
	tiers := map[string][]Candidate{
		"low": {
			{Provider: "ollama", Model: "kimi-k2.6"},       // hosted-only
			{Provider: "ollama-local", Model: "gemma4:9b"}, // local-only
			{Provider: "anthropic", Model: "claude-haiku-4-5"},
		},
	}
	r := NewResolver(priority, tiers)
	r.SeedModel("ollama", "kimi-k2.6")
	r.SeedModel("ollama-local", "gemma4:9b")
	r.SeedModel("anthropic", "claude-haiku-4-5")
	r.SetProviderReachable("ollama", true)
	r.SetProviderReachable("ollama-local", true)
	r.SetProviderReachable("anthropic", true)

	// First resolution: hosted is healthy → resolver picks it.
	dec, err := r.Resolve(AgentRequest{Name: "any", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "ollama" || dec.Model != "kimi-k2.6" {
		t.Errorf("first resolve = %+v; want ollama/kimi-k2.6", dec)
	}

	// Mark hosted's model stalled — resolver should walk past it to
	// the local registration. If the two ids were conflated the
	// resolver would either return the same decision or skip BOTH.
	r.MarkStalled("ollama", "kimi-k2.6", "503")
	dec, err = r.Resolve(AgentRequest{Name: "any", Tier: "low"})
	if err != nil {
		t.Fatalf("Resolve after stall: %v", err)
	}
	if dec.Provider != "ollama-local" || dec.Model != "gemma4:9b" {
		t.Errorf("post-stall resolve = %+v; want ollama-local/gemma4:9b (proves the two are distinct)", dec)
	}
}
