package config

import (
	"fmt"
	"reflect"
)

// ClassifierConfig is the memory.classifier block: the models Document op=classify
// asks when its rules and its neighbourhood vote leave a document undecided.
//
// It mirrors memory.reranker on purpose — the same operator decision (which model,
// whose account pays for a step no agent chooses) and the same no-default posture:
// unset, classify never calls a model and reports the document unclassified with the
// reason, never an error.
//
// A decision model is asked first: one typed choice over the declared classes, a
// yes/no per concept, answered with probabilities and no text to repair. The chat
// model is the fallback, used when no decision model is configured or the decision
// answer is unsure (MinProbability, MinMargin); it must quote the document verbatim
// or its verdict is dropped.
type ClassifierConfig struct {
	Decision ClassifierModelConfig `yaml:"decision"`
	Chat     ClassifierModelConfig `yaml:"chat"`
	// MinProbability is the least probability the decision model's top class must
	// have to be taken; 0 = DefaultClassifierMinProbability.
	MinProbability float64 `yaml:"min_probability"`
	// MinMargin is the least lead the top class must have over the runner-up; 0 =
	// DefaultClassifierMinMargin.
	MinMargin float64 `yaml:"min_margin"`
}

// ClassifierModelConfig names one model of the classifier, as the reranker block does.
type ClassifierModelConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	// TimeoutMs bounds one call; 0 = 30000.
	TimeoutMs int `yaml:"timeout_ms"`
	// Effort and ContextTokens apply to the chat model only, as on a listwise reranker.
	Effort        string `yaml:"effort"`
	ContextTokens int    `yaml:"context_tokens"`
}

// Provisional thresholds (RFC FO decision 10j.7): set from the measurement on the
// first deployment that classifies a labelled corpus, and recorded with it.
const (
	DefaultClassifierMinProbability = 0.6
	DefaultClassifierMinMargin      = 0.2
)

// Configured reports whether the model is named.
func (m ClassifierModelConfig) Configured() bool { return m.Provider != "" || m.Model != "" }

// Configured reports whether either model is named.
func (c ClassifierConfig) Configured() bool { return c.Decision.Configured() || c.Chat.Configured() }

// EffectiveMinProbability and EffectiveMinMargin apply the defaults.
func (c ClassifierConfig) EffectiveMinProbability() float64 {
	if c.MinProbability == 0 {
		return DefaultClassifierMinProbability
	}
	return c.MinProbability
}

func (c ClassifierConfig) EffectiveMinMargin() float64 {
	if c.MinMargin == 0 {
		return DefaultClassifierMinMargin
	}
	return c.MinMargin
}

func (c ClassifierConfig) validate(cfg *Config) error {
	if reflect.ValueOf(c).IsZero() {
		return nil
	}
	if !c.Configured() {
		return fmt.Errorf("memory.classifier: name a decision model, a chat model, or both")
	}
	if c.MinProbability < 0 || c.MinProbability > 1 {
		return fmt.Errorf("memory.classifier.min_probability must be between 0 and 1")
	}
	if c.MinMargin < 0 || c.MinMargin > 1 {
		return fmt.Errorf("memory.classifier.min_margin must be between 0 and 1")
	}
	if err := c.Decision.validate(cfg, "memory.classifier.decision", ModelKindDecision); err != nil {
		return err
	}
	return c.Chat.validate(cfg, "memory.classifier.chat", ModelKindChat)
}

func (m ClassifierModelConfig) validate(cfg *Config, place, kind string) error {
	if reflect.ValueOf(m).IsZero() {
		return nil
	}
	if err := cfg.requireModelKind(place+".model", m.Model, kind); err != nil {
		return err
	}
	provider, model, err := cfg.ExpandServiceModel(place, m.Provider, m.Model)
	if err != nil {
		return err
	}
	if provider == "" {
		return fmt.Errorf("%s: provider is required when the block is set (or name a models: alias that carries one)", place)
	}
	if model == "" {
		return fmt.Errorf("%s: model is required when the block is set", place)
	}
	if m.TimeoutMs < 0 {
		return fmt.Errorf("%s.timeout_ms must be >= 0", place)
	}
	if m.ContextTokens < 0 {
		return fmt.Errorf("%s.context_tokens must be >= 0", place)
	}
	if kind == ModelKindDecision {
		// A decision call has no reasoning to switch off and no context window to
		// request; a copied chat block must not look configured while these are ignored.
		if m.Effort != "" {
			return fmt.Errorf("%s.effort does not apply to a decision model (remove it)", place)
		}
		if m.ContextTokens != 0 {
			return fmt.Errorf("%s.context_tokens does not apply to a decision model (remove it)", place)
		}
	}
	switch m.Effort {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("%s.effort: %q is not one of low, medium, high", place, m.Effort)
	}
	if m.BaseURL != "" {
		if err := requireHTTPBaseURL(place+".base_url", m.BaseURL); err != nil {
			return err
		}
	}
	return nil
}
