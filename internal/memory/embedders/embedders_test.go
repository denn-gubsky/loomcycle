package embedders

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
)

// TestBuild_ResolvesAModelsAlias — the regression: `memory.embedder.model:
// local-embedding` reached Ollama as a literal model name ("model
// local-embedding not found"). The embedder must be asked for the concrete model
// the alias names, on the alias's provider when the block names none.
func TestBuild_ResolvesAModelsAlias(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelRef{"local-embedding": {Provider: "ollama", Model: "bge-m3:latest"}},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Model: "local-embedding", BaseURL: "http://127.0.0.1:1"}},
	}
	e, err := Build(cfg)
	if err != nil || e == nil {
		t.Fatalf("Build = %v, %v", e, err)
	}
	if e.Model() != "bge-m3:latest" || e.Provider() != "ollama" {
		t.Errorf("embedder = %s/%s, want ollama/bge-m3:latest", e.Provider(), e.Model())
	}
}
