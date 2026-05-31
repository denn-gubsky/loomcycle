package stubembedder

import (
	"context"
	"math"
	"testing"
)

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestStubEmbedder_Deterministic pins that the same text always yields the
// identical vector — the property a reproducible CI dedup test relies on.
func TestStubEmbedder_Deterministic(t *testing.T) {
	e := &Embedder{model: "t"}
	got, _ := e.Embed(context.Background(), []string{"the quick brown fox"})
	again, _ := e.Embed(context.Background(), []string{"the quick brown fox"})
	if len(got[0]) != stubDim {
		t.Fatalf("dim = %d, want %d", len(got[0]), stubDim)
	}
	for i := range got[0] {
		if got[0][i] != again[0][i] {
			t.Fatalf("non-deterministic at %d: %v vs %v", i, got[0][i], again[0][i])
		}
	}
}

// TestStubEmbedder_NearDuplicateHighCosine pins the dedup-enabling property:
// near-duplicate text scores at or above the 0.92 dedup threshold, while
// clearly-distinct text scores well below it.
func TestStubEmbedder_NearDuplicateHighCosine(t *testing.T) {
	e := &Embedder{model: "t"}
	vecs, _ := e.Embed(context.Background(), []string{
		"loomcycle is a high-load agentic runtime in go",         // anchor
		"loomcycle is a high-load agentic runtime written in go", // near-dup (+1 token)
		"the weather in paris is sunny and warm today",           // distinct
	})
	nearDup := cosine(vecs[0], vecs[1])
	distinct := cosine(vecs[0], vecs[2])

	if nearDup < 0.92 {
		t.Errorf("near-duplicate cosine = %.3f, want >= 0.92 (dedup would not collapse them)", nearDup)
	}
	if distinct >= 0.92 {
		t.Errorf("distinct cosine = %.3f, want < 0.92 (dedup would wrongly collapse them)", distinct)
	}
}

// TestStubEmbedder_EmptyTextIsValidUnit confirms a token-less text yields a
// valid (non-zero, unit) vector rather than an all-zero one.
func TestStubEmbedder_EmptyTextIsValidUnit(t *testing.T) {
	e := &Embedder{model: "t"}
	v, _ := e.Embed(context.Background(), []string{"   !!!   "})
	var norm float64
	for _, x := range v[0] {
		norm += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm)-1.0) > 1e-6 {
		t.Errorf("empty-text vector norm = %.6f, want 1.0", math.Sqrt(norm))
	}
}
