package config

import "strings"

// MetricModelNames is the set of model names the operator configured: the
// values the runtime's metrics may carry as a model label. Every other model
// — a gateway caller's pinned model, a tenant-authored agent's — is counted
// under one "other" label, so nobody but the operator can grow the series
// set. names holds aliases and concrete models from defaults, models,
// tiers, user tiers, yaml agents and the pricing table; patterns holds the
// model_pattern globs aliases resolve against a provider's catalog. Both are
// sorted and free of duplicates.
func (c *Config) MetricModelNames() (names, patterns []string) {
	if c == nil {
		return nil, nil
	}
	set := map[string]bool{}
	pats := map[string]bool{}
	add := func(m string) {
		if m = strings.TrimSpace(m); m != "" {
			set[m] = true
		}
	}
	addCandidates := func(tiers map[string][]TierCandidate) {
		for _, cands := range tiers {
			for _, cand := range cands {
				add(cand.Model)
			}
		}
	}
	add(c.Defaults.Model)
	for alias, ref := range c.Models {
		add(alias)
		add(ref.Model)
		if p := strings.TrimSpace(ref.ModelPattern); p != "" {
			pats[p] = true
		}
	}
	addCandidates(c.Tiers)
	for _, ut := range c.UserTiers {
		addCandidates(ut.Tiers)
	}
	for _, a := range c.Agents {
		add(a.Model)
		addCandidates(a.Models)
	}
	for key := range c.Pricing.Models {
		// "provider/model": the model may itself hold a "/".
		if _, m, ok := strings.Cut(key, "/"); ok {
			add(m)
		}
	}
	return sortedKeys(set), sortedKeys(pats)
}
