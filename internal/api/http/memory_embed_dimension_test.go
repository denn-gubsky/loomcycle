package http

import (
	"context"
	"encoding/json"
	"testing"
)

// zeroDimEmbedder returns real, fixed-width vectors while advertising
// Dimension() == 0 — exactly what the openai driver does for any model outside
// its static (model → dim) table, which is every model a self-hosted
// OpenAI-compatible server (vLLM, TEI, Infinity, Ollama's compat layer) serves.
type zeroDimEmbedder struct{ width int }

func (e *zeroDimEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, e.width)
	}
	return out, nil
}
func (e *zeroDimEmbedder) Provider() string { return "openai" }
func (e *zeroDimEmbedder) Model() string    { return "bge-m3" }

// The bug under test: a static table has no entry for this model, so the
// driver can only answer 0 — forever, not just before the first call.
func (e *zeroDimEmbedder) Dimension() int { return 0 }

// TestMemoryEmbed_StoresObservedWidthWhenEmbedderReportsZero — a stored
// embedding row must carry the width of the vector actually returned, never the
// embedder's advertised dimension.
//
// Why it matters: MemoryEmbedSearch probes ONE arbitrary row (LIMIT 1) and
// compares its `dimension` column to the query vector's width. A row written
// with 0 alongside a real vector makes every subsequent search in that scope
// fail `dimension_mismatch` and advise a reembed that was never needed — and
// with a mix of good and zero rows the failure is non-deterministic.
func TestMemoryEmbed_StoresObservedWidthWhenEmbedderReportsZero(t *testing.T) {
	srv, _, vs := vectorAdminFixture(t, true)
	const width = 4
	srv.SetEmbedder(&zeroDimEmbedder{width: width})

	if err := srv.embedMemoryEntry(context.Background(),
		"user", "alice", "k", json.RawMessage(`"hello"`)); err != nil {
		t.Fatalf("embedMemoryEntry: %v", err)
	}

	got, ok := vs.embeds["user|alice|k"]
	if !ok {
		t.Fatal("no embedding row was written")
	}
	if len(got.Vector) != width {
		t.Fatalf("fixture broken: stored vector is %d wide, want %d", len(got.Vector), width)
	}
	if got.Dimension != width {
		t.Errorf("stored dimension = %d, want %d (the observed vector width); "+
			"a stored 0 makes every later search in this scope fail dimension_mismatch",
			got.Dimension, width)
	}
}
