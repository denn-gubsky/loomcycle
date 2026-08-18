package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
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
	Scope string `json:"scope"`
	// Tenant names WHICH tenant was measured, and it is not decoration.
	//
	// This endpoint reported `models: [], total_embedding_bytes: 0` for a scope holding
	// ~2,900 embeddings, because it resolved the tenant from the caller's principal and
	// an admin's own tenant is not the one being inspected. A zero with no tenant beside
	// it reads as "this scope is unembedded" when it actually means "you measured
	// somewhere else" — so the answer now carries the question it answered.
	Tenant              string                        `json:"tenant"`
	Models              []store.MemoryEmbedModelStats `json:"models"`
	TotalEmbeddingBytes int64                         `json:"total_embedding_bytes"`
	Notes               []string                      `json:"notes,omitempty"`
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

// quoteTenant renders a tenant for a message, naming the default partition rather than
// showing an empty string — "" in prose reads as a missing value rather than a real one.
func quoteTenant(t string) string {
	if t == "" {
		return `"" (the default partition)`
	}
	return `"` + t + `"`
}

// handleMemoryEmbedStats serves GET /v1/_memory/embed_stats?scope=&tenant=.
// Returns per-(provider, model, dimension) row counts + total embedding bytes.
// Operators (and the UI) use this to spot multi-model scopes BEFORE running reembed.
//
// ?tenant= IS HONOURED FOR AN ADMIN, which it was not. The tenant came from the
// caller's principal alone, so an admin inspecting another tenant's scope measured
// their OWN and got zeros — indistinguishable from an unembedded scope. A tenant
// operator is still confined to its own tenant (principalTenantScope ignores the
// override), so this widens nothing.
//
// AGGREGATES ACROSS EVERY scope_id in the (tenant, scope) partition — there is no
// scope_id filter, and a caller expecting one subject's numbers would otherwise read
// the whole tenant's as that subject's. Stated in the response notes rather than left
// to be inferred.
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
			"scope must be one of: agent, user, tenant")
		return
	}
	// principalTenantScope keeps a non-admin confined to its own tenant while letting an
	// admin name one. Unlike the destructive sweeps this is a READ, so an admin naming
	// none is not refused — it resolves to the default partition and the response says
	// so, which is the ambiguity this endpoint used to have.
	tenant, _ := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	stats, err := s.store.MemoryEmbedStats(r.Context(), tenant, store.MemoryScope(scope))
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
	resp := memoryEmbedStatsResponse{
		Scope:               scope,
		Tenant:              tenant,
		Models:              stats.Models,
		TotalEmbeddingBytes: stats.TotalEmbeddingBytes,
		Notes: []string{
			"counts aggregate every scope_id in this (tenant, scope) partition — there is " +
				"no per-subject filter, so this is not one user's total.",
		},
	}
	if len(stats.Models) == 0 {
		resp.Notes = append(resp.Notes,
			"zero rows for tenant "+quoteTenant(tenant)+" — check that this is the tenant you "+
				"meant before concluding the scope is unembedded. Pass ?tenant=<id> to measure "+
				"another one (admin only).")
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// reembedEmbedBatch is how many rows are handed to the embedder per call.
//
// Not "all of them": a single Embed of a whole 1000-row page would make one failure
// cost the entire page (see the per-row fallback below), and some rows are large
// document bodies, so one request can carry a lot of tokens. 64 is two orders of
// magnitude fewer round trips than per-row while keeping a failure's blast radius
// small. The driver chunks again by its own EmbedderOptions.BatchSize
// (LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE), so an operator can still tune the wire size
// underneath this.
const reembedEmbedBatch = 64

// writeReembeddedRow stores one row's new embedding, reporting success. Shared by the
// batch path and its per-row fallback so the two cannot drift in what they record —
// Dimension in particular is the OBSERVED width of the returned vector, never the
// embedder's advertised one (a driver that cannot know its own dimension answers 0,
// and a row written with 0 makes every later search in that scope report a spurious
// dimension mismatch).
func (s *Server) writeReembeddedRow(ctx context.Context, tenantID, scope, storeScopeID string,
	row store.MemoryEntry, vec []float32, currentEmbedder memoryReembedConfigured) bool {
	return s.store.MemoryEmbedSet(ctx, tenantID, store.MemoryScope(scope), storeScopeID, row.Key,
		store.MemoryEmbedding{
			Provider:  currentEmbedder.Provider,
			Model:     currentEmbedder.Model,
			Dimension: len(vec),
			Vector:    vec,
			EmbedText: string(row.Value),
			CreatedAt: time.Now().UTC(),
		}) == nil
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
			"scope must be one of: agent, user, tenant")
		return
	}
	if adminMemoryScopeIDRequired(scope) && strings.TrimSpace(scopeID) == "" {
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
	// ?tenant= IS HONOURED FOR AN ADMIN, and an admin who names none is REFUSED.
	//
	// This route took the tenant from the caller's principal alone, which is the same
	// defect already fixed in embed_stats and guarded in purge_stale_embeddings — and
	// it was the worst of the three places to have it. An operator migrating another
	// tenant's scope after an embedder change swept their OWN partition, found nothing
	// to re-embed, and got `rows_total: 0` — indistinguishable from "already migrated".
	// The store then keeps serving that tenant's rows to a vector search that silently
	// excludes them, because nothing errors once any row of the new dimension exists.
	//
	// Refused rather than defaulted, like purge: a reembed WRITES (and spends one
	// embedder call per row), so sweeping a partition the operator never named is not
	// merely a misleading count. Has() distinguishes an explicit `?tenant=` — the
	// default partition, i.e. the whole deployment on a single-tenant install — from an
	// omitted one, so a single-tenant admin can still ask.
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	// Scoped to an AUTHENTICATED ADMIN, deliberately narrower than the sibling guard on
	// backfill_embeddings. principalTenantScope reports all=true for a request with NO
	// principal at all, which is every request on an open-mode deployment — and an
	// open-mode install has no tenants, so demanding one there is friction with no
	// safety to buy. The hazard is specifically a multi-tenant admin sweeping a
	// partition they did not name.
	pr, authed := auth.PrincipalFromContext(r.Context())
	adminMustName := authed && auth.HasScope(pr.Scopes, auth.ScopeAdmin)
	if all && adminMustName && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: memory rows are keyed on it, and this "+
				"operation re-embeds them at one model call per row. Pass ?tenant=<id>, or "+
				"?tenant= for the default tenant.")
		return
	}
	// tenant scope reembeds the single tenant-wide keyspace (store scope_id "");
	// the placeholder is dropped. Reused for the read + write-back below.
	storeScopeID := adminMemoryStoreScopeID(scope, scopeID)
	rows, err := s.store.MemoryEmbedListByModel(r.Context(), tenantID,
		store.MemoryScope(scope), storeScopeID,
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
			ScopeID:          storeScopeID,
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
	// EMBEDDED IN BATCHES, one STORE WRITE PER ROW.
	//
	// This loop used to call Embed once per row, which on a real migration is the whole
	// cost: 3,633 document-chunk rows moving from a 768d model to a 1024d one measured
	// ~12 rows/minute against a local Ollama — about five hours — because every row paid
	// a fresh HTTP round trip and prefill setup. The Embedder interface has always been
	// batch-shaped (N texts in, N vectors out, chunked again by the driver's own
	// BatchSize), so the per-row call was leaving that on the floor.
	//
	// The store write stays PER ROW on purpose: it is what makes this operation
	// resumable. A client timeout or a cancelled context mid-sweep costs only the rows
	// in the current batch, and the next call picks up whatever is left — which is how
	// an operator paginates a scope too large for one request.
	for start := 0; start < len(rows); start += reembedEmbedBatch {
		end := start + reembedEmbedBatch
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		texts := make([]string, len(batch))
		for i, row := range batch {
			texts[i] = string(row.Value)
		}
		vecs, err := s.embedder.Embed(r.Context(), texts)
		if err != nil || len(vecs) != len(batch) {
			// FALL BACK TO PER ROW rather than failing the batch. One unembeddable row
			// must not cost its batch-mates: before batching, a bad row was counted and
			// skipped and the rest still migrated, and that accounting is the thing an
			// operator reads to decide whether a sweep is done. Retrying singly restores
			// it exactly, at the cost of one extra call per genuinely bad batch.
			for _, row := range batch {
				one, oneErr := s.embedder.Embed(r.Context(), []string{string(row.Value)})
				if oneErr != nil || len(one) != 1 {
					failed++
					failedKeys = append(failedKeys, row.Key)
					continue
				}
				if !s.writeReembeddedRow(r.Context(), tenantID, scope, storeScopeID, row, one[0], currentEmbedder) {
					failed++
					failedKeys = append(failedKeys, row.Key)
					continue
				}
				reembedded++
			}
			continue
		}
		for i, row := range batch {
			if !s.writeReembeddedRow(r.Context(), tenantID, scope, storeScopeID, row, vecs[i], currentEmbedder) {
				failed++
				failedKeys = append(failedKeys, row.Key)
				continue
			}
			reembedded++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memoryReembedRealResponse{
		Scope:           scope,
		ScopeID:         storeScopeID,
		DryRun:          false,
		RowsReembedded:  reembedded,
		RowsFailed:      failed,
		CurrentEmbedder: currentEmbedder,
		FailedKeys:      failedKeys,
	})
}
