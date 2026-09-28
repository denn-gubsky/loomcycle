package config

import (
	"fmt"
	"strings"
)

// UnitGeneratorConfig is the memory.unit_generator block: the model that writes
// Document derived search units (RFC DM Design C) — a description, claims and
// questions per chunk — when the operator runs POST /v1/_document/derive_units.
//
// Shaped like memory.embedder and memory.reranker, because it is the same kind of
// decision: which model, and whose account, pays for a model call per chunk. No
// default model: without this block nothing is ever generated. Nothing is
// generated on write either — generation is always an explicit operator pass.
type UnitGeneratorConfig struct {
	// Provider / Model name the model (a models: alias works, and supplies the
	// provider when this names none). BaseURL / APIKeyEnv override the provider's.
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	// TimeoutMs bounds one generation call. 0 = 120000.
	TimeoutMs int `yaml:"timeout_ms"`
	// Effort is the reasoning hint ("low" turns thinking off on a hybrid local
	// model, which is how the units were measured).
	Effort string `yaml:"effort"`
	// ContextTokens is the context window requested per call (Ollama num_ctx).
	// 0 = 16384.
	ContextTokens int `yaml:"context_tokens"`
	// MaxOutputTokens caps one call's answer. 0 = 2000, the measured value: a
	// chunk's claims and questions run to several hundred tokens, and a JSON
	// answer cut short does not parse.
	MaxOutputTokens int `yaml:"max_output_tokens"`

	// Subtrees are the Path subtrees an operator marks for generation: every
	// document named under one gets units unless the document's own `index_units`
	// setting says otherwise. The memory trees (/facts, /memory) can never be
	// marked — see UnitsExcludedPath.
	Subtrees []UnitSubtree `yaml:"subtrees"`
}

// UnitSubtree marks one Path subtree for generation.
type UnitSubtree struct {
	// Path is the subtree root, e.g. /docs/policies (a document at the path itself
	// or anywhere under it).
	Path string `yaml:"path"`
	// Kinds are the unit kinds to generate: description, claims, questions.
	// Empty means all three (decision 8).
	Kinds []string `yaml:"kinds"`
}

// UnitKinds are the opt-in vocabulary, in the order they are generated.
var UnitKinds = []string{"description", "claims", "questions"}

// Configured reports whether a unit generator is declared at all.
func (g UnitGeneratorConfig) Configured() bool { return g.Provider != "" || g.Model != "" }

// UnitsExcludedPath reports whether a Path-tree path lies in the memory trees,
// which never get units whatever is set on them: /facts holds subject-homed facts
// and /memory the ontology and the user and tenant roots — memory's own derived
// data, and generating units from them would feed memory's output back into its
// input (decision 6).
func UnitsExcludedPath(path string) bool {
	for _, root := range []string{"/facts", "/memory"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// SubtreeKinds returns the kinds the operator marked for a document at path, and
// whether any subtree covers it. The deepest covering subtree wins, so a narrower
// marking can refine a broader one.
func (g UnitGeneratorConfig) SubtreeKinds(path string) ([]string, bool) {
	best := -1
	var kinds []string
	for _, st := range g.Subtrees {
		root := strings.TrimRight(st.Path, "/")
		if path == root || strings.HasPrefix(path, root+"/") {
			if len(root) > best {
				best, kinds = len(root), st.Kinds
			}
		}
	}
	if best < 0 {
		return nil, false
	}
	if len(kinds) == 0 {
		return append([]string(nil), UnitKinds...), true
	}
	return kinds, true
}

// ValidUnitKinds reports the first entry of kinds that is not a unit kind.
func ValidUnitKinds(kinds []string) error {
	for _, k := range kinds {
		switch k {
		case "description", "claims", "questions":
		default:
			return fmt.Errorf("%q is not a unit kind (description, claims, questions)", k)
		}
	}
	return nil
}

// validate checks the block structurally; whether its provider exists is decided
// when it is built.
func (g UnitGeneratorConfig) validate(c *Config) error {
	if !g.Configured() && len(g.Subtrees) == 0 && g.BaseURL == "" && g.APIKeyEnv == "" {
		return nil
	}
	if !g.Configured() {
		return fmt.Errorf("memory.unit_generator: model is required when the block is set")
	}
	provider, model, err := c.ExpandServiceModel("memory.unit_generator", g.Provider, g.Model)
	if err != nil {
		return err
	}
	if provider == "" {
		return fmt.Errorf("memory.unit_generator: provider is required (or name a models: alias that carries one)")
	}
	if model == "" {
		return fmt.Errorf("memory.unit_generator: model is required when the block is set")
	}
	for _, v := range []struct {
		name string
		n    int
	}{{"timeout_ms", g.TimeoutMs}, {"context_tokens", g.ContextTokens}, {"max_output_tokens", g.MaxOutputTokens}} {
		if v.n < 0 {
			return fmt.Errorf("memory.unit_generator.%s must be >= 0", v.name)
		}
	}
	switch g.Effort {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("memory.unit_generator.effort: %q is not one of low, medium, high", g.Effort)
	}
	if g.BaseURL != "" {
		if err := requireHTTPBaseURL("memory.unit_generator.base_url", g.BaseURL); err != nil {
			return err
		}
	}
	for i, st := range g.Subtrees {
		if !strings.HasPrefix(st.Path, "/") {
			return fmt.Errorf("memory.unit_generator.subtrees[%d].path %q must be an absolute Path-tree path", i, st.Path)
		}
		if UnitsExcludedPath(strings.TrimRight(st.Path, "/")) || strings.TrimRight(st.Path, "/") == "" {
			return fmt.Errorf("memory.unit_generator.subtrees[%d].path %q covers the memory trees (/facts, /memory), which never get units", i, st.Path)
		}
		if err := ValidUnitKinds(st.Kinds); err != nil {
			return fmt.Errorf("memory.unit_generator.subtrees[%d].kinds: %w", i, err)
		}
	}
	return nil
}
