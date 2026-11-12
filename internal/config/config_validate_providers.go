package config

// The blocks validate checks, in its order; see validate.

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/search"
)

// validateProviderPriority checks the library-level provider priority against
// the known providers. Part of validate.
func validateProviderPriority(c *Config, known map[string]bool) error {
	knownList := make([]string, 0, len(known))
	for id := range known {
		knownList = append(knownList, id)
	}
	sort.Strings(knownList)
	// Library-level provider priority — validate every entry is a
	// known provider name. Empty list is fine (resolver falls back
	// to its hardcoded default order).
	for i, p := range c.ProviderPriority {
		if !known[p] {
			return fmt.Errorf("provider_priority[%d]: unknown provider %q (known: %v)", i, p, knownList)
		}
	}
	return nil
}

// validateSearchProviders checks the enabled search providers and the search
// priority, and returns the enabled set the agents are checked against. Part
// of validate.
func validateSearchProviders(c *Config) (map[string]bool, error) {
	// Search providers (RFC BB): validate the enabled set against the known
	// drivers + SearXNG's required base_url, then the global priority. The
	// enabled set is reused below to validate each agent's search_providers.
	knownSearch := map[string]bool{}
	for _, id := range search.KnownProviderIDs() {
		knownSearch[id] = true
	}
	enabledSearch := map[string]bool{}
	for id, spc := range c.SearchProviders {
		if !knownSearch[id] {
			return nil, fmt.Errorf("search_providers: unknown provider %q (want one of %v)", id, search.KnownProviderIDs())
		}
		if id == "searxng" && strings.TrimSpace(spc.BaseURL) == "" {
			return nil, fmt.Errorf("search_providers.searxng: base_url is required for the self-hosted SearXNG provider")
		}
		enabledSearch[id] = true
	}
	for i, id := range c.SearchPriority {
		if !enabledSearch[id] {
			return nil, fmt.Errorf("search_priority[%d]: %q is not an enabled search_providers entry", i, id)
		}
	}
	return enabledSearch, nil
}

// validateProviderRegistry checks the declared providers against the
// compiled-in drivers, then the AWS and Bashbox credential allowlists. Part
// of validate.
func validateProviderRegistry(c *Config) error {
	// RFC BF provider registry (P2a): structural + registry-aware validation. Each
	// declared entry needs a driver compiled into the binary (∈ RegisteredDrivers),
	// a dialect the driver speaks (when set), and a non-negative concurrency cap.
	// The driver registry is populated by the driver packages' init() — present in
	// the server binary (blank imports in cmd/loomcycle) and the config test binary
	// (blank imports in drivers_registry_test.go). An absent `providers:` block
	// skips the loop, so a config that declares none validates unchanged.
	for id, pc := range c.Providers {
		if strings.TrimSpace(pc.Driver) == "" {
			return fmt.Errorf("providers.%s: driver is required", id)
		}
		if pc.MaxConcurrent < 0 {
			return fmt.Errorf("providers.%s: max_concurrent must be >= 0", id)
		}
		if err := pc.validateLanes(id, c.Models); err != nil {
			return err
		}
		if err := pc.validateAuth(id); err != nil {
			return err
		}
		dialects, ok := providers.DriverDialects(pc.Driver)
		if !ok {
			return fmt.Errorf("providers.%s: unknown driver %q (compiled-in: %v)", id, pc.Driver, providers.RegisteredDrivers())
		}
		if pc.Dialect != "" {
			supported := false
			for _, d := range dialects {
				if d == pc.Dialect {
					supported = true
					break
				}
			}
			if !supported {
				return fmt.Errorf("providers.%s: driver %q does not speak dialect %q (supported: %v)", id, pc.Driver, pc.Dialect, dialects)
			}
		}
	}
	if err := validateAWSSecretAllowlists(c); err != nil {
		return err
	}
	if err := validateBashboxFallbackCreds(c); err != nil {
		return err
	}
	return nil
}

// validateModelAliases checks the top-level models: aliases. Part of validate.
func validateModelAliases(c *Config, known map[string]bool) error {
	// Model aliases (top-level models:) — a non-empty provider on an alias must be a
	// known provider (RFC BF P2a; an empty provider defers to the pin/candidate that
	// names the alias, so it is left unvalidated here). Built-ins always pass via the
	// floor, so this only newly rejects a typo'd or undeclared 3rd-party provider.
	for name, ref := range c.Models {
		// RFC BG: exactly one of model / model_pattern. Both or neither is a
		// config error — a pattern alias has no concrete model, a concrete alias
		// has no glob, and an empty alias resolves to nothing.
		hasModel := ref.Model != ""
		hasPattern := ref.ModelPattern != ""
		if hasModel == hasPattern {
			return fmt.Errorf("models.%s: exactly one of model or model_pattern is required (got model=%q model_pattern=%q)", name, ref.Model, ref.ModelPattern)
		}
		if hasPattern {
			// A pattern is scoped to one provider's catalog, so the provider can't
			// be deferred to the referencing candidate the way a concrete alias can.
			if ref.Provider == "" {
				return fmt.Errorf("models.%s: model_pattern requires an explicit provider", name)
			}
			// Reject a malformed glob at load, not at first resolve. path.Match's
			// only error is ErrBadPattern; the empty name still forces a full scan
			// of the pattern's syntax (e.g. an unterminated character class).
			if _, perr := path.Match(ref.ModelPattern, ""); perr != nil {
				return fmt.Errorf("models.%s: invalid model_pattern %q: %v", name, ref.ModelPattern, perr)
			}
		}
		if ref.Provider != "" && !known[ref.Provider] {
			return fmt.Errorf("models.%s: unknown provider %q", name, ref.Provider)
		}
		switch ref.Kind {
		case "", ModelKindChat, ModelKindDecision, ModelKindEmbedder, ModelKindTranscriber, ModelKindSynthesizer:
		default:
			return fmt.Errorf("models.%s: kind %q is not one of chat, decision, embedder, transcriber, synthesizer", name, ref.Kind)
		}
		switch ref.License {
		case "", ModelLicenseCommercial, ModelLicenseNoncommercial:
		default:
			return fmt.Errorf("models.%s: license %q is not one of commercial, noncommercial", name, ref.License)
		}
	}
	return nil
}

// validateTiers checks the library and user tier definitions. Part of
// validate.
func validateTiers(c *Config, known map[string]bool) error {
	// Library-level tier definitions.
	for tierName, candidates := range c.Tiers {
		if !validTierNames[tierName] {
			return fmt.Errorf("tiers.%s: unknown tier (want one of low/middle/high)", tierName)
		}
		for i, cand := range candidates {
			if err := validateTierCandidate(cand, c.Models, known); err != nil {
				return fmt.Errorf("tiers.%s[%d]: %v", tierName, i, err)
			}
		}
	}
	// User-tier definitions (v0.8.2). When the map is populated, a
	// "default" entry is required so requests without a user_tier
	// field have a defined policy to fall through to. Each tier's
	// internal shape is validated with the same rules as the library
	// ProviderPriority + Tiers above — duplication is intentional so
	// the errors point at the offending user_tiers.<name> path rather
	// than a generic message.
	if len(c.UserTiers) > 0 {
		if _, ok := c.UserTiers["default"]; !ok {
			return fmt.Errorf(`user_tiers: a "default" entry is required when the user_tiers block is populated (covers requests without user_tier and back-compat with v0.7.x clients)`)
		}
		for tierName, ut := range c.UserTiers {
			if tierName == "" {
				return fmt.Errorf("user_tiers: empty tier name")
			}
			for i, p := range ut.ProviderPriority {
				if !known[p] {
					return fmt.Errorf("user_tiers.%s.provider_priority[%d]: unknown provider %q", tierName, i, p)
				}
			}
			for taskTier, candidates := range ut.Tiers {
				if !validTierNames[taskTier] {
					return fmt.Errorf("user_tiers.%s.tiers.%s: unknown tier (want one of low/middle/high)", tierName, taskTier)
				}
				for i, cand := range candidates {
					if err := validateTierCandidate(cand, c.Models, known); err != nil {
						return fmt.Errorf("user_tiers.%s.tiers.%s[%d]: %v", tierName, taskTier, i, err)
					}
				}
			}
			if ut.MaxFallbackAttempts < 0 {
				return fmt.Errorf("user_tiers.%s.max_fallback_attempts: must be >= 0 (0 = use default of 3)", tierName)
			}
			if ut.RateLimitCooldownMs < 0 {
				return fmt.Errorf("user_tiers.%s.rate_limit_cooldown_ms: must be >= 0 (0 = use resolver default of 30000ms)", tierName)
			}
		}
	}
	return nil
}
