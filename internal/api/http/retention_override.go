package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Break-glass for no-trace agents.
//
// An agent whose runs store no content cannot be debugged from its
// transcript. An operator may lift the mode for one agent, in one tenant, for
// a limited time: PUT /v1/_retention/overrides {agent, until}. Runs of that
// agent started inside the window are ordinary stored runs, and are removed
// afterwards with a run-tree delete.
//
// What it lifts is a mode that came from the agent's definition or from the
// run request. It NEVER lifts one that came from the caller's token: a
// developer's own runs stay unstored whatever an operator switches on. Nor
// does it lift the mode of a child of a no-trace parent — lift the parent.
//
// Who may set one: an admin for any tenant, a tenant operator for its own. A
// tenant member may not (memberCarveOut), and neither may a token limited to
// certain agents (it is held to the run endpoints).

// retentionOverrideMax is the longest an override may run. It is a tool for
// one debugging session, not a setting.
const retentionOverrideMax = 24 * time.Hour

type retentionOverridePutRequest struct {
	// Tenant is whose runs the override applies to. An admin names it; for a
	// tenant operator it is always the operator's own.
	Tenant *string `json:"tenant,omitempty"`
	Agent  string  `json:"agent"`
	// Until is when the override stops applying (RFC 3339), at most 24 hours
	// from now.
	Until time.Time `json:"until"`
}

type retentionOverrideResponse struct {
	Tenant    string `json:"tenant"`
	Agent     string `json:"agent"`
	Until     string `json:"until"`
	SetBy     string `json:"set_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func retentionOverrideView(r store.RetentionOverrideRow) retentionOverrideResponse {
	out := retentionOverrideResponse{Tenant: r.TenantID, Agent: r.Agent, Until: r.Until.UTC().Format(time.RFC3339), SetBy: r.SetBy}
	if !r.CreatedAt.IsZero() {
		out.CreatedAt = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// retentionOverrideTenant is the tenant a write is for: the caller's own,
// unless the caller is an admin, who names it.
func retentionOverrideTenant(ctx context.Context, wire string) string {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return wire
	}
	return p.TenantID
}

// activeRetentionOverrides lists the overrides the caller may see: an admin
// all of them (or one tenant's with ?tenant=), a tenant operator its own.
func (s *Server) activeRetentionOverrides(ctx context.Context, wireTenant string) ([]retentionOverrideResponse, error) {
	out := []retentionOverrideResponse{}
	if s.store == nil {
		return out, nil
	}
	rows, err := s.store.RetentionOverridesActive(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	tenantID, all := s.principalTenantScope(ctx, wireTenant)
	for _, r := range rows {
		if all || r.TenantID == tenantID {
			out = append(out, retentionOverrideView(r))
		}
	}
	return out, nil
}

// handleRetentionOverridesList serves GET /v1/_retention/overrides.
func (s *Server) handleRetentionOverridesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.activeRetentionOverrides(r.Context(), r.URL.Query().Get("tenant"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": list})
}

// handleRetentionOverridePut serves PUT /v1/_retention/overrides.
func (s *Server) handleRetentionOverridePut(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "store not configured")
		return
	}
	var body retentionOverridePutRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if err := agents.ValidateName(body.Agent); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "agent: "+err.Error())
		return
	}
	now := time.Now()
	switch {
	case body.Until.IsZero():
		writeJSONError(w, http.StatusBadRequest, "bad_request", "until is required: when the override stops applying (RFC 3339)")
		return
	case !body.Until.After(now):
		writeJSONError(w, http.StatusBadRequest, "bad_request", "until is in the past")
		return
	case body.Until.After(now.Add(retentionOverrideMax)):
		writeJSONError(w, http.StatusBadRequest, "bad_request", "until is more than 24 hours ahead: an override is for one debugging session")
		return
	}
	row := store.RetentionOverrideRow{
		TenantID: retentionOverrideTenant(r.Context(), deref(body.Tenant)), Agent: body.Agent,
		Until: body.Until.UTC(), SetBy: principalSubject(r.Context()), CreatedAt: now.UTC(),
	}
	// Audited before it takes effect, and refused if it cannot be: switching
	// storage back on for an agent that promised none is exactly what an
	// audit log is for.
	if !s.recordRetentionOverrideAudit(r.Context(), "retention_override_set", row) {
		writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable", "the override was not set: its audit record could not be written")
		return
	}
	if err := s.store.RetentionOverridePut(r.Context(), row, now); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	log.Printf("retention: override set by %q: runs of agent %q in tenant %q are stored until %s", row.SetBy, row.Agent, row.TenantID, row.Until.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, retentionOverrideView(row))
}

// handleRetentionOverrideDelete serves DELETE /v1/_retention/overrides?agent=&tenant=.
func (s *Server) handleRetentionOverrideDelete(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "store not configured")
		return
	}
	q := r.URL.Query()
	agent := q.Get("agent")
	if err := agents.ValidateName(agent); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "agent: "+err.Error())
		return
	}
	tenant := retentionOverrideTenant(r.Context(), q.Get("tenant"))
	had, err := s.store.RetentionOverrideDelete(r.Context(), tenant, agent)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if had {
		s.recordRetentionOverrideAudit(r.Context(), "retention_override_clear", store.RetentionOverrideRow{TenantID: tenant, Agent: agent})
		log.Printf("retention: override cleared by %q: agent %q in tenant %q", principalSubject(r.Context()), agent, tenant)
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordRetentionOverrideAudit writes the audit record for an override being
// set or cleared, and reports whether it was written. With no audit log
// configured there is nothing to write, and that is not a failure: the
// process log line is then the record.
func (s *Server) recordRetentionOverrideAudit(ctx context.Context, action string, row store.RetentionOverrideRow) bool {
	if s.tokenAudit == nil {
		return true
	}
	ev := audit.Event{
		Action: action, TargetTenant: row.TenantID, TargetName: row.Agent,
		ActorTenant: tenantFromCtx(ctx), ActorSubject: principalSubject(ctx),
	}
	if !row.Until.IsZero() {
		ev.RetentionUntil = row.Until.UTC().Format(time.RFC3339)
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		log.Printf("retention: override audit write failed: %v", err)
		return false
	}
	return true
}

// retentionLifted reports whether an operator's override is in force for runs
// of agent in tenant. A store that cannot be read means no: the mode the
// agent or the request asked for stands.
func (s *Server) retentionLifted(ctx context.Context, tenant, agent string) bool {
	if s.store == nil || agent == "" {
		return false
	}
	ok, err := s.store.RetentionOverrideActive(context.WithoutCancel(ctx), tenant, agent, time.Now())
	if err != nil {
		log.Printf("retention: could not read the override for agent %q in tenant %q (%v); the run stays no-trace", agent, tenant, err)
		return false
	}
	return ok
}
