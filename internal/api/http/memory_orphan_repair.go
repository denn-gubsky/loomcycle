package http

import (
	"encoding/json"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memoryOrphanRepairRequest is the POST /v1/_memory/repair-tenant body.
//
// The surface exists because this repair has to be reachable without a shell:
// the deployments most likely to carry stranded rows are appliance-style ones
// (the TrueNAS app, a container with no exec), and the alternative is asking an
// operator to hand-write a collision-aware UPDATE against a live database.
type memoryOrphanRepairRequest struct {
	// Tenant to re-stamp stranded rows onto. REQUIRED to apply, and never
	// defaulted from the caller: the "" partition records no owner, so the
	// server has nothing to infer from, and the principal's own tenant is a bad
	// proxy — a legacy/open-mode token reports tenant "default", which holds
	// none of the stranded rows and would be a plausible-but-wrong destination
	// for a bulk rewrite. Omit it on a dry run to discover the candidates.
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
	// An empty body is a valid tenantless dry run — the discovery call below.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body: "+err.Error())
			return
		}
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	// A dry run with no tenant is a legitimate question — "what is stranded, and
	// which tenants could own it?" — and answering it is how a caller learns a
	// valid target without shell access. The orphan total is target-independent;
	// collisions and per-scope groups are not, so they are omitted rather than
	// reported against a guessed tenant.
	if req.Tenant == "" {
		if !dryRun {
			writeJSONError(w, http.StatusBadRequest, "tenant_required",
				`"tenant" is required to apply: "" is the legacy partition itself rather than a repair target, and it records no owner for the server to infer one from. POST with dry_run to list candidate_tenants.`)
			return
		}
		legacy, tenants, err := s.store.MemoryLegacyTenantStats(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "scan_failed", err.Error())
			return
		}
		writeJSONOK(w, memoryOrphanRepairResponse{
			MemoryOrphanReport: store.MemoryOrphanReport{Orphaned: legacy},
			CandidateTenants:   tenants,
		})
		return
	}

	var (
		rep store.MemoryOrphanReport
		err error
	)
	if dryRun {
		rep, err = s.store.MemoryOrphanScan(r.Context(), req.Tenant)
	} else {
		rep, err = s.store.MemoryOrphanRepair(r.Context(), req.Tenant, req.ScopeIDs)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "repair_failed", err.Error())
		return
	}
	writeJSONOK(w, memoryOrphanRepairResponse{
		Tenant:             req.Tenant,
		MemoryOrphanReport: rep,
	})
}

// memoryOrphanRepairResponse echoes the tenant acted on, and on a tenantless dry
// run lists the tenants that hold rows — the valid targets, discoverable without
// a shell so a picker can be offered instead of a free-text field.
type memoryOrphanRepairResponse struct {
	Tenant string `json:"tenant,omitempty"`
	store.MemoryOrphanReport
	CandidateTenants []string `json:"candidate_tenants,omitempty"`
}
