// Package inprocess is the default memory.Backend: it serves the Memory
// tool's data operations from a store.Store (sqlite-vec / Postgres) plus
// loomcycle's in-process Embedder. It is the unconditional fallback when
// no other backend (MR-3 MemoryBackendDef / MR-4 Mem9) is configured, and
// it is behaviorally identical to the pre-MR-2 direct-store path — the
// embed-on-search and embed-on-write logic moved here verbatim from the
// Memory tool's execSearch / execSet.
package inprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Backend implements memory.Backend over a store.Store + Embedder.
//
// The embedder is read at call time (not cached at construction) so a
// late-bound embedder works: main.go constructs this after both the store
// and the embedder are known, but a nil embedder at construction is fine —
// the vector ops refuse with the same typed errors as before.
type Backend struct {
	store    store.Store
	embedder providers.Embedder
}

// New builds the in-process backend. Either argument may be nil at the
// moment of construction; a nil store makes every op fail like the
// pre-MR-2 nil-Store guard would, and a nil embedder makes the vector
// ops refuse with ErrEmbedderNotConfigured exactly as before.
func New(s store.Store, e providers.Embedder) *Backend {
	return &Backend{store: s, embedder: e}
}

// Get delegates to the store.
func (b *Backend) Get(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	return b.store.MemoryGet(ctx, scope, scopeID, key)
}

// Delete delegates to the store.
func (b *Backend) Delete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	return b.store.MemoryDelete(ctx, scope, scopeID, key)
}

// List delegates to the store.
func (b *Backend) List(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	return b.store.MemoryList(ctx, scope, scopeID, prefix, limit)
}

// Stats delegates to the store.
func (b *Backend) Stats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	return b.store.MemoryEmbedStats(ctx, scope)
}

// Set writes the k/v row, then (when opts.Embed) embeds and stores the
// vector. Pre-flight refusals (no embedder / no vector support) are
// returned as typed *store.MemoryError BEFORE the k/v write so the tool
// renders the identical upfront-refusal message and no partial state
// lands. A transient embed failure AFTER the k/v write is non-fatal:
// SetResult.EmbedWarning carries it, Embedded stays false, the row stands.
//
// This is execSet's embed orchestration moved verbatim; the tool keeps
// only validation, quota, and rendering.
func (b *Backend) Set(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, opts memory.SetOptions) (memory.SetResult, error) {
	// v0.9.0 pre-flight: refuse upfront BEFORE writing the k/v row if the
	// configuration is permanently broken. Without this an agent calling
	// embed=true against a misconfigured loomcycle would silently build up
	// an unembedded corpus that never participates in search.
	if opts.Embed {
		if b.embedder == nil {
			return memory.SetResult{}, store.ErrEmbedderNotConfigured
		}
		if !b.store.SupportsVectors() {
			return memory.SetResult{}, store.ErrVectorUnsupported
		}
	}

	if err := b.store.MemorySet(ctx, scope, scopeID, key, value, opts.TTL); err != nil {
		return memory.SetResult{}, err
	}

	if !opts.Embed {
		return memory.SetResult{}, nil
	}

	// embed=true with a configured stack: try to write the embedding
	// alongside the k/v row. Transient failures here DO NOT roll back; we
	// surface a warning so the agent sees the partial-write outcome and
	// can re-embed via the admin endpoint.
	if err := b.persistEmbedding(ctx, scope, scopeID, key, value, opts.EmbedText); err != nil {
		return memory.SetResult{Embedded: false, EmbedWarning: err.Error()}, nil
	}
	return memory.SetResult{Embedded: true}, nil
}

// persistEmbedding embeds the supplied text (or the JSON-stringified
// value when embedText is empty) and writes the embedding row. Moved
// verbatim from the Memory tool; assumes the pre-flight config checks in
// Set already passed.
func (b *Backend) persistEmbedding(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, embedText string) error {
	text := embedText
	if text == "" {
		// Fall back to the JSON-stringified value. Useful for agents that
		// store small text snippets directly — they don't have to repeat
		// the text in both `value` and `embed_text`.
		text = string(value)
	}
	vecs, err := b.embedder.Embed(ctx, []string{text})
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	if len(vecs) != 1 {
		return fmt.Errorf("embed: got %d vectors, want 1", len(vecs))
	}
	emb := store.MemoryEmbedding{
		Provider:  b.embedder.Provider(),
		Model:     b.embedder.Model(),
		Dimension: len(vecs[0]),
		Vector:    vecs[0],
		EmbedText: text,
		CreatedAt: time.Now().UTC(),
	}
	return b.store.MemoryEmbedSet(ctx, scope, scopeID, key, emb)
}

// Search embeds the query, fetches the cosine pool (with the over-fetch a
// hybrid rank needs + a truncation probe), re-ranks via the MR-1 ranker,
// trims to TopK, and computes the index-aligned rank scores. This is
// execSearch's data path moved verbatim; the tool keeps validation,
// top_k clamping, and rendering.
func (b *Backend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.SearchQuery, rank memory.RankConfig, dedup memory.DedupConfig) (memory.SearchResult, error) {
	if !b.store.SupportsVectors() {
		return memory.SearchResult{}, store.ErrVectorUnsupported
	}
	if b.embedder == nil {
		return memory.SearchResult{}, store.ErrEmbedderNotConfigured
	}

	topK := q.TopK

	// RFC I hybrid ranking. Pure-semantic config = today's behavior (zero
	// regression). We over-fetch a candidate pool by cosine when EITHER a
	// hybrid rank needs to re-score deeper rows (recency promoting an entry
	// the pure-cosine top-K would miss) OR dedup is enabled (collapsing a
	// near-duplicate cluster must back-fill from rows below top_k, else the
	// agent silently gets fewer than top_k results). The pool is bounded by
	// the store's defensive cap (<=51).
	pool := topK
	if !rank.IsPureSemantic() || dedup.Enabled {
		pool = topK * 4
	}
	fetch := pool + 1 // +1 truncation probe
	if fetch > 51 {
		fetch = 51
	}

	// Embed the query text. Failures here are the embedder's problem —
	// surface them directly so operators see exactly what went wrong.
	vecs, err := b.embedder.Embed(ctx, []string{q.QueryText})
	if err != nil {
		return memory.SearchResult{}, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 {
		return memory.SearchResult{}, fmt.Errorf("embed query: got %d vectors, want 1", len(vecs))
	}
	queryVec := vecs[0]

	// Fetch the candidate pool (cosine-ordered) with a +1 truncation probe:
	// len(results) > topK distinguishes "more matched than we return" from
	// "result set fits". Store caps at 51.
	results, err := b.store.MemoryEmbedSearch(ctx, scope, scopeID, q.Prefix, queryVec, fetch)
	if err != nil {
		// Pass the error through unwrapped. ErrDimensionMismatch /
		// ErrVectorUnsupported are user-actionable: the tool's errors.Is
		// checks match the backend-constructed *MemoryError values (via
		// MemoryError.Is) and render the migration hint. Other errors are
		// surfaced by the tool with a generic "search:" prefix.
		return memory.SearchResult{}, err
	}

	// truncated reflects the cosine pool (more matched than the caller's
	// top_k), computed before the re-rank trims the pool.
	truncated := len(results) > topK

	// Hybrid re-rank, then dedup, then trim to top_k. Default config is a
	// no-op reorder (cosine order preserved) and dedup-disabled, so
	// pure-semantic output is byte-identical to before. Dedup runs on the
	// full ranked pool BEFORE the trim (RFC I Decision 2) so collapsing a
	// duplicate cluster can promote a distinct entry into the top_k that the
	// duplicate would otherwise have crowded out. rank_scores are computed
	// with the SAME `now` as the ranking so the rendered score matches the
	// ordering.
	now := time.Now()
	ranked := memory.RankCandidates(results, rank, now)
	deduped, dropped := memory.DedupResults(ranked, dedup)
	if len(deduped) > topK {
		deduped = deduped[:topK]
	}
	rankScores := memory.ScoreAll(deduped, rank, now)

	out := memory.SearchResult{
		Entries:           deduped,
		RankScores:        rankScores,
		QueryEmbeddingDim: len(queryVec),
		Truncated:         truncated,
		DedupDropped:      dropped,
	}
	if rank.SourceFrequencyReserved() {
		out.RankNote = "source_weight and frequency_weight are reserved and contribute 0 until source/access_count tracking ships"
	}
	return out, nil
}

var _ memory.Backend = (*Backend)(nil)
