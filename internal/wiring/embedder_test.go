package wiring

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
)

// TestEmbedder_ResolvesAModelsAlias — the regression: `memory.embedder.model:
// local-embedding` reached Ollama as a literal model name ("model
// local-embedding not found"). The embedder must be asked for the concrete model
// the alias names, on the alias's provider when the block names none.
func TestEmbedder_ResolvesAModelsAlias(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelRef{"local-embedding": {Provider: "ollama", Model: "bge-m3:latest"}},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Model: "local-embedding", BaseURL: "http://127.0.0.1:1"}},
	}
	e, err := Embedder(cfg)
	if err != nil || e == nil {
		t.Fatalf("Embedder = %v, %v", e, err)
	}
	if e.Model() != "bge-m3:latest" || e.Provider() != "ollama" {
		t.Errorf("embedder = %s/%s, want ollama/bge-m3:latest", e.Provider(), e.Model())
	}
}

// The embedder's call timeout and batch size come from memory.embedder: unset
// they are 30s and 100 texts, and nothing else supplies them.
func TestEmbedderLimits_ComeFromTheYAML(t *testing.T) {
	for _, tc := range []struct {
		name               string
		embedder           config.EmbedderConfig
		timeoutMs, batchSz int
	}{
		{"default", config.EmbedderConfig{}, 30000, 100},
		{"set", config.EmbedderConfig{TimeoutMs: 5000, BatchSize: 8}, 5000, 8},
		{"one set", config.EmbedderConfig{BatchSize: 1}, 30000, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timeoutMs, batchSize := embedderLimits(&config.Config{Memory: config.MemoryConfig{Embedder: tc.embedder}})
			if timeoutMs != tc.timeoutMs || batchSize != tc.batchSz {
				t.Errorf("limits = %d ms, %d texts; want %d, %d", timeoutMs, batchSize, tc.timeoutMs, tc.batchSz)
			}
		})
	}
}
