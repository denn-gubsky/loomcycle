package http

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// usageReportResponse is the wire shape of GET /v1/_usage (RFC AV Phase 2).
type usageReportResponse struct {
	GroupBy []string               `json:"group_by"`
	From    string                 `json:"from,omitempty"`
	To      string                 `json:"to,omitempty"`
	Bucket  string                 `json:"bucket,omitempty"`
	Rows    []store.UsageAggregate `json:"rows"`
	// DetailSince is the oldest instant the per-call ledger still holds;
	// before it only whole days survive (the archive), so hour buckets and the
	// agent dimension stop there. Absent when nothing is archived.
	DetailSince string `json:"detail_since,omitempty"`
}

// UsageDetailSince is the oldest instant the per-call usage ledger holds:
// zero when the sweeper is off, since nothing is then folded into the archive.
func (s *Server) UsageDetailSince() time.Time {
	cfg := s.cfg()
	if cfg == nil {
		return time.Time{}
	}
	return store.UsageDetailSince(time.Now(), cfg.Env.UsageDetailRetention, cfg.Env.UsageSweeperEnabled)
}

// handleUsageReport serves GET /v1/_usage — aggregated token usage + cost from
// the RFC AV ledger, for building spend reports and charts.
//
// Query params (all optional):
//
//	group_by=tenant,user,provider,model,source,purpose,tier,user_tier,agent,class
//	               dimensions to group by (default: tenant,source)
//	from=RFC3339   ts >= from
//	to=RFC3339     ts <= to
//	bucket=hour|day|week|month
//	               split the window into UTC periods; each row carries period
//	               (hour only within detail_since)
//	top=N          keep the N largest values of the first group_by dimension
//	top_by=cost|tokens|calls
//	               what "largest" means (default cost); the rest fold into
//	               rows marked other
//	<dimension>=a,b
//	               keep rows whose dimension is one of the values, for any
//	               dimension but tenant
//	tenant=acme    admin-only focus on one tenant (a tenant operator is always
//	               scoped to its own tenant; the param is ignored)
//
// Tenant scoping (RFC AS): admin/legacy/open see all tenants (honoring ?tenant=);
// a substrate:tenant operator sees only its own tenant, so every filter only
// narrows within it. Read-only; 503 when the server has no store.
func (s *Server) handleUsageReport(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; usage reporting requires a persistent store")
		return
	}
	q := r.URL.Query()

	// group_by defaults to tenant,source: the operator-vs-tenant view.
	groupRaw := q.Get("group_by")
	if strings.TrimSpace(groupRaw) == "" {
		groupRaw = "tenant,source"
	}
	params := store.UsageParams{
		GroupBy: strings.Split(groupRaw, ","),
		From:    q.Get("from"),
		To:      q.Get("to"),
		Bucket:  q.Get("bucket"),
		TopBy:   q.Get("top_by"),
	}
	if v := q.Get("top"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "top must be a number")
			return
		}
		params.Top = n
	}
	// Every dimension but tenant is a filter by its own name; tenant keeps its
	// meaning as the admin's focus, with its authority check below.
	for _, d := range store.UsageCanonicalDims {
		if d == store.UsageByTenant {
			continue
		}
		if v := q.Get(string(d)); v != "" {
			if params.Filters == nil {
				params.Filters = map[string][]string{}
			}
			params.Filters[string(d)] = strings.Split(v, ",")
		}
	}
	detailSince := s.UsageDetailSince()
	query, err := store.ParseUsageQuery(params, detailSince)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	groupNames := make([]string, len(query.GroupBy))
	for i, g := range query.GroupBy {
		groupNames[i] = string(g)
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
	resp := usageReportResponse{GroupBy: groupNames, Bucket: string(query.Bucket), Rows: rows}
	if !query.From.IsZero() {
		resp.From = query.From.Format(time.RFC3339)
	}
	if !query.To.IsZero() {
		resp.To = query.To.Format(time.RFC3339)
	}
	if !detailSince.IsZero() {
		resp.DetailSince = detailSince.Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// usageSpeedResponse is the wire shape of GET /v1/_usage/speed.
type usageSpeedResponse struct {
	From        string             `json:"from,omitempty"`
	To          string             `json:"to,omitempty"`
	DetailSince string             `json:"detail_since,omitempty"`
	Models      []store.UsageSpeed `json:"models"`
}

// handleUsageSpeed serves GET /v1/_usage/speed — how fast each model answered
// in a window (RFC FM D6): over the latest store.UsageTimingsPerModel timed
// calls per model, the median and 95th-percentile call duration, the median
// time to the first token and the median output tokens per second.
//
// It takes the report's from, to, tenant and dimension filters, and is scoped
// like it: a tenant operator's figures come from its own tenant's calls only.
// Timings live in the per-call ledger alone, so the window stops at
// detail_since however far back from reaches.
func (s *Server) handleUsageSpeed(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; usage reporting requires a persistent store")
		return
	}
	q := r.URL.Query()
	params := store.UsageParams{From: q.Get("from"), To: q.Get("to")}
	for _, d := range store.UsageCanonicalDims {
		if d == store.UsageByTenant {
			continue
		}
		if v := q.Get(string(d)); v != "" {
			if params.Filters == nil {
				params.Filters = map[string][]string{}
			}
			params.Filters[string(d)] = strings.Split(v, ",")
		}
	}
	query, err := store.ParseUsageQuery(params, time.Time{})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if tenantID, all := s.principalTenantScope(r.Context(), q.Get("tenant")); !all {
		query.TenantID = tenantID
	}
	rows, err := s.store.UsageCallTimings(r.Context(), query, store.UsageTimingsPerModel)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := usageSpeedResponse{Models: store.SummarizeCallTimings(rows)}
	if !query.From.IsZero() {
		resp.From = query.From.Format(time.RFC3339)
	}
	if !query.To.IsZero() {
		resp.To = query.To.Format(time.RFC3339)
	}
	if ds := s.UsageDetailSince(); !ds.IsZero() {
		resp.DetailSince = ds.Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
