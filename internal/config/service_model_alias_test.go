package config

import (
	"strings"
	"testing"
)

// TestExpandServiceModel_ResolvesAModelsAlias — a service block naming a models:
// alias gets the concrete model, and the alias's provider when it names none. An
// explicit provider still wins, exactly as for an agent.
func TestExpandServiceModel_ResolvesAModelsAlias(t *testing.T) {
	c := &Config{Models: map[string]ModelRef{
		"local-embedding": {Provider: "ollama-local", Model: "bge-m3:latest"},
		"newest-qwen":     {Provider: "ollama-local", ModelPattern: "qwen3*"},
	}}
	for _, tc := range []struct{ provider, model, wantP, wantM string }{
		{"", "local-embedding", "ollama-local", "bge-m3:latest"},
		{"ollama", "local-embedding", "ollama", "bge-m3:latest"}, // explicit provider wins
		{"ollama-local", "nomic-embed-text", "ollama-local", "nomic-embed-text"},
	} {
		p, m, err := c.ExpandServiceModel("memory.embedder", tc.provider, tc.model)
		if err != nil || p != tc.wantP || m != tc.wantM {
			t.Errorf("(%q,%q) = (%q,%q,%v), want (%q,%q)", tc.provider, tc.model, p, m, err, tc.wantP, tc.wantM)
		}
	}
	if _, _, err := c.ExpandServiceModel("memory.embedder", "", "newest-qwen"); err == nil ||
		!strings.Contains(err.Error(), "memory.embedder.model") || !strings.Contains(err.Error(), "model_pattern") {
		t.Errorf("a pattern alias must be refused, naming the field: %v", err)
	}
}

// TestServiceBlocks_LoadWithAnAliasAndNoProvider — the operator's own shape: every
// model name kept in one models: map, and memory.embedder / memory.reranker
// naming the aliases alone. It must LOAD (the provider comes from the alias).
func TestServiceBlocks_LoadWithAnAliasAndNoProvider(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  local-medium:    { provider: ollama, model: qwen3.6:latest }
  local-embedding: { provider: ollama, model: bge-m3:latest }
memory:
  embedder: { model: local-embedding }
  reranker: { model: local-medium }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Memory.Reranker.Configured() {
		t.Error("a reranker block naming only an alias is declared")
	}
	_, err = Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  newest: { provider: ollama, model_pattern: "qwen3*" }
memory:
  reranker: { model: newest }
`))
	if err == nil || !strings.Contains(err.Error(), "memory.reranker.model") {
		t.Errorf("a pattern alias on the reranker must fail load, got %v", err)
	}
}
