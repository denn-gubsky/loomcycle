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
	"log"
	"time"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
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

// Search embeds the query, retrieves a candidate pool, re-ranks (recency/
// frequency terms), dedups, trims to TopK, and computes the index-aligned rank
// scores. RFC BL made retrieval hybrid BY DEFAULT wherever it can contribute:
// when the store has a full-text index the vector pool is fused with the
// keyword pool via RRF so a lexical-only match can surface. A pure-semantic
// search with dedup off against a store WITHOUT full-text (e.g. sqlite-vec, or
// Postgres without pgvector) takes the cheap pure-vector path instead — one
// cosine call, no keyword round-trip, raw cosine ordering unchanged.
func (b *Backend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.SearchQuery, rank memory.RankConfig, dedup memory.DedupConfig) (memory.SearchResult, error) {
	if !b.store.SupportsVectors() {
		return memory.SearchResult{}, store.ErrVectorUnsupported
	}
	if b.embedder == nil {
		return memory.SearchResult{}, store.ErrEmbedderNotConfigured
	}

	// RFC BL PR6: one loomcycle.memory.search span per retrieval — the span
	// duration is the retrieval latency (the loomcycle.memory.search.latency
	// histogram derives from it downstream), and it carries the mode + dead-link
	// counts set at the end. Opened AFTER the two pre-flight refusals so the
	// latency series measures real retrieval, not an instantaneous config error.
	// No-op-safe: a no-op tracer when OTEL is unconfigured.
	ctx, span := lcotel.RecordMemorySearch(ctx, "inprocess")
	defer span.End()

	topK := q.TopK

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

	// Hybrid (over-fetch + full-text leg + RRF) is the RFC BL default whenever
	// it can contribute: the store HAS a full-text index, OR the caller's rank/
	// dedup needs the deeper pool anyway (a non-pure-semantic rank re-scores
	// rows below top_k; dedup must back-fill a collapsed cluster). Only a
	// pure-semantic search with dedup off against a store WITHOUT full-text
	// gains nothing from the keyword round-trip — take the cheap pure-vector
	// path there (this restores the pre-PR2 zero-cost path for those backends).
	//
	// Gated on SupportsFullText, NOT SupportsVectors: SupportsVectors is a
	// prerequisite already checked above, so it cannot distinguish a
	// vector-capable store that lacks a full-text index (e.g. a future
	// sqlite-vec build) — SupportsFullText is the precise capability.
	// TODO(RFC BL): per-call/opsconfig hybrid opt-out if operators want it.
	hybrid := b.store.SupportsFullText() || !rank.IsPureSemantic() || dedup.Enabled

	var pool []store.MemorySearchEntry
	if hybrid {
		// Over-fetch both legs. RRF can promote a deeper vector row a lexical
		// match co-ranks, and dedup collapsing a near-duplicate cluster must
		// back-fill from below top_k. The pool is bounded by the store's
		// defensive cap (<=51); the rows beyond top_k also probe truncation.
		fetch := topK * 4
		if fetch > 51 {
			fetch = 51
		}
		// Leg 1: vector (cosine-ordered). Errors pass through unwrapped —
		// ErrDimensionMismatch / ErrVectorUnsupported are user-actionable and
		// the tool's errors.Is checks match the backend-constructed *MemoryError.
		vres, verr := b.store.MemoryEmbedSearch(ctx, tenant, scope, scopeID, q.Prefix, queryVec, fetch)
		if verr != nil {
			return memory.SearchResult{}, verr
		}
		// Leg 2: full-text (keyword, ts_rank-ordered). (nil,nil) on a store
		// without a full-text index, so the fusion collapses to pure-vector.
		fres, ferr := b.store.MemoryFullTextSearch(ctx, tenant, scope, scopeID, q.Prefix, q.QueryText, fetch)
		if ferr != nil {
			return memory.SearchResult{}, ferr
		}
		// Fuse by RRF: SemanticScore := the fused rank (the ranker's semantic
		// input); Score stays each row's raw cosine. With an empty full-text
		// leg the union is the vector list and its order is preserved.
		pool = memory.FuseRRF(vres, fres, memory.RRFDefaultK)
	} else {
		// Cheap pure-vector path: a single cosine call with a +1 truncation
		// probe, no keyword round-trip. The semantic signal IS the raw cosine,
		// so mirror Score into SemanticScore (the ranker reads that) and leave
		// Score untouched for the tool to render.
		fetch := topK + 1
		if fetch > 51 {
			fetch = 51
		}
		vres, verr := b.store.MemoryEmbedSearch(ctx, tenant, scope, scopeID, q.Prefix, queryVec, fetch)
		if verr != nil {
			return memory.SearchResult{}, verr
		}
		for i := range vres {
			vres[i].SemanticScore = vres[i].Score
		}
		pool = vres
	}

	// truncated: more distinct rows matched than the caller's top_k, computed
	// before the trim.
	truncated := len(pool) > topK

	// Re-rank (recency/frequency layer on top of the semantic signal), then
	// dedup on the full pool BEFORE the trim (RFC I Decision 2) so collapsing a
	// duplicate cluster can promote a distinct entry into the top_k. rank
	// scores use the SAME `now` as the ranking so the rendered score matches.
	now := time.Now()
	ranked := memory.RankCandidates(pool, rank, now)
	deduped, dropped := memory.DedupResults(ranked, dedup)
	if len(deduped) > topK {
		deduped = deduped[:topK]
	}

	// RFC BL §2.10 read-time dead-link guard: drop any surviving hit whose
	// backing resource no longer resolves, BEFORE scoring/access-recording so a
	// dead link is never scored, never access-bumped, never returned. Runs only
	// over the trimmed top-k, so it is cheap and no-ops when everything resolves
	// (the common case — the vector population FK-cascades in P1).
	deduped, deadDropped := dropDeadLinks(deduped)

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

	mode := "vector"
	if hybrid {
		mode = "hybrid"
	}
	lcotel.SetMemorySearchResult(span, mode, topK, deadDropped)

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

// dropDeadLinks is the RFC BL §2.10 read-time dead-link floor for the memory
// tier: it removes ranked hits whose backing resource no longer resolves and
// reports how many were dropped. A hit whose backing k/v body is gone comes
// back with an empty Value (the embedding outlived its base row — e.g. a
// doc.chunk:<id> body that was removed); that is the dead link. In P1 the
// vector population FK-cascades with its body, so this no-ops for a consistent
// store — it is the cheap, correct safety net for the doc-memory / entity tiers
// as they grow.
//
// The cleanup signal is bounded and never blocks the read path: dropped hits
// are counted (surfaced as the loomcycle.memory.deadlink.dropped span event by
// the caller) and logged, with NO unbounded map and NO store write on the read
// path. The authoritative sweep of orphaned index rows is deferred to RFC BL P2.
func dropDeadLinks(entries []store.MemorySearchEntry) ([]store.MemorySearchEntry, int) {
	dropped := 0
	kept := make([]store.MemorySearchEntry, 0, len(entries))
	for _, e := range entries {
		if len(e.Value) == 0 {
			dropped++
			// Debug-level: dead links are rare (a store-consistency window), so
			// this stays quiet in steady state. No secrets — key only.
			log.Printf("memory.search: dropped dead-link hit key=%q (backing value no longer resolves)", e.Key)
			continue
		}
		kept = append(kept, e)
	}
	return kept, dropped
}

var _ memory.Backend = (*Backend)(nil)
