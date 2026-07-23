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
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runTenant is the RFC BL isolation axis for base memory: the run's authoritative
// tenant, sourced from the ctx-carried RunIdentity (server-supplied, never model
// input). The memory.Backend interface deliberately takes no tenantID param —
// the base k/v ops key on (scope, scopeID) — so the impl pulls it from ctx here,
// mirroring how the mem9 backend derives its tenant from the run identity. An
// empty tenant ("" — open mode / legacy) is the shared/legacy partition.
func runTenant(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID }

// Backend implements memory.Backend over a store.Store + Embedder.
//
// The embedder is read at call time (not cached at construction) so a
// late-bound embedder works: main.go constructs this after both the store
// and the embedder are known, but a nil embedder at construction is fine —
// the vector ops refuse with the same typed errors as before.
type Backend struct {
	store    store.Store
	embedder providers.Embedder
	// accessFlusher, when set, records a +1 access for each entry a search
	// returns (RFC BL hybrid retrieval, OQ #4). nil in tests and when the
	// server didn't wire it — searches then simply skip access tracking.
	accessFlusher *memory.AccessFlusher
}

// New builds the in-process backend. Either argument may be nil at the
// moment of construction; a nil store makes every op fail like the
// pre-MR-2 nil-Store guard would, and a nil embedder makes the vector
// ops refuse with ErrEmbedderNotConfigured exactly as before.
func New(s store.Store, e providers.Embedder) *Backend {
	return &Backend{store: s, embedder: e}
}

// SetAccessFlusher wires the batched access-count flusher (RFC BL). Optional;
// when unset, Search does no access tracking. main wires exactly one flusher
// shared across the tool's default backend.
func (b *Backend) SetAccessFlusher(f *memory.AccessFlusher) { b.accessFlusher = f }

// Get delegates to the store.
func (b *Backend) Get(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	return b.store.MemoryGet(ctx, runTenant(ctx), scope, scopeID, key)
}

// Delete delegates to the store.
func (b *Backend) Delete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	return b.store.MemoryDelete(ctx, runTenant(ctx), scope, scopeID, key)
}

// List delegates to the store.
func (b *Backend) List(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	return b.store.MemoryList(ctx, runTenant(ctx), scope, scopeID, prefix, limit)
}

// Stats delegates to the store.
func (b *Backend) Stats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	return b.store.MemoryEmbedStats(ctx, runTenant(ctx), scope)
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

	if err := b.store.MemorySet(ctx, runTenant(ctx), scope, scopeID, key, value, opts.TTL); err != nil {
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
	return b.store.MemoryEmbedSet(ctx, runTenant(ctx), scope, scopeID, key, emb)
}

// Search embeds the query, runs the vector + full-text retrieval legs, fuses
// them via RRF, re-ranks (recency/frequency terms), dedups, trims to TopK, and
// computes the index-aligned rank scores. RFC BL made this hybrid: the vector
// pool is fused with the keyword (full-text) pool so a lexical-only match can
// surface, while the default rank config still yields the underlying vector
// order when the full-text leg is empty (SQLite, or no lexical match).
func (b *Backend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.SearchQuery, rank memory.RankConfig, dedup memory.DedupConfig) (memory.SearchResult, error) {
	if !b.store.SupportsVectors() {
		return memory.SearchResult{}, store.ErrVectorUnsupported
	}
	if b.embedder == nil {
		return memory.SearchResult{}, store.ErrEmbedderNotConfigured
	}

	topK := q.TopK

	// Over-fetch both legs. Hybrid fusion + dedup both need rows below the
	// caller's top_k: RRF can promote a deeper vector row a lexical match
	// co-ranks, and dedup collapsing a near-duplicate cluster must back-fill
	// from below top_k. The pool is bounded by the store's defensive cap
	// (<=51). The extra rows beyond top_k also serve as the truncation probe.
	fetch := topK * 4
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

	tenant := runTenant(ctx)

	// Leg 1: vector (cosine-ordered). Pass errors through unwrapped —
	// ErrDimensionMismatch / ErrVectorUnsupported are user-actionable and the
	// tool's errors.Is checks match the backend-constructed *MemoryError.
	vres, err := b.store.MemoryEmbedSearch(ctx, tenant, scope, scopeID, q.Prefix, queryVec, fetch)
	if err != nil {
		return memory.SearchResult{}, err
	}

	// Leg 2: full-text (keyword, ts_rank-ordered). Degrades to empty on
	// backends without a full-text index (SQLite returns (nil,nil)), so the
	// fusion below cleanly collapses to pure-vector there.
	fres, err := b.store.MemoryFullTextSearch(ctx, tenant, scope, scopeID, q.Prefix, q.QueryText, fetch)
	if err != nil {
		return memory.SearchResult{}, err
	}

	// Fuse the two legs by Reciprocal Rank Fusion. FuseRRF writes the fused
	// value into each entry's Score, so the ranker/dedup/score pipeline below
	// runs unchanged (it reads Score as the semantic signal). With an empty
	// full-text leg the union is the vector list and its order is preserved.
	fused := memory.FuseRRF(vres, fres, memory.RRFDefaultK)

	// truncated: more distinct rows matched (across both legs) than the
	// caller's top_k, computed before the trim.
	truncated := len(fused) > topK

	// Re-rank (recency/frequency layer on top of the fused rank), then dedup
	// on the full pool BEFORE the trim (RFC I Decision 2) so collapsing a
	// duplicate cluster can promote a distinct entry into the top_k. rank
	// scores use the SAME `now` as the ranking so the rendered score matches.
	now := time.Now()
	ranked := memory.RankCandidates(fused, rank, now)
	deduped, dropped := memory.DedupResults(ranked, dedup)
	if len(deduped) > topK {
		deduped = deduped[:topK]
	}
	rankScores := memory.ScoreAll(deduped, rank, now)

	// Record a +1 access for the returned entries (batched, off the hot path
	// — the flusher writes them later). Only when wired; access tracking is
	// main-store memory only.
	if b.accessFlusher != nil {
		at := now.UTC()
		for i := range deduped {
			b.accessFlusher.Record(tenant, scope, scopeID, deduped[i].Key, at)
		}
	}

	out := memory.SearchResult{
		Entries:           deduped,
		RankScores:        rankScores,
		QueryEmbeddingDim: len(queryVec),
		Truncated:         truncated,
		DedupDropped:      dropped,
	}
	if rank.SourceReserved() {
		out.RankNote = "source_weight is reserved and contributes 0 until source-score tracking ships"
	}
	return out, nil
}

var _ memory.Backend = (*Backend)(nil)
