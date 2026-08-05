package http

// memory_purge.go — POST /v1/_memory/purge_stale_embeddings.
//
// Deletes the embeddings of rows that should no longer be searchable at all, leaving
// the rows themselves alone.
//
// WHY THIS EXISTS. The write path learned to reject text that carries no language —
// markdown scaffolding ("```sh", "---", "#"), which a heading-split import turns into
// its own chunk. That stops NEW ones. It does nothing about the vectors already in the
// index, and nothing else could either: the backfill only ever ADDS an embedding, and
// /v1/_memory/reembed re-derives its text as the raw row value, so re-embedding
// faithfully reproduces the same junk. Deleting the chunk would work and is too big a
// hammer — the chunk is legitimate document structure, it simply should not be a
// search result.
//
// Measured on the reference deployment: a document search for "shell script example"
// returned "```bash" three times at 0.629 and "```sh" at 0.541, above every real hit.
// A short syntax token embeds near the centroid of everything, so it ranks mid-high
// for EVERY query.
//
// THE SAFETY PROPERTY, and the only one that matters here: an embedding is deleted
// ONLY when the row's text derives to empty under TODAY'S rules — the same
// composition the backfill uses to decide whether to CREATE one. The two must agree,
// because a purge computing "indexable" differently from the writer would delete rows
// the writer would immediately re-create, or worse, delete rows that are legitimately
// searchable. It is the inverse of the backfill, not a second opinion.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

type memoryPurgeResponse struct {
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	Prefix  string `json:"prefix,omitempty"`
	DryRun  bool   `json:"dry_run"`

	// Scanned is how many rows were examined, bounded by limit.
	Scanned int `json:"scanned"`
	// Stale is how many of them have no indexable text and therefore should carry no
	// embedding. Purged is how many embeddings were actually removed (0 on a dry run).
	Stale  int `json:"stale"`
	Purged int `json:"purged"`
	Failed int `json:"failed,omitempty"`
	// Truncated reports that MORE rows exist beyond limit — the scan is incomplete, so
	// a zero `stale` does not mean the scope is clean.
	Truncated bool `json:"truncated"`

	SampleKeys   []string `json:"sample_keys,omitempty"`
	FailedKeys   []string `json:"failed_keys,omitempty"`
	FirstFailure string   `json:"first_failure,omitempty"`
	Notes        []string `json:"notes"`
}

const memoryPurgeSampleCap = 20

// handleMemoryPurgeStaleEmbeddings serves POST
// /v1/_memory/purge_stale_embeddings?scope=&scope_id=&tenant=&prefix=&limit=&dry_run=
func (s *Server) handleMemoryPurgeStaleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; purge requires a persistent store")
		return
	}
	scope := r.URL.Query().Get("scope")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user, tenant")
		return
	}
	scopeID := r.URL.Query().Get("scope_id")
	if scopeID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id", "scope_id is required")
		return
	}
	// dry_run defaults TRUE. This one DELETES, so the default matters more than on the
	// sweeps that only add: a bare `curl -X POST` must never remove an index entry.
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	prefix := r.URL.Query().Get("prefix")

	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	// Same refusal as the backfill and the erasure surfaces: memory rows are keyed on
	// the tenant, so an admin naming none resolves to the DEFAULT tenant. On a
	// DESTRUCTIVE op that is worse than a misleading count — it would purge a tenant
	// the operator never named.
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: memory rows are keyed on it, and this "+
				"operation DELETES index entries. Pass ?tenant=<id>, or ?tenant= for the "+
				"default tenant.")
		return
	}

	rows, truncated, err := s.store.MemoryList(r.Context(), tenant,
		store.MemoryScope(scope), scopeID, prefix, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}

	resp := memoryPurgeResponse{
		Scope: scope, ScopeID: scopeID, Prefix: prefix, DryRun: dryRun,
		Scanned: len(rows), Truncated: truncated,
	}
	resp.Notes = []string{
		"a row is stale ONLY when its text derives to empty under today's rules — the " +
			"same composition the backfill uses to decide whether to CREATE an embedding. " +
			"This is the inverse of the backfill, not a second opinion, so it cannot " +
			"remove something the writer would immediately put back.",
		"the ROW is never touched; only its embedding. The chunk stays in its document, " +
			"it simply stops being a search result.",
	}
	if truncated {
		resp.Notes = append(resp.Notes,
			"SCAN INCOMPLETE — more rows exist beyond limit, so a low `stale` count is not "+
				"evidence the scope is clean. Raise limit or narrow with prefix.")
	}

	for _, row := range rows {
		if staleEmbeddingText(r.Context(), s, tenant, scope, scopeID, row) != "" {
			continue // still indexable — leave its embedding alone
		}
		resp.Stale++
		if len(resp.SampleKeys) < memoryPurgeSampleCap {
			resp.SampleKeys = append(resp.SampleKeys, row.Key)
		}
		if dryRun {
			continue
		}
		if err := s.store.MemoryEmbedDelete(r.Context(), tenant,
			store.MemoryScope(scope), scopeID, row.Key); err != nil {
			resp.Failed++
			if len(resp.FailedKeys) < memoryPurgeSampleCap {
				resp.FailedKeys = append(resp.FailedKeys, row.Key)
			}
			if resp.FirstFailure == "" {
				resp.FirstFailure = err.Error()
			}
			continue
		}
		resp.Purged++
	}

	if dryRun {
		resp.Notes = append(resp.Notes,
			"DRY RUN — nothing was deleted. Re-send with dry_run=false.")
	}
	if resp.Failed > 0 {
		resp.Notes = append(resp.Notes,
			"some deletions failed (see failed_keys, first_failure is the diagnostic). "+
				"They remain stale, so a re-invocation retries exactly those.")
	}
	writeJSON(w, http.StatusOK, resp)
}

// staleEmbeddingText returns the text a row WOULD be embedded with today, or "" when
// it should carry no embedding at all.
//
// It composes exactly what the backfill composes — embedTextForRow, then the document
// title fallback — because the two decisions must be one decision. A purge with its own
// notion of "indexable" would either delete rows the writer re-creates on the next
// touch, or delete rows that are legitimately searchable.
func staleEmbeddingText(ctx context.Context, s *Server, tenant, scope, scopeID string, row store.MemoryEntry) string {
	if text := embedTextForRow(row); text != "" {
		return text
	}
	return builtin.TitleFallbackForBodyKey(ctx, s.sqlMem, tenant,
		store.MemoryScope(scope), scopeID, row.Key)
}
