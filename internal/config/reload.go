package config

import (
	"errors"
	"sort"

	"gopkg.in/yaml.v3"
)

// ErrReloadInvalid classifies a runtime config reload (RFC CK) that was rejected
// because the candidate config failed to load or validate — the running config is
// left untouched. The HTTP layer maps it to 422.
var ErrReloadInvalid = errors.New("config reload: candidate invalid")

// ErrReloadApply classifies a reload whose candidate validated but whose
// subsystem apply failed (e.g. a provider driver that would not construct). The
// running subsystems keep the previous config. Mapped to 500.
var ErrReloadApply = errors.New("config reload: apply failed")

// ChangedSections reports which top-level config sections differ between two
// loaded configs (RFC CK runtime reload). It marshals each config to YAML and
// compares the serialized value of every top-level key, returning the changed
// keys sorted. This is a whole-config diff by section name (providers / models /
// tiers / agents / memory / concurrency / …) that a reload engine uses to decide
// what to apply vs report as restart-required, without enumerating every field.
//
// Both configs are produced by the SAME process env (env changes need a restart),
// so env-derived fields serialize identically and are not flagged — only sections
// an operator actually edited in the YAML surface as changed. A marshal error on
// either side yields ("", err); callers treat that as "cannot diff" and refuse.
func ChangedSections(oldCfg, newCfg *Config) ([]string, error) {
	oldSecs, err := sectionBytes(oldCfg)
	if err != nil {
		return nil, err
	}
	newSecs, err := sectionBytes(newCfg)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for k := range oldSecs {
		seen[k] = struct{}{}
	}
	for k := range newSecs {
		seen[k] = struct{}{}
	}
	var changed []string
	for k := range seen {
		if oldSecs[k] != newSecs[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// sectionBytes marshals a config to YAML and returns each top-level key mapped to
// its re-marshaled (canonical) value bytes, so two values compare equal iff they
// serialize identically regardless of map ordering.
func sectionBytes(c *Config) (map[string]string, error) {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(top))
	for k, node := range top {
		vb, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		out[k] = string(vb)
	}
	return out, nil
}
