package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// v0.9.0 Vector Memory admin endpoints. Drive the Web UI Memory tab's
// new vector features + give operators a CLI-friendly hook for
// embedder migrations.
//
// Routes (both bearer-authed at the mux layer):
//
//	GET  /v1/_memory/embed_stats?scope=
//	  → per-(provider, model) row counts + total embedding bytes
//	    for one scope. Drives the UI's model-distribution badge.
//
//	POST /v1/_memory/reembed?scope=&scope_id=&dry_run=true|false
//	  → walks rows under (scope, scope_id) whose stored embedder
//	    DIFFERS from the configured one; dry_run=true returns the
//	    list + counts; dry_run=false re-embeds in batches and
//	    returns rows_reembedded.

type memoryEmbedStatsResponse struct {
	Scope               string                        `json:"scope"`
	Models              []store.MemoryEmbedModelStats `json:"models"`
	TotalEmbeddingBytes int64                         `json:"total_embedding_bytes"`
}

type memoryReembedDryRunResponse struct {
	Scope            string                  `json:"scope"`
	ScopeID          string                  `json:"scope_id"`
	DryRun           bool                    `json:"dry_run"`
	RowsTotal        int                     `json:"rows_total"`
	RowsToReembed    int                     `json:"rows_to_reembed"`
	CurrentEmbedder  memoryReembedConfigured `json:"current_embedder"`
	SampleKeys       []string                `json:"sample_keys"`
	SampleKeysCapped bool                    `json:"sample_keys_capped"`
}

type memoryReembedRealResponse struct {
	Scope           string                  `json:"scope"`
	ScopeID         string                  `json:"scope_id"`
	DryRun          bool                    `json:"dry_run"`
	RowsReembedded  int                     `json:"rows_reembedded"`
	RowsFailed      int                     `json:"rows_failed"`
	CurrentEmbedder memoryReembedConfigured `json:"current_embedder"`
	FailedKeys      []string                `json:"failed_keys,omitempty"`
}

type memoryReembedConfigured struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
}

// handleMemoryEmbedStats serves GET /v1/_memory/embed_stats?scope=.
// Returns per-(provider, model, dimension) row counts + total
// embedding bytes for the scope. Operators (and the UI) use this to
// spot multi-model scopes BEFORE running reembed.
func (s *Server) handleMemoryEmbedStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}
	if !s.store.SupportsVectors() {
		writeJSONError(w, http.StatusServiceUnavailable, "vector_unsupported",
			"this backend has no vector support; set LOOMCYCLE_PGVECTOR_ENABLED=1 on Postgres (sqlite-vec ships in v0.9.1)")
		return
	}
	scope := r.URL.Query().Get("scope")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user")
		return
	}
	stats, err := s.store.MemoryEmbedStats(r.Context(), "", store.MemoryScope(scope))
	if err != nil {
		// Vector-unsupported can also surface here from refusal-stub
		// backends — treat as 503 for consistency with the upfront
		// check (operators see the same code in both paths).
		if errors.Is(err, store.ErrVectorUnsupported) {
			writeJSONError(w, http.StatusServiceUnavailable, "vector_unsupported", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// JSON-encode an empty array rather than null so the UI can
	// `.map` without a guard.
	if stats.Models == nil {
		stats.Models = []store.MemoryEmbedModelStats{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memoryEmbedStatsResponse{
		Scope:               scope,
		Models:              stats.Models,
		TotalEmbeddingBytes: stats.TotalEmbeddingBytes,
	})
}

// handleMemoryReembed serves POST /v1/_memory/reembed.
//
// Query params:
//
//	scope     — agent | user (required)
//	scope_id  — required
//	dry_run   — true (default) | false — true returns the planned
//	            migration without writing; false executes it
//
// The configured embedder is taken from the live Server.embedder
// (the same instance the Memory tool holds). When dry_run=false, the
// store re-embeds rows whose (provider, model) doesn't match the
// current embedder and writes them back via MemoryEmbedSet. Failures
// are collected (not fatal) — operators see which keys to retry.
func (s *Server) handleMemoryReembed(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}
	if !s.store.SupportsVectors() {
		writeJSONError(w, http.StatusServiceUnavailable, "vector_unsupported",
			"this backend has no vector support; set LOOMCYCLE_PGVECTOR_ENABLED=1 on Postgres")
		return
	}
	if s.embedder == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "embedder_not_configured",
			"no embedder configured; set memory.embedder in operator yaml")
		return
	}
	scope := r.URL.Query().Get("scope")
	scopeID := r.URL.Query().Get("scope_id")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user")
		return
	}
	if strings.TrimSpace(scopeID) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id",
			"scope_id is required")
		return
	}
	// dry_run defaults to TRUE so an operator with `curl -X POST` and
	// no flag doesn't accidentally re-embed an entire scope. Explicit
	// dry_run=false to commit.
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = !(v == "false" || v == "0")
	}

	currentEmbedder := memoryReembedConfigured{
		Provider:  s.embedder.Provider(),
		Model:     s.embedder.Model(),
		Dimension: s.embedder.Dimension(),
	}

	// Fetch the rows-needing-reembed list. Limit caps total work per
	// request — operators with huge scopes paginate by re-calling.
	limit := 1000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			limit = n
		}
	}
	rows, err := s.store.MemoryEmbedListByModel(r.Context(), "",
		store.MemoryScope(scope), scopeID,
		currentEmbedder.Provider, currentEmbedder.Model, limit)
	if err != nil {
		if errors.Is(err, store.ErrVectorUnsupported) {
			writeJSONError(w, http.StatusServiceUnavailable, "vector_unsupported", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	if dryRun {
		// Sample up to 20 keys so operators can spot patterns before
		// committing (e.g. "rows under prefix users/ are old; rows
		// under config/ already current — let me check the config ones").
		const sampleCap = 20
		sample := make([]string, 0, sampleCap)
		for i, row := range rows {
			if i >= sampleCap {
				break
			}
			sample = append(sample, row.Key)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(memoryReembedDryRunResponse{
			Scope:            scope,
			ScopeID:          scopeID,
			DryRun:           true,
			RowsTotal:        len(rows), // we already filtered to needs-reembed; not the total in scope
			RowsToReembed:    len(rows),
			CurrentEmbedder:  currentEmbedder,
			SampleKeys:       sample,
			SampleKeysCapped: len(rows) > sampleCap,
		})
		return
	}

	// Real run. Re-embed each row using the row's `value` field as
	// the text. Operators wanting to re-embed against a custom text
	// (e.g. preserving the original `embed_text`) can fetch the
	// stored embedding first via MemoryEmbedGet — that's a v0.9.x
	// nice-to-have, not in scope for v0.9.0's "swap models on the
	// existing corpus" use case.
	var (
		reembedded int
		failed     int
		failedKeys []string
	)
	for _, row := range rows {
		texts := []string{string(row.Value)}
		vecs, err := s.embedder.Embed(r.Context(), texts)
		if err != nil || len(vecs) != 1 {
			failed++
			failedKeys = append(failedKeys, row.Key)
			continue
		}
		emb := store.MemoryEmbedding{
			Provider:  currentEmbedder.Provider,
			Model:     currentEmbedder.Model,
			Dimension: len(vecs[0]),
			Vector:    vecs[0],
			EmbedText: string(row.Value),
			CreatedAt: time.Now().UTC(),
		}
		if err := s.store.MemoryEmbedSet(r.Context(), "",
			store.MemoryScope(scope), scopeID, row.Key, emb); err != nil {
			failed++
			failedKeys = append(failedKeys, row.Key)
			continue
		}
		reembedded++
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memoryReembedRealResponse{
		Scope:           scope,
		ScopeID:         scopeID,
		DryRun:          false,
		RowsReembedded:  reembedded,
		RowsFailed:      failed,
		CurrentEmbedder: currentEmbedder,
		FailedKeys:      failedKeys,
	})
}
