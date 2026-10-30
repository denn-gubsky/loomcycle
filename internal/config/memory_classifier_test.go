package config

import (
	"strings"
	"testing"
)

// TestClassifierConfig_LoadsBothModelsAndTheThresholds — the memory.classifier block
// round-trips off the yaml, and the thresholds default when unset.
func TestClassifierConfig_LoadsBothModelsAndTheThresholds(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  classifier:
    decision: { provider: ollama-local, model: nimble, timeout_ms: 5000 }
    chat: { provider: ollama-local, model: qwen3.6:latest, effort: low, context_tokens: 16384 }
    min_probability: 0.7
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cc := cfg.Memory.Classifier
	if !cc.Configured() || cc.Decision.Model != "nimble" || cc.Decision.TimeoutMs != 5000 || cc.Chat.Effort != "low" || cc.Chat.ContextTokens != 16384 {
		t.Errorf("classifier = %+v", cc)
	}
	if cc.EffectiveMinProbability() != 0.7 || cc.EffectiveMinMargin() != DefaultClassifierMinMargin {
		t.Errorf("thresholds = %v / %v", cc.EffectiveMinProbability(), cc.EffectiveMinMargin())
	}
}

// TestClassifierConfig_AbsentMeansNoModel — no block, no model step: there is no
// default model to fall back to.
func TestClassifierConfig_AbsentMeansNoModel(t *testing.T) {
	cfg, err := Load(writeCfg(t, "defaults: { provider: anthropic, model: claude-sonnet-4-6 }\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Memory.Classifier.Configured() {
		t.Errorf("no block, yet Configured: %+v", cfg.Memory.Classifier)
	}
}

// TestClassifierConfig_RejectsAnIncompleteOrInvalidBlock — a half-written block fails
// LOAD, naming the field, rather than as a model step that silently never runs.
func TestClassifierConfig_RejectsAnIncompleteOrInvalidBlock(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"min_probability: 0.5", "memory.classifier: name a decision model, a chat model, or both"},
		{"decision: { model: nimble }", "memory.classifier.decision: provider is required"},
		{"chat: { provider: ollama-local }", "memory.classifier.chat: model is required"},
		{"chat: { provider: p, model: m, timeout_ms: -1 }", "memory.classifier.chat.timeout_ms"},
		{"chat: { provider: p, model: m, effort: max }", "memory.classifier.chat.effort"},
		{"chat: { provider: p, model: m, base_url: gpu:11434 }", "memory.classifier.chat.base_url"},
		{"decision: { provider: p, model: m, effort: low }", "memory.classifier.decision.effort does not apply to a decision model"},
		{"decision: { provider: p, model: m, context_tokens: 8192 }", "memory.classifier.decision.context_tokens does not apply"},
		{"chat: { provider: p, model: m }\n    min_probability: 1.5", "memory.classifier.min_probability"},
		{"chat: { provider: p, model: m }\n    min_margin: -0.1", "memory.classifier.min_margin"},
	} {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  classifier:
    `+c.body+`
`))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error = %v, want it to contain %q", c.body, err, c.want)
		}
	}
}
