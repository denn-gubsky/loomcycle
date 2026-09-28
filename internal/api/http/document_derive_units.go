package http

import (
	"net/http"
	"strconv"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// SetUnitGenerator installs the memory.unit_generator model that
// POST /v1/_document/derive_units writes units with. Nil (the default) lets the
// route dry-run but not write.
func (s *Server) SetUnitGenerator(g memrank.UnitGenerator) { s.unitGenerator = g }

// handleDeriveUnits serves POST /v1/_document/derive_units — generate Document
// derived search units for the documents that opted in (their own index_units, or
// a subtree marked in memory.unit_generator.subtrees), skipping the memory trees.
//
// Query params:
//
//	scope    — agent | user (default) | tenant; ?scope_id= / ?tenant= as for the
//	           other Document browse routes
//	dry_run  — true (default) | false: a dry run makes no model call and writes
//	           nothing, and reports what WOULD be generated
//	limit    — chunks to generate (model calls) in this call; default 25, max 500
//	after    — resume cursor: pass back the previous call's next_cursor
//
// Re-invoke with next_cursor while `more` is true. Re-running is safe: a chunk whose
// units were written from its current body is up to date and costs nothing.
func (s *Server) handleDeriveUnits(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sqlmem_unavailable",
			"derived units require SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
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
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if !dryRun {
		if s.unitGenerator == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "unit_generator_unavailable",
				"no memory.unit_generator is configured, so no units can be written (a dry run still reports what would be)")
			return
		}
		if s.embedder == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "embedder_unavailable",
				"no embedder is configured, so a written unit could never be found")
			return
		}
		// The generator is called directly with the operator's key — the same
		// choke point describe_images refuses a restricted principal at.
		if s.operatorKeyRestrictedForCtx(r.Context()) {
			writeJSONError(w, http.StatusForbidden, "operator_key_restricted", operatorKeyRestrictedMsg)
			return
		}
	}

	ctx := s.substrateBrowseCtxFn(r)(r.Context())
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem, Embedder: s.embedder}
	rep, err := doc.DeriveUnits(ctx, scope, s.unitGenerator, builtin.DeriveUnitsOptions{
		Generator: s.cfg().Memory.UnitGenerator,
		After:     q.Get("after"),
		Limit:     limit,
		DryRun:    dryRun,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "derive_units_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
