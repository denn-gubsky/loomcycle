package eval

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// vectorStore wraps a real SQLite store (for the k/v rows) with an
// in-memory vector index (for embeddings + cosine search). SQLite has no
// real vector search in any build, so the harness supplies one here — the
// same approach the in-process backend's own tests use. This keeps the
// evaluator running against the REAL in-process backend + ranker + dedup
// code path, only substituting the vector storage the default build lacks.
type vectorStore struct {
	store.Store
	mu     sync.Mutex
	embeds map[string]store.MemoryEmbedding
}

// newVectorStore opens a fresh in-memory SQLite store and wraps it. The
// returned closer disposes the underlying store.
func newVectorStore() (*vectorStore, func(), error) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		return nil, nil, err
	}
	return &vectorStore{Store: s, embeds: map[string]store.MemoryEmbedding{}},
		func() { _ = s.Close() }, nil
}

func vsKey(scope store.MemoryScope, id, key string) string {
	return string(scope) + "|" + id + "|" + key
}

func (v *vectorStore) SupportsVectors() bool { return true }

func (v *vectorStore) MemoryEmbedSet(_ context.Context, scope store.MemoryScope, id, key string, e store.MemoryEmbedding) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.embeds[vsKey(scope, id, key)] = e
	return nil
}

// MemoryEmbedSearch runs an exact cosine Top-K over the in-memory index,
// returning each row's stored vector on the entry (so the in-process
// backend's MR-5 dedup pass has vectors to compare — same contract as the
// real sqlite/pgvector stores after the MR-5 change).
func (v *vectorStore) MemoryEmbedSearch(ctx context.Context, scope store.MemoryScope, id, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if topK > 51 {
		topK = 51
	}
	prefix := string(scope) + "|" + id + "|"
	type scored struct {
		key string
		s   float64
		emb store.MemoryEmbedding
	}
	var rows []scored
	for k, e := range v.embeds {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		key := strings.TrimPrefix(k, prefix)
		if keyPrefix != "" && !strings.HasPrefix(key, keyPrefix) {
			continue
		}
		rows = append(rows, scored{key: key, s: cosine(query, e.Vector), emb: e})
	}
	// Sort by score descending, breaking ties by key. The tie-break is
	// load-bearing for the eval harness's reproducibility claim: rows are
	// gathered by ranging a map (randomized order), and the bundled dataset
	// deliberately includes identical-vector rows (pref_color_1/2/3) with
	// equal scores. Without a deterministic secondary key, those tie rows
	// — and thus which one a dedup pass keeps — would vary run-to-run,
	// making recall@k / duplication_rate flap and defeating the gating-tool
	// purpose. (Real pgvector/sqlite-vec search has its own stable order;
	// this in-memory eval store must supply one explicitly.)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].s != rows[j].s {
			return rows[i].s > rows[j].s
		}
		return rows[i].key < rows[j].key
	})
	if len(rows) > topK {
		rows = rows[:topK]
	}
	out := make([]store.MemorySearchEntry, 0, len(rows))
	for _, r := range rows {
		entry, err := v.Store.MemoryGet(ctx, scope, id, r.key)
		if err != nil {
			continue
		}
		se := store.MemorySearchEntry{MemoryEntry: entry, Score: r.s}
		se.EmbeddedWith.Provider = r.emb.Provider
		se.EmbeddedWith.Model = r.emb.Model
		se.Vector = r.emb.Vector
		out = append(out, se)
	}
	return out, nil
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
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
