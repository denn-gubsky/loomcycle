package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// handleDocumentReindex serves POST /v1/_document/reindex — bring a scope's document
// index up to date with how chunks are indexed now (a header naming the document and
// section, then the content).
//
// Query params:
//
//	scope    — agent | user (default) | tenant; the subject comes from the operator
//	           context, ?scope_id= and ?tenant= as for describe_images
//	dry_run  — true (default) | false: a dry run reports what WOULD change and writes
//	           nothing
//	limit    — how many chunks to CHANGE in this call (default 200). Up-to-date chunks
//	           are stepped over and do not count, so a mostly-current store still makes
//	           progress
//	after    — resume cursor: pass back the previous call's next_cursor
//
// Re-invoke with the returned next_cursor while `more` is true. The pass is idempotent:
// a chunk already indexed under its current text costs a read and no embedding call.
func (s *Server) handleDocumentReindex(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sqlmem_unavailable",
			"re-indexing documents requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		scope = "user"
	}
	if scope != "agent" && scope != "user" && scope != "tenant" {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope", "scope must be one of: agent, user, tenant")
		return
	}
	dryRun := true
	if v := q.Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	if !dryRun && s.embedder == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "embedder_unavailable",
			"no embedder is configured (memory.embedder), so there is nothing to re-index with")
		return
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	ctx := s.substrateBrowseCtxFn(r)(r.Context())
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem, Embedder: s.embedder}
	rep, err := doc.ReindexScope(ctx, scope, q.Get("after"), limit, dryRun)
	if err != nil {
		if errors.Is(err, store.ErrVectorUnsupported) {
			writeJSONError(w, http.StatusServiceUnavailable, "vectors_unsupported",
				"this store has no vector index, so there is nothing to re-index: "+err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "reindex_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
