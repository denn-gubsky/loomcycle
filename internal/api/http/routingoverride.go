package http

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// routingOverride is a run's own answer to "which model should this run use",
// chosen by the caller instead of by the agent definition.
//
// It is RUN state, not definition state: it lives in the run's persisted
// configuration record, survives a pause, and never touches `content_sha256`.
// Two agents differing only by a routing override are the same definition.
//
// What it may NAME is bounded by the definition (see validate): the operator
// declares which providers and models an agent may reach, and an override
// selects WITHIN that set rather than widening it. That keeps data residency —
// which vendor sees the conversation — an operator decision, while letting a
// caller pick among the vendors the operator already allowed.
type routingOverride struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

func (o *routingOverride) isZero() bool {
	return o == nil || (o.Provider == "" && o.Model == "" && o.Tier == "" && o.Effort == "")
}

// knownEfforts is the set the drivers translate. An unknown value is refused at
// the wire rather than silently dropped, because "effort was ignored" and
// "effort was applied" are indistinguishable from the outside.
var knownEfforts = map[string]bool{"low": true, "medium": true, "high": true}

// applyRoutingOverride returns the definition this RUN should resolve against.
//
// It never mutates the stored definition — it returns a copy — which is what
// keeps an override out of the content hash. The caller resolves against the
// result exactly as it would have resolved against the original.
//
// The rules, and why each is what it is:
//
//   - tier / effort replace the definition's own. Both are selections within
//     operator configuration, so they are checked against it.
//   - provider alone NARROWS: on a tiered agent it restricts the cascade to
//     that one vendor and the tier still picks the model and still falls back
//     within it. Clearing the tier here would silently trade fallback for a
//     pin the caller did not ask for.
//   - model PINS. "Use this model" is the request; a cascade that might route
//     elsewhere is not an answer to it. The tier is cleared deliberately, and
//     that trade — losing the cascade — is the caller's to make by naming a
//     model.
func (s *Server) applyRoutingOverride(ctx context.Context, def config.AgentDef, ov *routingOverride) (config.AgentDef, error) {
	if ov.isZero() {
		return def, nil
	}
	cfg := s.cfg()

	if ov.Effort != "" {
		if !knownEfforts[ov.Effort] {
			return def, fmt.Errorf("%w: effort %q is not one of low, medium, high", runner.ErrInvalidArgument, ov.Effort)
		}
		def.Effort = ov.Effort
	}

	if ov.Tier != "" {
		if len(cfg.Tiers) > 0 {
			if _, ok := cfg.Tiers[ov.Tier]; !ok {
				return def, fmt.Errorf("%w: tier %q is not configured (have: %s)",
					runner.ErrInvalidArgument, ov.Tier, configuredTierNames(cfg.Tiers))
			}
		}
		def.Tier = ov.Tier
		// A tier and a pin are mutually exclusive in a definition, and the
		// caller just chose the tier.
		def.Provider, def.Model = "", ""
	}

	if ov.Provider != "" {
		if err := s.checkProviderAllowed(def, ov.Provider); err != nil {
			return def, err
		}
		if def.Tier != "" {
			// Narrow the cascade to this vendor; the tier still chooses the
			// model within it and still falls back within it.
			def.Providers = []string{ov.Provider}
		} else {
			def.Provider = ov.Provider
		}
	}

	if ov.Model != "" {
		provider, err := s.checkModelAllowed(ctx, def, ov.Model, ov.Provider)
		if err != nil {
			return def, err
		}
		def.Model = ov.Model
		if provider != "" {
			def.Provider = provider
		}
		// Naming a model is a pin: it answers the routing question outright.
		def.Tier = ""
	}
	return def, nil
}

// checkProviderAllowed enforces the definition's declared provider set. An
// empty declaration means the operator did not restrict this agent, so the
// override is bounded only by what the deployment actually has configured —
// an override still cannot invent a provider.
func (s *Server) checkProviderAllowed(def config.AgentDef, provider string) error {
	if len(def.Providers) > 0 && !contains(def.Providers, provider) {
		return fmt.Errorf("%w: provider %q is not among the ones agent's definition allows (%s)",
			runner.ErrInvalidArgument, provider, strings.Join(def.Providers, ", "))
	}
	if s.providers != nil {
		if _, err := s.providers.Get(provider); err != nil {
			return fmt.Errorf("%w: provider %q is not configured on this deployment", runner.ErrInvalidArgument, provider)
		}
	}
	return nil
}

// checkModelAllowed enforces the definition's declared model set and, when it
// can, reports which provider serves the chosen model.
//
// Three cases, in the order they are checked:
//
//  1. the definition declares `models:` candidates — the override must name one
//     of them. This is the operator's explicit allowlist and it wins.
//  2. the definition routes by TIER with no explicit candidates — the live
//     cascade is the allowlist, and it also tells us the provider. Reusing
//     Cascade rather than matching config means this cannot drift from what
//     Resolve would actually pick.
//  3. neither — the operator did not restrict this agent, so any model its
//     provider serves is fair game. The caller supplies the provider or the
//     definition's own pin does.
func (s *Server) checkModelAllowed(ctx context.Context, def config.AgentDef, model, wantProvider string) (string, error) {
	if len(def.Models) > 0 {
		for _, cands := range def.Models {
			for _, c := range cands {
				if c.Model == model {
					if c.Provider != "" && wantProvider == "" {
						return c.Provider, nil
					}
					return wantProvider, nil
				}
			}
		}
		return "", fmt.Errorf("%w: model %q is not among the ones agent's definition allows",
			runner.ErrInvalidArgument, model)
	}

	if def.Tier != "" {
		if provider, ok := s.providerForModel(ctx, def, "", "", "", "", false, model); ok {
			if wantProvider != "" {
				return wantProvider, nil
			}
			return provider, nil
		}
		return "", fmt.Errorf("%w: model %q is not reachable from tier %q on this deployment",
			runner.ErrInvalidArgument, model, def.Tier)
	}
	return wantProvider, nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// configuredTierNames lists the deployment's tiers for an error message, so a
// caller who names a tier that does not exist is told what does.
func configuredTierNames(tiers map[string][]config.TierCandidate) string {
	out := make([]string, 0, len(tiers))
	for k := range tiers {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// persistedRouting drops an all-empty override so a run that chose no routing
// records none. "No override" and "an override that says nothing" are the same
// fact, and storing the second shape would make every run's record non-empty
// for no gain.
func persistedRouting(ov *routingOverride) *routingOverride {
	if ov.isZero() {
		return nil
	}
	return ov
}
