package http

import (
	"net/http"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// handleDocumentProject serves POST /v1/_document/project — run the document
// projection pass (RFC FQ P2) over one scope now, rather than waiting for the
// sweeper: fit the scope's 3D map where it is due (or forced) and place the chunks
// missing from it. The coordinates are what graph_snapshot's projection block and
// search's query_xyz serve.
//
// Query params, as for /v1/_document/reindex:
//
//	scope    — agent | user (default) | tenant; the subject comes from the operator
//	           context, ?scope_id= and ?tenant= as for the re-index
//	dry_run  — true (default) | false: a dry run reports what WOULD change and writes
//	           nothing
//	refit    — true | false (default): fit a new basis even when the current one is fresh
//	limit    — the most chunks to place in this call (default 5000)
//	method   — pca | umap: fit with this method instead of memory.projection.method,
//	           to compare the two maps of one scope side by side (RFC FQ §11). A map
//	           of the other method is refitted, so the next pass under the configured
//	           method (the sweeper's, or a call without method) fits that one again.
//
// Responds {scope, dry_run, model, basis_version, method, placed, refitted, raced,
// skipped, unsupported, more}; re-invoke while `more` is true. `raced` means the
// call set out to refit but another pass over the scope (the sweeper, another call,
// another replica) saved that basis first: `refitted` is then false and the chunks
// were placed on the basis that pass saved. It reads stored vectors and makes no
// model call; a UMAP fit takes seconds to a minute at 10,000 chunks, and the call
// returns when it is done. 409 while memory.projection is off.
func (s *Server) handleDocumentProject(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sqlmem_unavailable",
			"projecting documents requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
		return
	}
	cfg := s.cfg()
	if !cfg.DocumentProjection().Enabled {
		writeJSONError(w, http.StatusConflict, "projection_disabled",
			"the document projection is off: set memory.projection.enabled (or LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL)")
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
	// A real pass rewrites the scope's map, and a refit is a long fit on the
	// operator's compute even in a dry run: both are writes.
	ctx, ok := s.browseCtx(w, r, scope, !dryRunParam(q.Get("dry_run")) || q.Get("refit") == "true" || q.Get("refit") == "1")
	if !ok {
		return
	}
	if s.embedder == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "embedder_unavailable",
			"no embedder is configured (memory.embedder), so there is no model whose vectors to place")
		return
	}
	opts := builtin.ProjectionOptions{DryRun: true}
	if v := q.Get("dry_run"); v != "" {
		opts.DryRun = v != "false" && v != "0"
	}
	if v := q.Get("refit"); v != "" {
		opts.Refit = v == "true" || v == "1"
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.PlaceLimit = n
		}
	}
	switch m := q.Get("method"); m {
	case "", config.ProjectionMethodPCA, config.ProjectionMethodUMAP:
		opts.Method = m
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid_method", "method must be one of: pca, umap")
		return
	}

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem, Embedder: s.embedder, Cfg: cfg}
	rep, err := doc.ProjectScope(ctx, scope, opts)
	if err != nil {
		if builtin.ErrProjectionDisabled(err) {
			writeJSONError(w, http.StatusConflict, "projection_disabled", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "project_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
