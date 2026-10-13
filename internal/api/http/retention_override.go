package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
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

// retentionOverrideResponse is one override on the wire; the connector's
// shape, so every transport renders the same record.
type retentionOverrideResponse = connector.RetentionOverride

func retentionOverrideView(r store.RetentionOverrideRow) retentionOverrideResponse {
	out := retentionOverrideResponse{Tenant: r.TenantID, Agent: r.Agent, Until: r.Until.UTC().Format(time.RFC3339), SetBy: r.SetBy}
	if !r.CreatedAt.IsZero() {
		out.CreatedAt = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// retentionOverrideTenant is the tenant a write is for: the caller's own,
// unless the caller is an admin and names another.
//
// An admin that names none gets its own too, and that matters most for the
// plainest deployment: with the one shared token every run is in that token's
// tenant, and an override filed under the empty tenant would be accepted,
// listed, and apply to nothing.
func retentionOverrideTenant(ctx context.Context, wire string) string {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return wire // open mode: runs carry the tenant the caller gives, or none
	}
	if wire != "" && auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return wire
	}
	return p.TenantID
}

// retentionOverrideOperator reports whether the caller may touch overrides
// at all: an admin or a tenant operator, and not a token held to the run
// plane. Each transport gates this too; it is repeated here because this is
// the one function all of them call, and switching storage back on for an
// agent that promised none must not depend on every gate being right.
func retentionOverrideOperator(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true // open mode
	}
	return auth.HasScope(p.Scopes, auth.ScopeTenant) && !p.Restrictions.RunPlaneOnly()
}

// activeRetentionOverrides lists the overrides the caller may see: an admin
// all of them (or one tenant's with a tenant named), a tenant operator its
// own.
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

// RetentionOverride implements connector.Connector: the one implementation
// of list, set and clear, behind HTTP, gRPC and MCP alike.
func (s *Server) RetentionOverride(ctx context.Context, req connector.RetentionOverrideRequest) (connector.RetentionOverrideResult, error) {
	none := connector.RetentionOverrideResult{Overrides: []connector.RetentionOverride{}}
	invalid := func(msg string) (connector.RetentionOverrideResult, error) {
		return none, connector.WithMessage(connector.ErrRetentionOverrideInvalid, msg)
	}
	if !retentionOverrideOperator(ctx) {
		return none, connector.WithMessage(connector.ErrRetentionOverrideForbidden,
			"retention overrides are for an admin or a tenant operator")
	}
	switch req.Op {
	case "list":
		list, err := s.activeRetentionOverrides(ctx, req.Tenant)
		if err != nil {
			return none, err
		}
		return connector.RetentionOverrideResult{Overrides: list}, nil
	case "set", "clear":
	default:
		return invalid(fmt.Sprintf("op must be one of: list, set, clear (got %q)", req.Op))
	}
	if s.store == nil {
		return none, connector.WithMessage(connector.ErrRetentionOverrideStoreUnavailable, "store not configured")
	}
	if err := agents.ValidateName(req.Agent); err != nil {
		return invalid("agent: " + err.Error())
	}
	tenant := retentionOverrideTenant(ctx, req.Tenant)

	if req.Op == "clear" {
		had, err := s.store.RetentionOverrideDelete(ctx, tenant, req.Agent)
		if err != nil {
			return none, err
		}
		if had {
			s.recordRetentionOverrideAudit(ctx, "retention_override_clear", store.RetentionOverrideRow{TenantID: tenant, Agent: req.Agent})
			log.Printf("retention: override cleared by %q: agent %q in tenant %q", principalSubject(ctx), req.Agent, tenant)
		}
		return connector.RetentionOverrideResult{Overrides: []connector.RetentionOverride{}, Cleared: had}, nil
	}

	now := time.Now()
	switch {
	case req.Until.IsZero():
		return invalid("until is required: when the override stops applying (RFC 3339)")
	case !req.Until.After(now):
		return invalid("until is in the past")
	case req.Until.After(now.Add(retentionOverrideMax)):
		return invalid("until is more than 24 hours ahead: an override is for one debugging session")
	}
	row := store.RetentionOverrideRow{
		TenantID: tenant, Agent: req.Agent,
		Until: req.Until.UTC(), SetBy: principalSubject(ctx), CreatedAt: now.UTC(),
	}
	// Audited before it takes effect, and refused if it cannot be: switching
	// storage back on for an agent that promised none is exactly what an
	// audit log is for.
	if !s.recordRetentionOverrideAudit(ctx, "retention_override_set", row) {
		return none, connector.WithMessage(connector.ErrRetentionOverrideAuditUnavailable,
			"the override was not set: its audit record could not be written")
	}
	if err := s.store.RetentionOverridePut(ctx, row, now); err != nil {
		return none, err
	}
	log.Printf("retention: override set by %q: runs of agent %q in tenant %q are stored until %s", row.SetBy, row.Agent, row.TenantID, row.Until.Format(time.RFC3339))
	return connector.RetentionOverrideResult{Overrides: []connector.RetentionOverride{retentionOverrideView(row)}}, nil
}

// writeRetentionOverrideError is the HTTP form of a RetentionOverride error.
func writeRetentionOverrideError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connector.ErrRetentionOverrideInvalid):
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, connector.ErrRetentionOverrideForbidden):
		writeJSONError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, connector.ErrRetentionOverrideAuditUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable", err.Error())
	case errors.Is(err, connector.ErrRetentionOverrideStoreUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// handleRetentionOverridesList serves GET /v1/_retention/overrides.
func (s *Server) handleRetentionOverridesList(w http.ResponseWriter, r *http.Request) {
	res, err := s.RetentionOverride(r.Context(), connector.RetentionOverrideRequest{Op: "list", Tenant: r.URL.Query().Get("tenant")})
	if err != nil {
		writeRetentionOverrideError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": res.Overrides})
}

// handleRetentionOverridePut serves PUT /v1/_retention/overrides.
func (s *Server) handleRetentionOverridePut(w http.ResponseWriter, r *http.Request) {
	var body retentionOverridePutRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	res, err := s.RetentionOverride(r.Context(), connector.RetentionOverrideRequest{
		Op: "set", Tenant: deref(body.Tenant), Agent: body.Agent, Until: body.Until,
	})
	if err != nil {
		writeRetentionOverrideError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res.Overrides[0])
}

// handleRetentionOverrideDelete serves DELETE /v1/_retention/overrides?agent=&tenant=.
func (s *Server) handleRetentionOverrideDelete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := s.RetentionOverride(r.Context(), connector.RetentionOverrideRequest{Op: "clear", Tenant: q.Get("tenant"), Agent: q.Get("agent")}); err != nil {
		writeRetentionOverrideError(w, err)
		return
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
