package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/decision"
)

// DecisionConfig is the top-level `decision:` block: the decision models this
// deployment may ask, and the one asked when a caller names none. Unset, there
// are none and nothing asks one.
//
// It is a list, not one model, because decision models trade speed for context
// (a small one answers in tens of milliseconds over a few thousand tokens; a
// larger one reads twice as much), and which fits is the caller's question to
// answer per call. It is the OPERATOR'S list because each entry is a model on
// an account the operator pays for: a caller picks among the entries by name
// and cannot reach a model outside them.
type DecisionConfig struct {
	// Default is the model asked when a caller names none: a models: alias, or a
	// model name served by Provider. Required when the block is set.
	Default string `yaml:"default"`
	// Models are the models a caller may name, written like Default. Omitted, it
	// is every models: alias tagged `kind: decision`. Default is always allowed,
	// listed or not.
	Models []string `yaml:"models"`
	// Provider serves the entries that are plain model names. An alias carries
	// its own provider and ignores this.
	Provider string `yaml:"provider"`
	// TimeoutMs bounds one call, including its wait for a slot. 0 = 30000.
	TimeoutMs int `yaml:"timeout_ms"`
	// MaxConcurrent bounds the calls in flight to each provider the block uses.
	// 0 = 4. It is not a provider run slot: a decision is asked from inside a run
	// that already holds one.
	MaxConcurrent int `yaml:"max_concurrent"`
}

// UnmarshalYAML refuses a key the block does not have. A dropped key here is a
// silent change of meaning (`model: nimble` for `default: nimble` would leave
// the block without the model the operator named), and the rest of the config
// is decoded leniently, so the block checks its own keys. The key set is read
// off the struct, so a field added later is accepted without editing this.
func (d *DecisionConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		known := map[string]bool{}
		t := reflect.TypeOf(*d)
		for i := 0; i < t.NumField(); i++ {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			known[name] = true
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if key := n.Content[i].Value; !known[key] {
				names := make([]string, 0, len(known))
				for k := range known {
					names = append(names, k)
				}
				sort.Strings(names)
				return fmt.Errorf("decision: unknown key %q (the block takes: %s)", key, strings.Join(names, ", "))
			}
		}
	}
	type plain DecisionConfig // no UnmarshalYAML, so the decode does not recurse
	return n.Decode((*plain)(d))
}

// Configured reports whether the block is written at all. Derived from the
// struct, so a field added later is covered without editing this.
func (d DecisionConfig) Configured() bool { return !reflect.ValueOf(d).IsZero() }

// DecisionEntry is one model the decision block allows: the name as the operator
// wrote it, and the provider to resolve it with ("" for a models: alias, which
// carries its own).
type DecisionEntry struct {
	Name     string
	Provider string
}

// DecisionEntries are the models the decision block allows, in the order
// written (or by name, when the list is the tagged aliases), each once, with
// the default among them. Empty when the block is unset. It is the one place
// the list is decided, so what load validates is what boot builds.
func (c *Config) DecisionEntries() []DecisionEntry {
	d := c.Decision
	if !d.Configured() {
		return nil
	}
	names := append([]string(nil), d.Models...)
	if d.Models == nil {
		for name, ref := range c.Models {
			if ref.Kind == ModelKindDecision {
				names = append(names, name)
			}
		}
		sort.Strings(names)
	}
	names = append(names, d.Default)
	seen := map[string]bool{}
	var out []DecisionEntry
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		e := DecisionEntry{Name: name}
		if _, isAlias := c.Models[name]; !isAlias {
			e.Provider = d.Provider
		}
		out = append(out, e)
	}
	return out
}

// validate refuses a block that could not be built, so `loomcycle validate`
// says so and not the first call.
func (d DecisionConfig) validate(c *Config) error {
	if !d.Configured() {
		return nil
	}
	if d.Default == "" {
		return fmt.Errorf("decision.default is required when the decision block is set")
	}
	if d.TimeoutMs < 0 {
		return fmt.Errorf("decision.timeout_ms must be >= 0")
	}
	if d.MaxConcurrent < 0 {
		return fmt.Errorf("decision.max_concurrent must be >= 0")
	}
	declared := func() []string {
		ids := make([]string, 0, len(c.Providers))
		for id := range c.Providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
	}
	if _, ok := c.Providers[d.Provider]; d.Provider != "" && !ok {
		return fmt.Errorf("decision.provider: %q is not declared in providers (declared: %v)", d.Provider, declared())
	}
	for _, e := range c.DecisionEntries() {
		place := "decision.models"
		switch {
		case e.Name == d.Default:
			place = "decision.default"
		case d.Models == nil:
			// Nobody wrote this entry: say why it is being checked.
			place = "decision (no models listed, so every alias tagged kind: decision is allowed)"
		}
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("%s: an entry is empty", place)
		}
		if err := c.requireModelKind(place, e.Name, ModelKindDecision); err != nil {
			return err
		}
		_, isAlias := c.Models[e.Name]
		if !isAlias && d.Provider == "" {
			return fmt.Errorf("%s: %q is not a models: alias, so decision.provider must name the provider that serves it", place, e.Name)
		}
		provider, _, err := c.ExpandServiceModel("decision", e.Provider, e.Name)
		if err != nil {
			return err
		}
		if provider == "" {
			return fmt.Errorf("%s: models.%s names no provider", place, e.Name)
		}
		pc, ok := c.Providers[provider]
		if !ok {
			return fmt.Errorf("%s: %q is served by provider %q, which is not declared in providers (declared: %v)", place, e.Name, provider, declared())
		}
		served := false
		for _, driver := range decision.Registered() {
			served = served || driver == pc.Driver
		}
		if !served {
			return fmt.Errorf("%s: %q is served by provider %q, whose %q driver serves no decision models (drivers that do: %v)",
				place, e.Name, provider, pc.Driver, decision.Registered())
		}
	}
	return nil
}
