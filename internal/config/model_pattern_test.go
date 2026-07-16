package config

import (
	"strings"
	"testing"
)

// RFC BG model_pattern alias validation. A models: entry must set EXACTLY one of
// model / model_pattern; a pattern alias needs an explicit provider (a pattern
// is scoped to one catalog) and a legal path.Match glob.

func modelPatternBase(models map[string]ModelRef) *Config {
	return &Config{
		Defaults:    Defaults{Provider: "anthropic", Model: "x"},
		Concurrency: Concurrency{MaxConcurrentRuns: 1},
		Models:      models,
	}
}

func TestValidate_ModelPattern_ValidAliasLoads(t *testing.T) {
	if err := validate(modelPatternBase(map[string]ModelRef{
		"haiku": {Provider: "anthropic", ModelPattern: "claude-haiku-*"},
	})); err != nil {
		t.Errorf("valid model_pattern alias rejected: %v", err)
	}
}

func TestValidate_ModelPattern_RejectsBothModelAndPattern(t *testing.T) {
	err := validate(modelPatternBase(map[string]ModelRef{
		"haiku": {Provider: "anthropic", Model: "claude-haiku-4-5", ModelPattern: "claude-haiku-*"},
	}))
	if err == nil || !strings.Contains(err.Error(), "exactly one of model or model_pattern") {
		t.Errorf("both model and model_pattern accepted: %v", err)
	}
}

func TestValidate_ModelPattern_RejectsNeitherModelNorPattern(t *testing.T) {
	err := validate(modelPatternBase(map[string]ModelRef{
		"empty": {Provider: "anthropic"},
	}))
	if err == nil || !strings.Contains(err.Error(), "exactly one of model or model_pattern") {
		t.Errorf("empty alias (no model, no pattern) accepted: %v", err)
	}
}

func TestValidate_ModelPattern_RequiresProvider(t *testing.T) {
	err := validate(modelPatternBase(map[string]ModelRef{
		"haiku": {ModelPattern: "claude-haiku-*"},
	}))
	if err == nil || !strings.Contains(err.Error(), "model_pattern requires an explicit provider") {
		t.Errorf("provider-less pattern accepted: %v", err)
	}
}

func TestValidate_ModelPattern_RejectsBadGlob(t *testing.T) {
	// An unterminated character class is path.ErrBadPattern.
	err := validate(modelPatternBase(map[string]ModelRef{
		"haiku": {Provider: "anthropic", ModelPattern: "claude-[haiku"},
	}))
	if err == nil || !strings.Contains(err.Error(), "invalid model_pattern") {
		t.Errorf("malformed glob accepted: %v", err)
	}
}

// TestValidate_ModelPattern_ConcreteOnlyUnchanged pins the byte-identical
// guarantee: a config with only concrete aliases validates exactly as before —
// the RFC BG exactly-one check must not reject a plain model alias.
func TestValidate_ModelPattern_ConcreteOnlyUnchanged(t *testing.T) {
	if err := validate(modelPatternBase(map[string]ModelRef{
		"smart": {Provider: "anthropic", Model: "claude-sonnet-4-6"},
		"fast":  {Model: "claude-haiku-4-5"}, // empty provider deferred to the referencing candidate
	})); err != nil {
		t.Errorf("concrete-only alias config rejected: %v", err)
	}
}
