package http

// memory_search.go — POST /v1/_memory/search (RFC BV phase 1).
//
// An OFF-RUN unified semantic search over a scope's memory: it runs with an
// EMPTY key prefix so results span BOTH plain k/v entries AND document-chunk
// bodies (doc.chunk:<id>), which the memory view needs to answer "where did I
// record this" across the whole stack in one query. The in-band Memory tool's
// `search` op is prefix-scoped and run-bound; this is its off-run twin for the
// admin/operator surface, matching the HTTP-only posture of the sibling
// /v1/_memory/* endpoints. Admin-only in this phase (the /v1/_* catch-all gates
// it at ScopeAdmin); a later phase re-gates the family to the tenant axis.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// parseMemorySources maps the wire strings onto the typed selector. Unknown values are
// dropped rather than rejected, matching the in-band tool: the enum is documented, and
// rejecting an unrecognised value would make a future source name break an older
// runtime.
func parseMemorySources(in []string) []memrank.Source {
	if len(in) == 0 {
		return nil
	}
	out := make([]memrank.Source, 0, len(in))
	for _, v := range in {
		switch memrank.Source(strings.ToLower(strings.TrimSpace(v))) {
		case memrank.SourceFacts:
			out = append(out, memrank.SourceFacts)
		case memrank.SourceNotes:
			out = append(out, memrank.SourceNotes)
		case memrank.SourceDocuments:
			out = append(out, memrank.SourceDocuments)
		}
	}
	return out
}

// docChunkKeyPrefix is the Memory keyspace namespace the Document tool writes
// chunk bodies under (builtin.chunkBodyKey). A hit on such a key is a document
// chunk, not a plain memory entry — kept as a literal here to avoid importing
// internal/tools/builtin into the HTTP layer.
const docChunkKeyPrefix = "doc.chunk:"

type memorySearchRequest struct {
	Query   string               `json:"query"`
	Scope   string               `json:"scope"`
	ScopeID string               `json:"scope_id"`
	TopK    int                  `json:"top_k"`
	Rank    *memrank.RankConfig  `json:"rank"`
	Dedup   *memrank.DedupConfig `json:"dedup"`
	// Sources narrows which kinds of remembered thing come back (RFC BW). Empty
	// keeps this endpoint's purpose: it exists to answer "where did I record this"
	// across BOTH planes, so unlike the in-band recall it does not default to facts.
	Sources []string `json:"sources"`
}

type memorySearchEmbeddedWith struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type memorySearchResultEntry struct {
	Key          string                   `json:"key"`
	Value        json.RawMessage          `json:"value"`
	Score        float64                  `json:"score"`      // raw cosine similarity
	RankScore    float64                  `json:"rank_score"` // hybrid rank the row was ordered by
	EmbeddedWith memorySearchEmbeddedWith `json:"embedded_with"`
	// Kind is the row's class: "fact" (a consolidator distilled it), "note" (an agent
	// wrote it directly) or "document" (a Document chunk body). A document hit also
	// carries chunk_id so the viewer can fetch its entity block via get_chunk.
	//
	// WIRE CHANGE (RFC BW §7): this used to be "memory" | "document". A reader that
	// switches on "document" is unaffected; one asserting kind == "memory" must move to
	// fact/note. The refinement is the point — "the user told me this" and "an agent
	// jotted this down" are different claims, and collapsing them is what let document
	// prose read as remembered fact.
	Kind    string `json:"kind"`
	ChunkID string `json:"chunk_id,omitempty"`
	// Document and Title are the hit's READABLE identity — the document title and the
	// chunk's own heading — for document-kind rows only. Both are best-effort: they
	// live in SQL Memory, a different plane from the bodies, so a scope with no
	// document tables or a chunk deleted mid-search simply omits them. ChunkID
	// remains the address; these annotate it.
	Document string `json:"document,omitempty"`
	Title    string `json:"title,omitempty"`
}

type memorySearchResponse struct {
	Scope             string                    `json:"scope"`
	ScopeID           string                    `json:"scope_id"`
	Entries           []memorySearchResultEntry `json:"entries"`
	QueryEmbeddingDim int                       `json:"query_embedding_dim"`
	Truncated         bool                      `json:"truncated"`
}

// handleMemorySearch serves POST /v1/_memory/search.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}

	// A query string is small; cap the body so an off-run caller can't stream a
	// large payload into an admin endpoint.
	var body memorySearchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"request body must be valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_query", "query is required")
		return
	}
	if !validAdminMemoryScope(body.Scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope", "scope must be one of: agent, user, tenant")
		return
	}
	if adminMemoryScopeIDRequired(body.Scope) && strings.TrimSpace(body.ScopeID) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id", "scope_id is required")
		return
	}
	// tenant scope searches the single tenant-wide keyspace (store scope_id "");
	// any scope_id the caller sent is a placeholder and is dropped.
	storeScopeID := adminMemoryStoreScopeID(body.Scope, body.ScopeID)

	topK := body.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}
	rankCfg := memrank.DefaultRankConfig()
	if body.Rank != nil {
		rankCfg = *body.Rank
	}
	var dedupCfg memrank.DedupConfig
	if body.Dedup != nil {
		dedupCfg = *body.Dedup
	}

	// TENANT STAMPING — security-critical. The in-process backend resolves the
	// tenant from tools.RunIdentity(ctx).TenantID ONLY (backends/inprocess.runTenant).
	// Off-run there is no RunIdentity, so without stamping the authenticated
	// principal's tenant the search would run at the shared "" tenant and could
	// read/return another tenant's rows. tenantFromCtx is caller/config-
	// authoritative — never a request-body field.
	ctx := tools.WithRunIdentity(r.Context(), tools.RunIdentityValue{TenantID: tenantFromCtx(r.Context())})

	// Empty Prefix so the search spans both k/v entries and doc.chunk bodies unless the
	// caller narrows it with sources.
	backend := inprocess.New(s.store, s.embedder)
	res, err := backend.Search(ctx, store.MemoryScope(body.Scope), storeScopeID,
		memrank.SearchQuery{
			QueryText: body.Query, Prefix: "", TopK: topK,
			Sources: parseMemorySources(body.Sources),
		}, rankCfg, dedupCfg)
	if err != nil {
		// The three typed refusals are operator-actionable (no embedder / no
		// vector index / a model swap left the stored dimension stale), so surface
		// them verbatim as a 400 rather than a blind 500.
		if errors.Is(err, memrank.ErrSourcesNotExpressible) {
			writeJSONError(w, http.StatusBadRequest, "invalid_sources", err.Error())
			return
		}
		if errors.Is(err, store.ErrDimensionMismatch) ||
			errors.Is(err, store.ErrVectorUnsupported) ||
			errors.Is(err, store.ErrEmbedderNotConfigured) {
			writeJSONError(w, http.StatusBadRequest, "search_unavailable", err.Error())
			return
		}
		log.Printf("memory search failed (scope=%s scope_id=%s): %v", body.Scope, body.ScopeID, err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "search failed")
		return
	}

	entries := make([]memorySearchResultEntry, 0, len(res.Entries))

	// Collect the document hits' chunk ids up front so their labels resolve in ONE
	// batched query rather than one per row.
	var docChunkIDs []string
	for _, e := range res.Entries {
		if memrank.Class(e) == store.MemoryRowDocument {
			docChunkIDs = append(docChunkIDs, strings.TrimPrefix(e.Key, docChunkKeyPrefix))
		}
	}
	labels := builtin.ChunkLabelsFor(ctx, s.sqlMem, tenantFromCtx(r.Context()),
		store.MemoryScope(body.Scope), storeScopeID, docChunkIDs)

	for i, e := range res.Entries {
		entry := memorySearchResultEntry{
			Key:       e.Key,
			Value:     e.Value,
			Score:     e.Score,
			RankScore: res.RankScores[i],
			EmbeddedWith: memorySearchEmbeddedWith{
				Provider: e.EmbeddedWith.Provider,
				Model:    e.EmbeddedWith.Model,
			},
			// Classified by the SAME function the in-band tool and the filter use, so
			// the label cannot disagree with what the selector selected.
			Kind: string(memrank.Class(e)),
		}
		// A doc.chunk hit may be an entity fact OR a plain document chunk; the
		// viewer calls get_chunk to see its entity block. The ENTITY sidecar is still
		// not read here — but the chunk's document + heading are, because a page of
		// opaque `doc.chunk:<hex>` keys cannot be attributed without a fetch per row.
		// One batched, best-effort query above; a fault costs the labels, not the hit.
		if entry.Kind == string(store.MemoryRowDocument) {
			entry.ChunkID = strings.TrimPrefix(e.Key, docChunkKeyPrefix)
			if lb, ok := labels[entry.ChunkID]; ok {
				entry.Document, entry.Title = lb.Document, lb.Title
			}
		}
		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, memorySearchResponse{
		Scope:             body.Scope,
		ScopeID:           storeScopeID,
		Entries:           entries,
		QueryEmbeddingDim: res.QueryEmbeddingDim,
		Truncated:         res.Truncated,
	})
}
