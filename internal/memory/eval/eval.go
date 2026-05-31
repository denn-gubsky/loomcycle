// Package eval is the RFC I (MR-5 / Decision 5) memory retrieval-quality
// harness. It seeds a corpus into the REAL in-process memory backend
// (ranker + search-time dedup included), runs a set of {query, expected}
// tuples, and reports precision@k, recall@k, duplication_rate, and recall
// latency percentiles.
//
// The harness is the gating tool for ranker / dedup changes: run it before
// and after a change and compare the metrics. With the bundled
// deterministic embedder it runs in CI with no provider key (reproducible
// but NOT semantic — see DeterministicEmbedder); a real quality number
// comes from an operator run against a real embedder via --dataset.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Dataset is the harness input. SHAPE: one shared corpus, then many
// queries against it. Each query names the keys it expects to recall.
// This is the simpler of the two candidate shapes (vs per-query corpora)
// and is what the JSONL files use: the FIRST line is the corpus object,
// every SUBSEQUENT line is a query object. See LoadJSONL.
type Dataset struct {
	// Name labels the dataset in the report.
	Name string `json:"name"`
	// Corpus is the set of memory rows seeded before any query runs.
	Corpus []CorpusItem `json:"corpus"`
	// Queries are evaluated in order against the seeded corpus.
	Queries []Query `json:"queries"`
	// TopK is the retrieval depth metrics are computed at. <= 0 → 10.
	TopK int `json:"top_k"`
	// Rank / Dedup are the optional configs applied to every query. A nil
	// Rank means pure-semantic (default); a nil Dedup means dedup disabled.
	Rank  *memory.RankConfig  `json:"rank,omitempty"`
	Dedup *memory.DedupConfig `json:"dedup,omitempty"`
}

// CorpusItem is one seeded memory row. EmbedText defaults to Value's
// string when empty (matching the Memory tool's set contract).
type CorpusItem struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	EmbedText string          `json:"embed_text,omitempty"`
}

// Query is one evaluation tuple: a query text + the keys a correct
// retrieval should surface.
type Query struct {
	Query    string   `json:"query"`
	Expected []string `json:"expected"`
}

// Report is the harness output: corpus-wide metrics aggregated over all
// queries. All ratios are in [0,1].
type Report struct {
	Dataset            string  `json:"dataset"`
	Queries            int     `json:"queries"`
	CorpusSize         int     `json:"corpus_size"`
	TopK               int     `json:"top_k"`
	Embedder           string  `json:"embedder"`
	PrecisionAtK       float64 `json:"precision_at_k"`
	RecallAtK          float64 `json:"recall_at_k"`
	DuplicationRate    float64 `json:"duplication_rate"`
	RecallLatencyP50Ms float64 `json:"recall_latency_p50_ms"`
	RecallLatencyP99Ms float64 `json:"recall_latency_p99_ms"`
}

// Run seeds the dataset's corpus into a fresh in-process backend (over an
// in-memory vector store) using the supplied embedder, then evaluates each
// query and returns the aggregated Report. The embedder is the SAME one
// used for both seeding and querying so the vectors are comparable.
func Run(ctx context.Context, ds Dataset, emb providers.Embedder) (Report, error) {
	topK := ds.TopK
	if topK <= 0 {
		topK = 10
	}

	vs, closeStore, err := newVectorStore()
	if err != nil {
		return Report{}, fmt.Errorf("eval: open store: %w", err)
	}
	defer closeStore()

	backend := inprocess.New(vs, emb)
	const scope = store.MemoryScopeAgent
	const scopeID = "eval"

	// Seed the corpus.
	for _, it := range ds.Corpus {
		if _, err := backend.Set(ctx, scope, scopeID, it.Key, it.Value, memory.SetOptions{
			Embed:     true,
			EmbedText: it.EmbedText,
		}); err != nil {
			return Report{}, fmt.Errorf("eval: seed key %q: %w", it.Key, err)
		}
	}

	rank := memory.DefaultRankConfig()
	if ds.Rank != nil {
		rank = *ds.Rank
	}
	var dedup memory.DedupConfig
	if ds.Dedup != nil {
		dedup = *ds.Dedup
	}

	var (
		sumPrecision float64
		sumRecall    float64
		totalDropped int
		totalBefore  int
		latencies    []time.Duration
	)
	for _, q := range ds.Queries {
		start := time.Now()
		res, err := backend.Search(ctx, scope, scopeID, memory.SearchQuery{
			QueryText: q.Query,
			TopK:      topK,
		}, rank, dedup)
		latencies = append(latencies, time.Since(start))
		if err != nil {
			return Report{}, fmt.Errorf("eval: query %q: %w", q.Query, err)
		}

		retrieved := make([]string, 0, len(res.Entries))
		for _, e := range res.Entries {
			retrieved = append(retrieved, e.Key)
		}
		sumPrecision += precisionAtK(retrieved, q.Expected, topK)
		sumRecall += recallAtK(retrieved, q.Expected)

		// duplication_rate is dropped / (retrieved_after + dropped) — the
		// fraction of the would-be result set that was near-duplicate. When
		// dedup is disabled DedupDropped is 0, so the rate is 0.
		totalDropped += res.DedupDropped
		totalBefore += len(res.Entries) + res.DedupDropped
	}

	n := float64(len(ds.Queries))
	rep := Report{
		Dataset:    ds.Name,
		Queries:    len(ds.Queries),
		CorpusSize: len(ds.Corpus),
		TopK:       topK,
		Embedder:   emb.Provider() + "/" + emb.Model(),
	}
	if n > 0 {
		rep.PrecisionAtK = sumPrecision / n
		rep.RecallAtK = sumRecall / n
	}
	if totalBefore > 0 {
		rep.DuplicationRate = float64(totalDropped) / float64(totalBefore)
	}
	rep.RecallLatencyP50Ms = percentileMs(latencies, 0.50)
	rep.RecallLatencyP99Ms = percentileMs(latencies, 0.99)
	return rep, nil
}

// precisionAtK = |retrieved ∩ expected| / k. Uses k (the retrieval depth)
// as the denominator per the RFC definition, NOT len(retrieved) — a query
// that returns fewer than k rows is "penalised" for the empty slots, which
// is the standard precision@k convention.
func precisionAtK(retrieved, expected []string, k int) float64 {
	if k <= 0 {
		return 0
	}
	hits := intersectionCount(retrieved, expected)
	return float64(hits) / float64(k)
}

// recallAtK = |retrieved ∩ expected| / |expected|.
func recallAtK(retrieved, expected []string) float64 {
	if len(expected) == 0 {
		return 0
	}
	hits := intersectionCount(retrieved, expected)
	return float64(hits) / float64(len(expected))
}

func intersectionCount(retrieved, expected []string) int {
	want := make(map[string]bool, len(expected))
	for _, e := range expected {
		want[e] = true
	}
	hits := 0
	seen := make(map[string]bool, len(retrieved))
	for _, r := range retrieved {
		if want[r] && !seen[r] {
			seen[r] = true
			hits++
		}
	}
	return hits
}

// percentileMs returns the p-th percentile (0..1) of the durations, in
// milliseconds, using nearest-rank. Empty input → 0.
func percentileMs(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000.0
}
