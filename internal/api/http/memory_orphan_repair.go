package http

import (
	"encoding/json"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memoryOrphanRepairRequest is the POST /v1/_memory/repair-tenant body.
//
// The surface exists because this repair has to be reachable without a shell:
// the deployments most likely to carry stranded rows are appliance-style ones
// (the TrueNAS app, a container with no exec), and the alternative is asking an
// operator to hand-write a collision-aware UPDATE against a live database.
type memoryOrphanRepairRequest struct {
	// Tenant to re-stamp stranded rows onto. Defaults to the caller's own
	// tenant; required when the caller has none (a legacy/open-mode admin
	// token), because the "" partition records no owner and the server must not
	// guess which tenant a row belongs to.
	Tenant string `json:"tenant,omitempty"`
	// DryRun defaults to TRUE — an omitted field reports, never writes. This is
	// a bulk row rewrite, so the safe reading of an ambiguous request is the
	// read-only one.
	DryRun *bool `json:"dry_run,omitempty"`
	// ScopeIDs narrows the move to specific scope_ids. Empty moves every
	// non-global orphan, which is correct for a single-tenant deployment; a
	// deployment whose orphans span tenants must narrow, and the scan's Groups
	// are what reveal that.
	ScopeIDs []string `json:"scope_ids,omitempty"`
}

// handleMemoryOrphanRepair scans, and on request repairs, memory rows stranded
// at the legacy "" tenant by the RFC BL tenant axis. substrate:admin (via the
// /v1/_* catch-all): it rewrites rows across every scope in one statement, which
// is not a tenant-operator's authority even over its own tenant.
func (s *Server) handleMemoryOrphanRepair(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_store", "memory repair requires a persistent store")
		return
	}
	var req memoryOrphanRepairRequest
	// An empty body is a valid dry-run request for the caller's own tenant.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body: "+err.Error())
			return
		}
	}
	tenant := req.Tenant
	if tenant == "" {
		if p, ok := auth.PrincipalFromContext(r.Context()); ok {
			tenant = p.TenantID
		}
	}
	if tenant == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			`"tenant" is required: the caller has no tenant of its own, and "" is the legacy partition itself rather than a repair target`)
		return
	}

	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	var (
		rep store.MemoryOrphanReport
		err error
	)
	if dryRun {
		rep, err = s.store.MemoryOrphanScan(r.Context(), tenant)
	} else {
		rep, err = s.store.MemoryOrphanRepair(r.Context(), tenant, req.ScopeIDs)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "repair_failed", err.Error())
		return
	}
	writeJSONOK(w, memoryOrphanRepairResponse{
		Tenant:             tenant,
		MemoryOrphanReport: rep,
	})
}

// memoryOrphanRepairResponse echoes the resolved tenant alongside the report, so
// a caller that relied on the default can see which tenant was acted on.
type memoryOrphanRepairResponse struct {
	Tenant string `json:"tenant"`
	store.MemoryOrphanReport
}
