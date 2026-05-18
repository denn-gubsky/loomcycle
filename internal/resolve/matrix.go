// Package resolve implements model resolution: an agent declares a
// tier (low / middle / high) plus optional effort hint, and the
// resolver picks a concrete (provider, model) pair against an
// availability matrix that's seeded at startup and updated reactively
// when calls fail.
//
// PR 1 of feature-resolve-matrix scaffolds the resolver behind a
// "stub probe" — the matrix is populated from config + treats a
// provider as available iff its API key is present (or for ollama,
// its base URL is set). Real probes against /v1/models and friends
// land in PR 2 alongside live stall feedback.
//
// Concurrency: the Resolver is safe for concurrent Resolve / MarkStalled
// calls. The matrix is guarded by a single sync.RWMutex — writes (the
// stall path + the periodic re-probe in PR 2) are infrequent compared
// to reads (every agent invocation), so a RWMutex is the right shape.
//
// Design + decisions: see doc-internal/rfcs/model-resolution-matrix.md.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors. Wire surfaces (HTTP / gRPC) translate these into
// 503 Service Unavailable / codes.Unavailable.
var (
	// ErrTierUnavailable is returned by Resolve when no candidate in
	// the requested tier resolves to an available (provider, model).
	// Wraps the tier name so callers can surface it.
	ErrTierUnavailable = errors.New("no provider available for requested tier")

	// ErrInvalidArgument is returned when an agent has neither an
	// explicit provider+model pin nor a tier — the resolver has no
	// way to pick a model.
	ErrInvalidArgument = errors.New("invalid agent definition")

	// ErrUnknownAgent is returned when an agent name isn't registered
	// in the config. Distinct from ErrInvalidArgument so HTTP can map
	// to 400 (bad request) and gRPC to InvalidArgument; the agent
	// name typo case is the most common cause.
	ErrUnknownAgent = errors.New("unknown agent")

	// ErrPinUnavailable is returned when an agent has an explicit
	// provider+model pin but the matrix says that provider/model is
	// stalled or unreachable. Distinct from ErrTierUnavailable
	// because a pinned agent has no fallthrough — caller asked for
	// THAT model specifically.
	ErrPinUnavailable = errors.New("pinned (provider, model) is unavailable")

	// ErrTierAgentNotAvailable is returned by Resolve (v0.8.2+) when
	// the agent's policy (per-agent Providers / Models) and the
	// run's user_tier policy have an empty intersection — the agent
	// requires providers / models the user_tier doesn't grant access
	// to. Distinct from ErrTierUnavailable because it's a POLICY
	// refusal (operator-defined), not a matrix outage. HTTP maps
	// this to 403 Forbidden with a typed error code so the client
	// can render an "upgrade your tier to use this agent" message.
	ErrTierAgentNotAvailable = errors.New("agent not available for user_tier")
)

// Decision is the resolver's output: which (provider, model) the loop
// should dispatch to, plus the effort hint to plumb through to the
// driver. Effort is empty when the agent didn't declare one.
type Decision struct {
	Provider string
	Model    string
	Effort   string
}

// AgentRequest is the resolver's input. Mirrors the relevant subset of
// config.AgentDef but kept as a separate type so internal/resolve
// doesn't import internal/config (avoids circular imports — the HTTP
// server, which has both, builds the AgentRequest).
type AgentRequest struct {
	// Name of the agent for error messages. Resolver doesn't index
	// by name internally.
	Name string

	// Pin path. When both are non-empty, the resolver returns the
	// pin (still consulting the matrix for the stall check; pinned
	// agents fail with ErrPinUnavailable rather than falling
	// through to a different provider).
	PinProvider string
	PinModel    string

	// Tier path. When set, the resolver walks the candidate list
	// (Models[Tier] if non-empty, else library Tiers[Tier]) in the
	// effective priority order (Providers if non-empty, else
	// library ProviderPriority). One of "low" / "middle" / "high".
	Tier string

	// Effort is plumbed through unchanged (the resolver doesn't
	// translate it — the driver does that in PR 3).
	Effort string

	// Per-agent overrides. Empty / nil = use library defaults.
	Providers []string
	Models    map[string][]Candidate

	// UserTier is the v0.8.2 user-facing-tier policy overlay for
	// this resolution. Nil = no overlay (back-compat path; resolver
	// uses library + per-agent overrides as in v0.7.x). Non-nil:
	// the overlay's ProviderPriority + Tiers sit BETWEEN library and
	// per-agent in the precedence chain.
	//
	// When the overlay AND per-agent Providers are both set, the
	// intersection (in per-agent order) is what the resolver walks.
	// Empty intersection → ErrTierAgentNotAvailable (operator policy
	// refusal — not a transient outage).
	UserTier *UserTierOverlay
}

// UserTierOverlay carries the per-request user-tier policy. Built by
// the HTTP server from cfg.UserTiers[req.user_tier] and threaded into
// AgentRequest. PR 1 plumbs ProviderPriority + Tiers through the
// resolver; PR 2 consumes FallbackOnError + MaxFallbackAttempts in the
// loop's runtime-fallback path.
//
// Lives in this package (rather than imported from config) for the
// same dependency-arrow reason as Candidate — keeps internal/resolve
// from depending on internal/config.
type UserTierOverlay struct {
	// Name is the operator-declared tier name ("default" / "free" /
	// "low" / "medium" / "high" / etc.) — used only in error
	// messages so refusals cite WHICH tier blocked the request.
	Name string

	// ProviderPriority overlays the library order. See AgentRequest's
	// UserTier docstring for intersection semantics.
	ProviderPriority []string

	// Tiers overlays library Tiers (per-task-tier candidate lists).
	// Falls through library when this tier doesn't define candidates
	// for the requested task tier; agent.Models[tier] still takes
	// precedence on top of this.
	Tiers map[string][]Candidate

	// FallbackOnError + MaxFallbackAttempts are read by PR 2's loop;
	// the resolver doesn't act on them, but plumbing them on the
	// overlay keeps "everything about this user_tier in one place"
	// for callers downstream.
	FallbackOnError     bool
	MaxFallbackAttempts int
}

// Candidate is one (provider, model) pair in a tier's candidate list.
// Mirrors config.TierCandidate; see AgentRequest for the rationale on
// the duplicated type.
type Candidate struct {
	Provider string
	Model    string
}

// Resolver picks (provider, model) for an agent against the
// availability matrix. Construct one with NewResolver and pass it the
// library defaults; mutate the matrix via SetReachable / MarkStalled
// (PR 2 wires the periodic re-probe to call SetReachable; runtime
// failures call MarkStalled).
type Resolver struct {
	mu sync.RWMutex

	// libraryPriority is the library-wide provider priority order.
	// Falls back to defaultLibraryPriority when empty.
	libraryPriority []string

	// libraryTiers is the library-wide tier → candidates map.
	// Per-agent Models override this when set.
	libraryTiers map[string][]Candidate

	// matrix tracks (provider, model) availability. The outer key is
	// provider; the inner is model. A provider with no entry in the
	// outer map is treated as not-yet-probed (effectively unavailable
	// in PR 1's stub-probe world).
	matrix map[string]*Availability

	// forceProbe is the v0.8.17 hook the periodic probe loop sets so
	// out-of-band callers (POST /v1/_snapshots/{id}/restore handler)
	// can request an immediate matrix refresh. main.go's
	// runResolveProbeLoop sets this to a closure that triggers
	// runResolveProbeOnce; tests + callers that don't set it just
	// see ForceProbe() return immediately. Lock-free via mu —
	// SetForceProbeCallback writes under the write lock; ForceProbe
	// reads under the read lock.
	forceProbe func(ctx context.Context)
}

// Availability is the resolver's view of one provider's reachability
// plus per-model status. Mutated by SetReachable / SetExcluded /
// MarkStalled / the periodic probe loop.
type Availability struct {
	// Excluded means the provider was deliberately not probed
	// because no API key (or for Ollama, no base URL) was
	// configured. Distinct from Reachable=false (which means probe
	// attempted and failed) so operators reading Snapshot() can
	// tell "operator chose not to enable this provider" apart from
	// "provider is down right now". Resolver skips Excluded
	// providers identically to unreachable ones.
	Excluded bool

	// Reachable means the most recent probe to the provider's
	// endpoint succeeded. False when Excluded=true (we never
	// probed) or when the probe failed.
	Reachable bool

	// Models tracks per-model status. The outer Reachable check
	// gates ALL models for a provider — if the provider is
	// unreachable, no model on it is considered available, even if
	// Models[X].Listed is true from a prior probe.
	Models map[string]ModelStatus

	// LastCheck is when the matrix was last updated for this
	// provider — either by a probe or a stall feedback call.
	// Zero value when Excluded=true and the entry was never
	// updated from anything other than SetExcluded.
	LastCheck time.Time

	// LastError is the last failure reason (probe error or stall
	// reason). For excluded providers, contains the reason
	// (typically "no API key configured"). Surfaced in operator
	// logs and the 503 message so triage doesn't require a
	// separate trace.
	LastError string
}

// ModelStatus is one model's status under a provider. A model is
// usable iff (provider.Reachable && model.Listed && !model.Stalled).
type ModelStatus struct {
	// Listed means the provider's models endpoint surfaced this
	// model on the most recent probe. PR 1's stub probe pre-seeds
	// every configured tier candidate as listed when the provider
	// has an API key; PR 2's real probe gates this on the actual
	// /v1/models response.
	Listed bool

	// Stalled is set by MarkStalled when a runtime call failed in a
	// way that suggests the model itself is broken (404 on the model
	// name, 5xx after the rate-limit retry budget). Cleared by the
	// next successful probe of this provider.
	Stalled bool

	// LastError is the last failure for this specific model.
	// Independent of the provider-level LastError so an operator can
	// distinguish "DeepSeek is down" from "DeepSeek is up but
	// deepseek-v4-pro 404s".
	LastError string
}

// defaultLibraryPriority is the cost-floor-first ordering: try the
// cheapest reasonable backend first, escalate when stalled. Used when
// the operator hasn't set provider_priority in yaml.
//
// ollama-local (no auth, runs on a workstation) sits at the absolute
// floor — when an operator has a GPU on the network there's no reason
// to pay for the first attempt. Paid clouds escalate from cheap
// (DeepSeek) to premium (Anthropic). Hosted ollama.com (the `ollama`
// id since the v0.8.3 split) sits after the paid clouds because it's
// only sensible when the operator has explicitly paid for the
// quota — agents that want it will pin it via per-agent `providers:`.
var defaultLibraryPriority = []string{"ollama-local", "deepseek", "openai", "anthropic", "ollama"}

// NewResolver constructs a Resolver with the library-wide defaults.
// libraryPriority and libraryTiers come from the loaded Config; pass
// empty/nil for either to use the package defaults
// (defaultLibraryPriority for priority; nil tier map means tier-only
// requests will always return ErrTierUnavailable until library tiers
// are configured in yaml).
//
// Initial matrix is empty — every (provider, model) is unavailable
// until SetReachable is called. The HTTP server's startup probe (PR 2)
// or the stub-probe path (PR 1, in cmd/loomcycle/main.go) populates
// the matrix before traffic begins.
func NewResolver(libraryPriority []string, libraryTiers map[string][]Candidate) *Resolver {
	if len(libraryPriority) == 0 {
		libraryPriority = defaultLibraryPriority
	}
	return &Resolver{
		libraryPriority: libraryPriority,
		libraryTiers:    libraryTiers,
		matrix:          map[string]*Availability{},
	}
}

// Resolve picks a (Decision) for the agent, or returns a sentinel
// error. The decision is consultable but not authoritative — the
// driver may still fail at the wire, in which case the loop calls
// MarkStalled and resolves again (or fails out).
func (r *Resolver) Resolve(req AgentRequest) (Decision, error) {
	if req.Name == "" {
		// The Name field is for error messages, but a resolver
		// caller that forgets to set it is also likely to be
		// confused by other defaults. Fail fast in development.
		return Decision{}, fmt.Errorf("%w: agent name is required", ErrInvalidArgument)
	}

	// Pin path. Explicit provider+model bypasses tier resolution
	// entirely — caller asked for THAT model. Still gated by the
	// matrix so a stalled pin surfaces as ErrPinUnavailable rather
	// than the driver's 5xx.
	if req.PinProvider != "" && req.PinModel != "" {
		return r.resolvePin(req)
	}
	if req.PinProvider != "" || req.PinModel != "" {
		// Half a pin — provider without model or vice versa — is
		// almost certainly a config typo. The validator should
		// catch this at load time, but we double-check for cases
		// where AgentRequest is constructed directly (tests).
		return Decision{}, fmt.Errorf("%w: pin requires both provider and model (got provider=%q model=%q)",
			ErrInvalidArgument, req.PinProvider, req.PinModel)
	}

	// Tier path.
	if req.Tier == "" {
		return Decision{}, fmt.Errorf("%w: agent %q has neither pin nor tier", ErrInvalidArgument, req.Name)
	}

	candidates := r.candidatesFor(req)
	if len(candidates) == 0 {
		// Tier requested but no candidates configured for it —
		// either the library tier definition is empty or the
		// agent's Models override didn't include this tier. Treat
		// as unavailable so the operator gets a clear 503 rather
		// than a misleading "unknown agent" error.
		return Decision{}, fmt.Errorf("%w: agent %q tier %q has no candidates configured",
			ErrTierUnavailable, req.Name, req.Tier)
	}

	priority, refused := r.priorityFor(req)
	if refused {
		// Per-agent Providers ∩ user_tier.ProviderPriority is empty.
		// This is an operator policy refusal (option A in the v0.8.2
		// design): the agent demands providers the user_tier doesn't
		// grant. Distinct from a transient outage; the client can
		// render "upgrade required" without retry.
		return Decision{}, fmt.Errorf("%w: agent %q requires providers %v; user_tier %q grants %v",
			ErrTierAgentNotAvailable, req.Name, req.Providers, req.UserTier.Name, req.UserTier.ProviderPriority)
	}
	if len(priority) == 0 {
		// No effective priority — defaults must be wrong or user_tier
		// has an empty provider_priority. Treat as outage shape.
		return Decision{}, fmt.Errorf("%w: agent %q tier %q has no effective provider priority",
			ErrTierUnavailable, req.Name, req.Tier)
	}

	// Pre-check: do any candidates list a provider that's in the
	// effective priority? If the operator's agent.Models[tier] only
	// lists providers excluded by user_tier (e.g. anthropic-pinned
	// cv-adapter + free tier with no anthropic), refuse with the
	// policy-refusal shape — operator's intent is clear, the
	// resolver shouldn't surface it as a transient outage that the
	// client might retry.
	if req.UserTier != nil {
		hasViableCandidate := false
		for _, p := range priority {
			for _, cand := range candidates {
				if cand.Provider == p {
					hasViableCandidate = true
					break
				}
			}
			if hasViableCandidate {
				break
			}
		}
		if !hasViableCandidate {
			return Decision{}, fmt.Errorf("%w: agent %q tier %q candidates do not include any provider granted by user_tier %q",
				ErrTierAgentNotAvailable, req.Name, req.Tier, req.UserTier.Name)
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, providerID := range priority {
		for _, cand := range candidates {
			if cand.Provider != providerID {
				continue
			}
			if r.isAvailableLocked(cand.Provider, cand.Model) {
				return Decision{
					Provider: cand.Provider,
					Model:    cand.Model,
					Effort:   req.Effort,
				}, nil
			}
		}
	}
	return Decision{}, fmt.Errorf("%w: agent %q tier %q (no reachable provider with a non-stalled model)",
		ErrTierUnavailable, req.Name, req.Tier)
}

// resolvePin consults the matrix for the explicit pin and returns
// either the Decision or ErrPinUnavailable. Caller already checked
// that both PinProvider and PinModel are set.
func (r *Resolver) resolvePin(req AgentRequest) (Decision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.isAvailableLocked(req.PinProvider, req.PinModel) {
		return Decision{}, fmt.Errorf("%w: agent %q wants %s/%s",
			ErrPinUnavailable, req.Name, req.PinProvider, req.PinModel)
	}
	return Decision{
		Provider: req.PinProvider,
		Model:    req.PinModel,
		Effort:   req.Effort,
	}, nil
}

// candidatesFor returns the candidate list the resolver should walk
// for this request, applying the v0.8.2 overlay precedence:
//
//	per-agent Models[Tier]   (highest)
//	user_tier overlay Tiers  (when set; v0.8.2)
//	library Tiers            (fallback)
//
// Caller has already validated req.Tier is non-empty.
func (r *Resolver) candidatesFor(req AgentRequest) []Candidate {
	if cands, ok := req.Models[req.Tier]; ok && len(cands) > 0 {
		return cands
	}
	if req.UserTier != nil {
		if cands, ok := req.UserTier.Tiers[req.Tier]; ok && len(cands) > 0 {
			return cands
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if cands, ok := r.libraryTiers[req.Tier]; ok {
		return cands
	}
	return nil
}

// priorityFor returns the provider-priority order the resolver should
// walk for this request, applying the v0.8.2 overlay precedence:
//
//	per-agent Providers      (highest, but intersected with user_tier)
//	user_tier ProviderPriority (when set; v0.8.2)
//	library ProviderPriority (fallback)
//
// The second return value is true when the per-agent Providers and the
// user_tier overlay's ProviderPriority have an empty intersection.
// This is the option-A refusal path — caller propagates as
// ErrTierAgentNotAvailable so the client surfaces "upgrade required"
// rather than "transient outage."
func (r *Resolver) priorityFor(req AgentRequest) (order []string, refused bool) {
	switch {
	case len(req.Providers) > 0 && req.UserTier != nil:
		// Intersection: filter agent.Providers, keep only those also
		// in the user_tier overlay. Walk in agent's declared order so
		// the per-agent operator intent (e.g. "anthropic first, then
		// deepseek") is preserved within the tier-restricted space.
		allowed := make(map[string]struct{}, len(req.UserTier.ProviderPriority))
		for _, p := range req.UserTier.ProviderPriority {
			allowed[p] = struct{}{}
		}
		out := make([]string, 0, len(req.Providers))
		for _, p := range req.Providers {
			if _, ok := allowed[p]; ok {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return nil, true
		}
		return out, false
	case len(req.Providers) > 0:
		return req.Providers, false
	case req.UserTier != nil && len(req.UserTier.ProviderPriority) > 0:
		return req.UserTier.ProviderPriority, false
	}
	return r.libraryPriority, false
}

// isAvailableLocked checks whether a (provider, model) is currently
// usable. Caller holds r.mu (read or write).
func (r *Resolver) isAvailableLocked(provider, model string) bool {
	avail, ok := r.matrix[provider]
	if !ok || avail.Excluded || !avail.Reachable {
		return false
	}
	status, ok := avail.Models[model]
	if !ok {
		return false
	}
	return status.Listed && !status.Stalled
}

// SetReachable updates the matrix for a probe outcome. PR 1 calls
// this from cmd/loomcycle/main.go's stub probe (API key present →
// Reachable=true, all configured tier candidates pre-listed); PR 2's
// periodic probe calls this with the real /v1/models response. Pass
// listedModels=nil to mark the provider unreachable while preserving
// the prior model list (for transient probe failures).
//
// Calling SetReachable for a provider also clears any per-model Stalled
// flag for models that show up in listedModels — a model coming back
// from the wire is the resolver's signal that the runtime feedback was
// transient. Models not in listedModels keep their prior Stalled flag
// (the absence of a model from /v1/models doesn't necessarily mean
// "stalled" — it might mean "not entitled" — so we don't clear that
// way).
func (r *Resolver) SetReachable(provider string, reachable bool, listedModels []string, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail, ok := r.matrix[provider]
	if !ok {
		avail = &Availability{Models: map[string]ModelStatus{}}
		r.matrix[provider] = avail
	}
	// SetReachable means a probe ran, which conceptually retracts
	// the "deliberately not probed" Excluded marker. If the probe
	// failed, Reachable goes false but Excluded stays cleared —
	// "we tried, it didn't work" is distinct from "we didn't try".
	avail.Excluded = false
	avail.Reachable = reachable
	avail.LastCheck = time.Now()
	avail.LastError = lastErr

	if listedModels == nil {
		// Transient probe failure — keep the prior model list so a
		// hiccup doesn't blank out availability. The Reachable=false
		// flag above is enough to gate Resolve.
		return
	}

	// Build a fresh per-model map. Models in listedModels gain
	// Listed=true and lose Stalled. Models that were in the prior
	// map but not in listedModels are dropped — they're not on the
	// provider anymore.
	newModels := map[string]ModelStatus{}
	listed := map[string]bool{}
	for _, m := range listedModels {
		listed[m] = true
	}
	for m := range listed {
		newModels[m] = ModelStatus{Listed: true} // Stalled cleared on re-probe
	}
	avail.Models = newModels
}

// SeedModel inserts a model into the matrix as listed for a provider,
// without mutating the provider's reachability or LastCheck. Used by
// PR 1's stub probe to pre-seed every configured tier candidate as
// listed (since we don't have a real /v1/models response yet); also
// used by tests. PR 2's live probe will use SetReachable instead and
// SeedModel will primarily be a test-fixture helper.
func (r *Resolver) SeedModel(provider, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail, ok := r.matrix[provider]
	if !ok {
		avail = &Availability{Models: map[string]ModelStatus{}}
		r.matrix[provider] = avail
	}
	if avail.Models == nil {
		avail.Models = map[string]ModelStatus{}
	}
	avail.Models[model] = ModelStatus{Listed: true}
}

// SetProviderReachable is a SeedModel companion: marks a provider as
// reachable without touching the model list. PR 1's stub probe uses
// it after seeding the configured candidates.
func (r *Resolver) SetProviderReachable(provider string, reachable bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail, ok := r.matrix[provider]
	if !ok {
		avail = &Availability{Models: map[string]ModelStatus{}}
		r.matrix[provider] = avail
	}
	avail.Reachable = reachable
	avail.LastCheck = time.Now()
}

// SetExcluded marks a provider as deliberately not probed — no API
// key configured, or for Ollama no base URL. The resolver skips
// excluded providers identically to unreachable ones, but Snapshot()
// surfaces the distinct flag so operators can tell "operator chose
// not to enable" from "operator enabled but the probe failed".
//
// Reason is surfaced in LastError for operator logs (typical values:
// "ANTHROPIC_API_KEY not set", "DEEPSEEK_API_KEY not set",
// "OLLAMA_BASE_URL not configured").
//
// Calling SetExcluded clears Reachable (an excluded provider is
// definitionally not reachable) and is idempotent — safe to call on
// every probe sweep without side effects beyond updating LastCheck
// and LastError.
func (r *Resolver) SetExcluded(provider, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail, ok := r.matrix[provider]
	if !ok {
		avail = &Availability{Models: map[string]ModelStatus{}}
		r.matrix[provider] = avail
	}
	avail.Excluded = true
	avail.Reachable = false
	avail.LastCheck = time.Now()
	avail.LastError = reason
}

// MarkStalled records a runtime failure for a (provider, model). The
// loop should call this on first 5xx-after-retry or 404-on-model-name
// — the model is presumed broken until the next successful probe
// clears the flag (PR 2 wires the probe → clear path; in PR 1, only a
// fresh SeedModel / SetReachable call resets the flag).
//
// Stall is per-model, not per-provider, so a single bad model on
// DeepSeek doesn't take down the whole driver — the resolver just
// skips that candidate and moves to the next.
func (r *Resolver) MarkStalled(provider, model, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail, ok := r.matrix[provider]
	if !ok {
		// Stalling a provider that was never seen is suspicious but
		// not worth a hard error — the runtime feedback path is
		// best-effort. Create the entry so the next Resolve at
		// least sees the stall flag.
		avail = &Availability{Models: map[string]ModelStatus{}}
		r.matrix[provider] = avail
	}
	if avail.Models == nil {
		avail.Models = map[string]ModelStatus{}
	}
	st := avail.Models[model]
	st.Stalled = true
	st.LastError = reason
	avail.Models[model] = st
	avail.LastCheck = time.Now()
}

// ClearStall records a runtime SUCCESS for a (provider, model) and
// clears any stale Stalled flag the matrix may be holding. The loop
// calls this when an iteration completes without error against the
// pair — the most direct possible evidence that the model is healthy
// right now.
//
// Without this, a per-model stall flag was process-lifetime: it
// persisted until the next periodic probe (default several minutes)
// even if the operator had since had successful calls against the
// same pair. Observed 2026-05-15: a free user_tier with two
// candidates collapsed into a 503 because both were stalled by
// transient failures, and the staleness outlasted the next call's
// resolve attempt. Clear-on-success eliminates that class of bug.
//
// Idempotent: clearing a non-stalled or non-existent (provider, model)
// is a no-op. Doesn't touch Listed (the probe owns that field) or
// the provider-level Reachable flag.
func (r *Resolver) ClearStall(provider, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail, ok := r.matrix[provider]
	if !ok || avail.Models == nil {
		return
	}
	st, ok := avail.Models[model]
	if !ok || !st.Stalled {
		return
	}
	st.Stalled = false
	st.LastError = ""
	avail.Models[model] = st
}

// Snapshot returns a read-only copy of the current matrix for
// observability (operator dashboards, /healthz extension, debug logs).
// Cheap enough to call on every healthz hit; the inner maps are
// shallow-copied so callers can't mutate resolver state. Per-model
// status structs are copied by value.
func (r *Resolver) Snapshot() map[string]Availability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Availability, len(r.matrix))
	for provider, avail := range r.matrix {
		models := make(map[string]ModelStatus, len(avail.Models))
		for m, s := range avail.Models {
			models[m] = s
		}
		out[provider] = Availability{
			Excluded:  avail.Excluded,
			Reachable: avail.Reachable,
			Models:    models,
			LastCheck: avail.LastCheck,
			LastError: avail.LastError,
		}
	}
	return out
}

// SetForceProbeCallback installs the closure ForceProbe invokes.
// Used by cmd/loomcycle/main.go to wire the probe loop's
// runResolveProbeOnce as the immediate-probe trigger. Callers that
// don't set this see ForceProbe() return immediately (matrix stays
// stale until the next periodic sweep).
func (r *Resolver) SetForceProbeCallback(fn func(ctx context.Context)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forceProbe = fn
}

// ForceProbe triggers an immediate refresh of the resolver matrix.
// Called by the snapshot restore handler so the resolver's view of
// provider availability is populated before the operator calls
// Resume — the pause-resume-snapshot RFC excludes the resolver
// state from snapshots (re-probe on restore).
//
// Blocking: returns when the underlying probe completes. Operator
// behind a slow / unreachable provider sees the restore response
// wait briefly (~3-5s in the worst case); main.go's probe loop has
// per-provider timeouts that bound this.
//
// No-op when SetForceProbeCallback hasn't been called (tests, or
// runtime configurations without a probe loop).
func (r *Resolver) ForceProbe(ctx context.Context) {
	r.mu.RLock()
	fn := r.forceProbe
	r.mu.RUnlock()
	if fn == nil {
		return
	}
	fn(ctx)
}
