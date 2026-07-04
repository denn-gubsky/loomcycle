package http

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// usageReportResponse is the wire shape of GET /v1/_usage (RFC AV Phase 2).
type usageReportResponse struct {
	GroupBy []string               `json:"group_by"`
	From    string                 `json:"from,omitempty"`
	To      string                 `json:"to,omitempty"`
	Rows    []store.UsageAggregate `json:"rows"`
}

// handleUsageReport serves GET /v1/_usage — aggregated token usage + cost from
// the RFC AV ledger, for building spend reports.
//
// Query params (all optional):
//
//	group_by=tenant,source,provider,model,user   dimensions to group by
//	                                              (default: tenant,source)
//	from=RFC3339   ts >= from
//	to=RFC3339     ts <= to
//	tenant=acme    admin-only focus on one tenant (a tenant operator is always
//	               scoped to its own tenant; the param is ignored/logged)
//
// Tenant scoping (RFC AS): admin/legacy/open see all tenants (honoring ?tenant=);
// a substrate:tenant operator sees only its own tenant. The operator bill is
// group_by=source filtered to credential_source=operator; a tenant's consumption
// is the rows for its tenant; its self-funded spend is source in {tenant,user}.
// Read-only; 503 when the server has no store.
func (s *Server) handleUsageReport(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; usage reporting requires a persistent store")
		return
	}
	q := r.URL.Query()

	var query store.UsageQuery

	// group_by → validated dimensions (default tenant,source: the operator-vs-
	// tenant view). An unknown dimension is a 400, not silently dropped.
	groupRaw := q.Get("group_by")
	if strings.TrimSpace(groupRaw) == "" {
		groupRaw = "tenant,source"
	}
	var groupNames []string
	for _, part := range strings.Split(groupRaw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := store.UsageDimColumn(store.UsageDimension(part)); !ok {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"unknown group_by dimension: "+part+" (allowed: tenant,user,provider,model,source)")
			return
		}
		query.GroupBy = append(query.GroupBy, store.UsageDimension(part))
		groupNames = append(groupNames, part)
	}

	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "from must be RFC3339: "+err.Error())
			return
		}
		query.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "to must be RFC3339: "+err.Error())
			return
		}
		query.To = t
	}

	// Tenant-scope the report (RFC AS): a tenant operator is confined to its own
	// tenant; admin sees all + an optional ?tenant= focus.
	if tenantID, all := s.principalTenantScope(r.Context(), q.Get("tenant")); !all {
		query.TenantID = tenantID
	}

	rows, err := s.store.UsageReport(r.Context(), query)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// A Go nil slice marshals to JSON `null`, but the wire contract (and every
	// client's type, e.g. the Web UI's `rows: UsageAggregate[]`) is a JSON array.
	// Normalize an empty result to `[]` so a no-usage window (a fresh deploy, or a
	// tenant with no spend yet) doesn't send `"rows": null` — which crashed the
	// Web UI's `resp.rows.length`.
	if rows == nil {
		rows = []store.UsageAggregate{}
	}
	resp := usageReportResponse{GroupBy: groupNames, Rows: rows}
	if !query.From.IsZero() {
		resp.From = query.From.Format(time.RFC3339)
	}
	if !query.To.IsZero() {
		resp.To = query.To.Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
