package eval

import (
	"context"
	"strings"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestRun_BundledDatasetComputesMetricsInRange is the harness end-to-end
// test: load the embedded dataset, run it against the real in-process
// backend with the deterministic embedder, and assert every metric is
// computed and in [0,1] (latencies non-negative). This is the CI gate
// proving the plumbing + metric math hold without a real provider key.
func TestRun_BundledDatasetComputesMetricsInRange(t *testing.T) {
	ds, err := BundledDataset()
	if err != nil {
		t.Fatalf("BundledDataset: %v", err)
	}
	if len(ds.Corpus) < 15 {
		t.Fatalf("bundled corpus has %d items, want >= 15", len(ds.Corpus))
	}
	if len(ds.Queries) == 0 {
		t.Fatal("bundled dataset has no queries")
	}

	emb := NewDeterministicEmbedder(64)
	rep, err := Run(context.Background(), ds, emb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Queries != len(ds.Queries) {
		t.Errorf("report queries = %d, want %d", rep.Queries, len(ds.Queries))
	}
	if rep.CorpusSize != len(ds.Corpus) {
		t.Errorf("report corpus_size = %d, want %d", rep.CorpusSize, len(ds.Corpus))
	}
	inUnit := func(name string, v float64) {
		if v < 0 || v > 1 {
			t.Errorf("%s = %v, want in [0,1]", name, v)
		}
	}
	inUnit("precision_at_k", rep.PrecisionAtK)
	inUnit("recall_at_k", rep.RecallAtK)
	inUnit("duplication_rate", rep.DuplicationRate)
	if rep.RecallLatencyP50Ms < 0 || rep.RecallLatencyP99Ms < 0 {
		t.Errorf("latencies must be non-negative: p50=%v p99=%v", rep.RecallLatencyP50Ms, rep.RecallLatencyP99Ms)
	}
	if !strings.Contains(rep.Embedder, "deterministic-eval-stub") {
		t.Errorf("embedder label = %q, want the deterministic stub", rep.Embedder)
	}

	// The bundled dataset enables dedup and includes a 3-row near-duplicate
	// cluster (pref_color_1/2/3, identical embed_text). At least one query
	// must therefore drop a duplicate, making duplication_rate > 0 — proving
	// the dedup path actually ran inside the harness.
	if rep.DuplicationRate <= 0 {
		t.Errorf("duplication_rate = %v, want > 0 (the dedup cluster should collapse)", rep.DuplicationRate)
	}

	// The deterministic embedder shares tokens between each query and its
	// expected key, so recall should be meaningfully above zero — a sanity
	// check that retrieval works at all (not a quality bar).
	if rep.RecallAtK <= 0 {
		t.Errorf("recall_at_k = %v, want > 0 with token-overlapping queries", rep.RecallAtK)
	}
}

// TestDeterministicEmbedder_Reproducible pins that the stub embedder is a
// pure function of its input: the same text always yields the same vector.
// The harness's reproducibility depends on this.
func TestDeterministicEmbedder_Reproducible(t *testing.T) {
	e := NewDeterministicEmbedder(32)
	a, err := e.Embed(context.Background(), []string{"the capital of France is Paris"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(context.Background(), []string{"the capital of France is Paris"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a[0]) != 32 || len(b[0]) != 32 {
		t.Fatalf("dim mismatch: %d / %d, want 32", len(a[0]), len(b[0]))
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("embedder not reproducible at coord %d: %v vs %v", i, a[0][i], b[0][i])
		}
	}
}

// TestRun_DedupOffYieldsZeroDuplicationRate pins that with dedup disabled
// the duplication_rate is 0 (nothing is dropped), proving the metric tracks
// the dedup pass and not some incidental count.
func TestRun_DedupOffYieldsZeroDuplicationRate(t *testing.T) {
	ds, err := BundledDataset()
	if err != nil {
		t.Fatalf("BundledDataset: %v", err)
	}
	ds.Dedup = &memory.DedupConfig{Enabled: false}

	rep, err := Run(context.Background(), ds, NewDeterministicEmbedder(64))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DuplicationRate != 0 {
		t.Errorf("dedup off: duplication_rate = %v, want 0", rep.DuplicationRate)
	}
}

// TestLoadJSONL_RejectsEmptyCorpus pins the schema validation: a header
// with an empty corpus is rejected with a clear error.
func TestLoadJSONL_RejectsEmptyCorpus(t *testing.T) {
	_, err := LoadJSONL(strings.NewReader(`{"name":"x","corpus":[]}` + "\n" + `{"query":"q","expected":["k"]}` + "\n"))
	if err == nil {
		t.Fatal("expected an error for empty corpus")
	}
	if !strings.Contains(err.Error(), "corpus is empty") {
		t.Errorf("error = %v, want 'corpus is empty'", err)
	}
}
