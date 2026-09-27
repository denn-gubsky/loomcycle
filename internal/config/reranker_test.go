package config

import (
	"strings"
	"testing"
)

// TestRerankerConfig_LoadsFromYaml — every field of the memory.reranker block
// round-trips off the yaml.
func TestRerankerConfig_LoadsFromYaml(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  reranker:
    provider: ollama-local
    model: qwen3.6:latest
    base_url: http://gpu.internal:11434
    api_key_env: MY_RERANK_TOKEN
    timeout_ms: 20000
    effort: low
    context_tokens: 32768
    max_concurrent: 2
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RerankerConfig{
		Provider: "ollama-local", Model: "qwen3.6:latest",
		BaseURL: "http://gpu.internal:11434", APIKeyEnv: "MY_RERANK_TOKEN",
		TimeoutMs: 20000, Effort: "low", ContextTokens: 32768, MaxConcurrent: 2,
	}
	if cfg.Memory.Reranker != want {
		t.Errorf("reranker = %+v, want %+v", cfg.Memory.Reranker, want)
	}
	if !cfg.Memory.Reranker.Configured() {
		t.Error("a declared reranker must report Configured")
	}
}

// TestRerankerConfig_AbsentMeansNotConfigured — no block, no reranker: there is
// no default model to fall back to.
func TestRerankerConfig_AbsentMeansNotConfigured(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Memory.Reranker.Configured() {
		t.Errorf("no memory.reranker block, yet Configured: %+v", cfg.Memory.Reranker)
	}
}

// TestRerankerConfig_RejectsAnIncompleteOrInvalidBlock — a half-written block
// fails LOAD, naming the field, rather than surfacing as a rerank that silently
// never happens.
func TestRerankerConfig_RejectsAnIncompleteOrInvalidBlock(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"model: m", "memory.reranker: provider is required"},
		{"provider: ollama-local", "memory.reranker: model is required"},
		{"provider: p\n    model: m\n    timeout_ms: -1", "memory.reranker.timeout_ms"},
		{"provider: p\n    model: m\n    context_tokens: -5", "memory.reranker.context_tokens"},
		{"provider: p\n    model: m\n    effort: max", "memory.reranker.effort"},
		{"provider: p\n    model: m\n    max_concurrent: -1", "memory.reranker.max_concurrent"},
		{"provider: p\n    model: m\n    base_url: gpu.internal:11434", "memory.reranker.base_url"},
	} {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
memory:
  reranker:
    `+c.body+`
`))
		if err == nil {
			t.Errorf("%q: expected a load error", c.body)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error should contain %q, got: %v", c.body, c.want, err)
		}
	}
}
