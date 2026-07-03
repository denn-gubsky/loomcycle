package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC AW — the /v1/_limits management surface for per-scope token budgets.
//
// Tenant scoping mirrors /v1/_usage (RFC AS): admin/legacy/open see every row
// and may focus one tenant via ?tenant=; a substrate:tenant operator sees +
// writes ONLY its own tenant_id rows (its tenant + its users). The
// operator-global cap (tenant_id='', scope='operator') and cross-tenant rows
// are admin-only. The write path stamps the tenant from the principal for a
// scoped caller — the wire tenant_id is never trusted for confinement.

// limitScopes is the closed set of valid scope axes.
var limitScopes = map[string]bool{"operator": true, "tenant": true, "user": true}

// limitRowResponse is one token_limits row plus its live month-to-date usage.
type limitRowResponse struct {
	TenantID  string `json:"tenant_id"`
	Scope     string `json:"scope"`
	ScopeID   string `json:"scope_id,omitempty"`
	SoftLimit *int64 `json:"soft_limit,omitempty"`
	HardLimit *int64 `json:"hard_limit,omitempty"`
	Used      int64  `json:"used"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

type limitsListResponse struct {
	Limits []limitRowResponse `json:"limits"`
}

// handleLimitsList serves GET /v1/_limits — the token budgets visible to the
// caller, each with its current month-to-date usage from the tracker.
func (s *Server) handleLimitsList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; token budgets require a persistent store")
		return
	}
	rows, err := s.store.TokenLimitsAll(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	out := make([]limitRowResponse, 0, len(rows))
	for _, row := range rows {
		if !all && row.TenantID != tenantID {
			continue // a tenant operator sees only its own tenant's rows
		}
		resp := limitRowResponse{
			TenantID:  row.TenantID,
			Scope:     row.Scope,
			ScopeID:   row.ScopeID,
			SoftLimit: row.SoftLimit,
			HardLimit: row.HardLimit,
			Used:      s.limits.UsedFor(row.Scope, row.TenantID, row.ScopeID),
			UpdatedBy: row.UpdatedBy,
		}
		if !row.UpdatedAt.IsZero() {
			resp.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, resp)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(limitsListResponse{Limits: out})
}

// limitPutRequest is the PUT /v1/_limits body. tenant_id is a pointer so an
// admin can address any tenant; it is IGNORED for confinement on a scoped
// caller (stamped from the principal).
type limitPutRequest struct {
	TenantID  *string `json:"tenant_id"`
	Scope     string  `json:"scope"`
	ScopeID   string  `json:"scope_id"`
	SoftLimit *int64  `json:"soft_limit"`
	HardLimit *int64  `json:"hard_limit"`
}

// handleLimitPut serves PUT /v1/_limits — upsert one budget row. Tenant-scoped:
// a substrate:tenant caller may write only its own tenant's tenant/user rows;
// the operator scope + any cross-tenant write is admin-only (403).
func (s *Server) handleLimitPut(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; token budgets require a persistent store")
		return
	}
	var body limitPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if !limitScopes[body.Scope] {
		writeJSONError(w, http.StatusBadRequest, "bad_request",
			"scope must be one of: operator, tenant, user")
		return
	}
	if (body.SoftLimit != nil && *body.SoftLimit < 0) || (body.HardLimit != nil && *body.HardLimit < 0) {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "soft_limit/hard_limit must be >= 0")
		return
	}

	tenantID, scopeID, ok := s.resolveLimitWrite(w, r, body.Scope, deref(body.TenantID), body.ScopeID)
	if !ok {
		return
	}

	row := store.TokenLimitRow{
		TenantID:  tenantID,
		Scope:     body.Scope,
		ScopeID:   scopeID,
		SoftLimit: body.SoftLimit,
		HardLimit: body.HardLimit,
		UpdatedAt: time.Now().UTC(),
		UpdatedBy: principalSubject(r.Context()),
	}
	if err := s.store.TokenLimitPut(r.Context(), row); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.limits.ReloadLimits(r.Context()); err != nil {
		// The row is persisted; a reload fault just means the cached ceiling
		// lags until the next reload/seed. Log-and-continue (advisory).
		writeJSONError(w, http.StatusInternalServerError, "internal", "limit stored but reload failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(limitRowResponse{
		TenantID:  row.TenantID,
		Scope:     row.Scope,
		ScopeID:   row.ScopeID,
		SoftLimit: row.SoftLimit,
		HardLimit: row.HardLimit,
		Used:      s.limits.UsedFor(row.Scope, row.TenantID, row.ScopeID),
		UpdatedAt: row.UpdatedAt.Format(time.RFC3339),
		UpdatedBy: row.UpdatedBy,
	})
}

// handleLimitDelete serves DELETE /v1/_limits?scope=&scope_id=&tenant= — remove
// a budget (→ unlimited again). Same tenant-scope gate as the PUT.
func (s *Server) handleLimitDelete(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; token budgets require a persistent store")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	if !limitScopes[scope] {
		writeJSONError(w, http.StatusBadRequest, "bad_request",
			"scope must be one of: operator, tenant, user")
		return
	}
	tenantID, scopeID, ok := s.resolveLimitWrite(w, r, scope, q.Get("tenant"), q.Get("scope_id"))
	if !ok {
		return
	}
	if err := s.store.TokenLimitDelete(r.Context(), tenantID, scope, scopeID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.limits.ReloadLimits(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "limit deleted but reload failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveLimitWrite validates + resolves the authoritative (tenant_id, scope_id)
// for a write, enforcing the RFC AW tenant confinement:
//   - operator scope: admin-only; tenant_id and scope_id forced to "".
//   - tenant scope: scope_id must be "" (the whole tenant); a scoped caller is
//     confined to its own tenant, an admin may target any via wireTenant.
//   - user scope: scope_id (the subject) required.
//
// Returns ok=false and writes the error response when the caller is not
// entitled or the shape is invalid.
func (s *Server) resolveLimitWrite(w http.ResponseWriter, r *http.Request, scope, wireTenant, scopeID string) (tenantID, resolvedScopeID string, ok bool) {
	// all == admin / legacy / open mode → full authority (may write any tenant
	// + the operator scope). A scoped substrate:tenant caller has all=false.
	callerTenant, all := tenantScopeFromCtx(r.Context())

	switch scope {
	case "operator":
		if !all {
			writeJSONError(w, http.StatusForbidden, "forbidden",
				"the operator-global budget is admin-only")
			return "", "", false
		}
		// operator scope is a single global row; ignore any tenant/scope id.
		return "", "", true
	case "tenant":
		if scopeID != "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"scope_id must be empty for scope=tenant")
			return "", "", false
		}
	case "user":
		if scopeID == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"scope_id (the user subject) is required for scope=user")
			return "", "", false
		}
	}

	// tenant / user scope: resolve the authoritative tenant.
	if all {
		// Admin may address any tenant; the wire tenant_id is authoritative here.
		return wireTenant, scopeID, true
	}
	// Scoped caller: confined to its own tenant. A wire tenant_id that disagrees
	// is a cross-tenant attempt → 403 (never silently rewritten to a foreign id).
	if wireTenant != "" && wireTenant != callerTenant {
		writeJSONError(w, http.StatusForbidden, "forbidden",
			"a tenant operator may only manage budgets in its own tenant")
		return "", "", false
	}
	return callerTenant, scopeID, true
}

// principalSubject returns the ctx principal's subject for the updated_by audit
// column (empty for open/legacy mode).
func principalSubject(ctx context.Context) string {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return p.Subject
	}
	return ""
}

// deref returns the pointed-to string, or "" when the pointer is nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
