package http

import (
	"net/http"
	"strings"

	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// memory_collapse.go — RFC CV P2f: give a fact ONE home.
//
// A fact is currently written twice: a `memory/<class>/<slug>` row in the k/v
// plane and a chunk whose natural_key is that same key, verbatim. This deletes the
// k/v half, leaving the chunk as the fact's only home — one text, one embedding,
// one provenance record.
//
// IT REFUSES RATHER THAN LOSING ANYTHING. The check is not "did the migration
// report success" but "does every fact it is about to delete exist in the other
// plane", verified by LISTING BOTH PLANES. A fact with no chunk is a fact with
// nowhere to go, so one unmirrored row stops the whole scope — the alternative is
// a sweep that reports a clean run and has silently dropped rows, which is the
// failure this project has already been bitten by on a purge trusted for its own
// count.
//
// IT TOUCHES `memory/` ROWS ONLY. Document chunk BODIES live in the same keyspace
// at `doc.chunk:<hex>` and are the thing a fact is being moved INTO — deleting one
// would destroy the document store. The prefix is asserted per row immediately
// before the delete rather than trusted to the list query, because that is the one
// mistake in this file that could not be undone.
//
// access_count comes with the fact. It feeds the recall ranker's frequency term,
// and it lives on the row being deleted while the surviving body row starts at
// zero — measured at 726 accesses across 89 facts, against 0 on their bodies. Left
// uncopied, every fact's frequency term silently resets and ranking shifts, which
// is an accuracy change in a phase whose gate forbids one.

const memoryCollapseSampleCap = 20

type memoryCollapseReport struct {
	Scope      string   `json:"scope"`
	ScopeID    string   `json:"scope_id"`
	Tenant     string   `json:"tenant"`
	Facts      int      `json:"facts"`
	Mirrored   int      `json:"mirrored"`
	Unmirrored int      `json:"unmirrored"`
	Sample     []string `json:"unmirrored_sample,omitempty"`
	Copied     int      `json:"access_counts_copied"`
	Deleted    int      `json:"deleted"`
	DryRun     bool     `json:"dry_run"`
	Note       string   `json:"note,omitempty"`
}

// handleMemoryCollapseFacts serves POST
// /v1/_memory/collapse_facts?scope=&scope_id=&tenant=&dry_run=
func (s *Server) handleMemoryCollapseFacts(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; the collapse requires a persistent store")
		return
	}
	if s.sqlMem == nil {
		// Without SQL Memory there is no chunk plane, so every fact would read as
		// unmirrored and the refusal below would fire on the whole scope. Saying so
		// here is the difference between "not configured" and "your data is broken".
		writeJSONError(w, http.StatusServiceUnavailable, "sqlmem_unavailable",
			"SQL Memory is not enabled, so there is no chunk plane to collapse onto")
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
	// dry_run defaults TRUE, like the stale-embedding purge and for the stronger
	// reason: this deletes the row a fact has lived in since before the chunk tier.
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: memory rows are keyed on it, and this "+
				"operation DELETES fact rows. Pass ?tenant=<id>, or ?tenant= for the "+
				"default tenant.")
		return
	}

	// The fact rows, and ONLY the fact rows. The prefix is the query's, and it is
	// asserted again per row below.
	rows, truncated, err := s.store.MemoryList(r.Context(), tenant,
		store.MemoryScope(scope), scopeID, builtin.MemoryFactKeyPrefix, 100000)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}

	rep := memoryCollapseReport{Scope: scope, ScopeID: scopeID, Tenant: tenant, DryRun: dryRun}
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	mirror, err := doc.FactChunksByNaturalKey(r.Context(), tenant, store.MemoryScope(scope), scopeID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "chunk_plane_unreadable",
			"could not read the chunk plane, so it is not known whether these facts have a "+
				"home there: "+err.Error())
		return
	}

	var deletable []store.MemoryEntry
	for _, e := range rows {
		// BELT AND BRACES. The list was prefix-scoped, but this is the assertion that
		// stands between a fact collapse and the document store.
		if !strings.HasPrefix(e.Key, builtin.MemoryFactKeyPrefix) {
			continue
		}
		rep.Facts++
		if _, ok := mirror[e.Key]; ok {
			rep.Mirrored++
			deletable = append(deletable, e)
			continue
		}
		rep.Unmirrored++
		if len(rep.Sample) < memoryCollapseSampleCap {
			rep.Sample = append(rep.Sample, e.Key)
		}
	}
	if truncated {
		rep.Note = "the fact listing was truncated, so this covers only part of the scope"
	}

	if rep.Unmirrored > 0 {
		// REFUSE THE WHOLE SCOPE. A partial collapse would delete the facts that have a
		// home and leave the ones that do not, which is the worst of both: the operator
		// sees a success and the un-homed facts are still one run away from deletion.
		writeJSONError(w, http.StatusConflict, "unmirrored_facts",
			"refusing to collapse: "+strconv.Itoa(rep.Unmirrored)+" of "+strconv.Itoa(rep.Facts)+
				" facts have no chunk, so deleting their k/v row would delete the fact. "+
				"Run a consolidation pass to mirror them first. Sample: "+
				strings.Join(rep.Sample, ", "))
		return
	}

	if dryRun {
		rep.Note = strings.TrimSpace(rep.Note + " DRY RUN — nothing was copied or deleted. Re-send with dry_run=false.")
		writeJSON(w, http.StatusOK, rep)
		return
	}

	// COPY BEFORE DELETE, and in that order deliberately: a crash between the two
	// leaves a fact with both homes and a stale counter, which the next run fixes.
	// The reverse would lose the counter with no way back.
	for _, e := range deletable {
		bodyKey := mirror[e.Key]
		if e.AccessCount <= 0 {
			continue
		}
		if err := s.store.MemoryBumpAccessBatch(r.Context(), []store.MemoryAccessBump{{
			TenantID: tenant, Scope: store.MemoryScope(scope), ScopeID: scopeID,
			Key: bodyKey, CountDelta: e.AccessCount, LastAccess: e.LastAccessedAt,
		}}); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "access_copy_failed",
				"stopped before deleting anything: the retrieval counters could not be "+
					"carried onto the surviving row, and losing them shifts recall ranking: "+err.Error())
			return
		}
		rep.Copied++
	}

	for _, e := range deletable {
		if !strings.HasPrefix(e.Key, builtin.MemoryFactKeyPrefix) {
			continue // unreachable; the invariant is asserted where it is acted on
		}
		if _, err := s.store.MemoryDelete(r.Context(), tenant, store.MemoryScope(scope), scopeID, e.Key); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "delete_failed",
				"collapsed "+strconv.Itoa(rep.Deleted)+" of "+strconv.Itoa(len(deletable))+" before failing on "+
					e.Key+": "+err.Error())
			return
		}
		rep.Deleted++
	}
	writeJSON(w, http.StatusOK, rep)
}
