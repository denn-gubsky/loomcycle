package http

import (
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// memory_home_facts.go — RFC CV P3: the migration that moves facts already stored in
// the shared `/memory/entities` document into the document that IS their subject.
//
// A store written before subject-homing keeps every fact in one document with the
// subject as a sibling chunk. A store written after it keeps `/facts/<slug>` per
// subject, root chunk = the entity. Both READ correctly — the fact surfaces search
// and the graph walks edges, and neither cares where a chunk is filed — so this is
// not a repair. It is the step that stops the two shapes disagreeing about IDENTITY:
// the old subject chunk and the new document root carry the same natural key, and a
// key resolves scope-wide, so a later write finds whichever the database hands back
// first. One subject, two nodes, each collecting a different half of what is known.
//
// Operator-triggered rather than run-at-boot, and dry_run defaults TRUE, for the
// reason the collapse gave: a migration that runs itself is a migration nobody reads
// the report of. The work itself is in builtin, where createDocument and the chunk
// transaction live — this is the scoping, the authorization and the refusal.

// handleMemoryHomeFacts serves POST
// /v1/_memory/home_facts?scope=&scope_id=&tenant=&dry_run=
func (s *Server) handleMemoryHomeFacts(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; subject homing requires a persistent store")
		return
	}
	if s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sqlmem_unavailable",
			"SQL Memory is not enabled, so there is no chunk plane to home facts in")
		return
	}
	scope := r.URL.Query().Get("scope")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user, tenant")
		return
	}
	scopeID := r.URL.Query().Get("scope_id")
	if adminMemoryScopeIDRequired(scope) && strings.TrimSpace(scopeID) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id",
			"scope_id is required for scope="+scope+" (tenant is a single keyspace and needs none)")
		return
	}
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	if all && !r.URL.Query().Has("tenant") {
		// The same refusal the collapse makes, for a weaker but still real reason:
		// nothing here is deleted but a subject node whose contents have already
		// moved, yet the documents this CREATES are named in a tenant's Path tree and
		// an unnamed tenant would silently be the default one.
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: memory rows are keyed on it, and this "+
				"operation MOVES facts between documents. Pass ?tenant=<id>, or ?tenant= for "+
				"the default tenant.")
		return
	}

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	rep, err := doc.HomeFactsUnderSubjects(r.Context(), tenant, store.MemoryScope(scope), scopeID, dryRun)
	if err != nil {
		// A fault mid-scope leaves the subjects already committed homed and the rest
		// where they were — both shapes read, so a partial run is a smaller job next
		// time rather than a broken store. The report says how far it got.
		writeJSONError(w, http.StatusInternalServerError, "homing_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
