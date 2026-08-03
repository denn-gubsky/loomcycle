package http

// directory.go — the HTTP face of the derived directory.
//
//	GET /v1/_users/{subject}   one subject's aggregate view (ScopeTenant)
//	GET /v1/_tenants           tenants with derived counts (ADMIN ONLY)
//
// There is no create/update/delete here, deliberately. A "user" in loomcycle is
// derived from runs.user_id — there is no row to write. The delete half of "user
// CRUD" is the subject-erasure surface, which is the only thing that removes a
// person's footprint coherently across four planes.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/directory"
)

func (s *Server) directoryService() *directory.Service {
	return &directory.Service{Store: s.store, SqlMem: s.sqlMem}
}

// handleInspectUser serves GET /v1/_users/{subject}.
//
// Gated ScopeTenant like the list it drills into: everything it aggregates is
// already readable by this principal through /v1/_usage, /v1/_limits and the
// memory surfaces. It saves five calls; it grants no new reach.
func (s *Server) handleInspectUser(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; the directory requires a persistent store")
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	// Same refusal as the erasure surface, for the same reason: a subject id is
	// only unique within a tenant, so an admin who names none would be answered
	// about the DEFAULT tenant's `alice` rather than told the question is ambiguous.
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: a subject id is only unique within one. "+
				"Pass ?tenant=<id>, or ?tenant= for the default tenant.")
		return
	}
	ins, err := s.directoryService().Inspect(r.Context(), tenant, subject)
	if errors.Is(err, directory.ErrNoSubject) {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "inspect_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ins)
}

// handleListTenants serves GET /v1/_tenants.
//
// ADMIN ONLY, and the check is here rather than left to the route table because
// the reason is easy to lose: the counts are unremarkable, but THE LIST ITSELF is
// the disclosure. Which tenants exist is precisely what a tenant-confined
// principal must not learn — which is why every other cross-tenant read in this
// codebase answers with an opaque not-found rather than a filtered list.
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; tenant listing requires a persistent store")
		return
	}
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		writeJSONError(w, http.StatusForbidden, "admin_required",
			"listing tenants requires an operator-admin token: the list of tenants is itself "+
				"cross-tenant information")
		return
	}
	rows, err := s.directoryService().Tenants(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_tenants_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenants": rows,
		"notes": []string{
			"derived from runs, so a tenant that has never started a run does not appear — " +
				"an empty list means no ACTIVITY, not no tenants.",
		},
	})
}

// DirectoryUsers implements connector.Connector.
func (s *Server) DirectoryUsers(ctx context.Context, tenant string) ([]directory.UserRow, error) {
	if s.store == nil {
		return nil, errors.New("no store configured")
	}
	return s.directoryService().Users(ctx, tenant)
}

// DirectoryInspect implements connector.Connector.
func (s *Server) DirectoryInspect(ctx context.Context, tenant, subject string) (directory.Inspection, error) {
	if s.store == nil {
		return directory.Inspection{}, errors.New("no store configured")
	}
	return s.directoryService().Inspect(ctx, tenant, subject)
}

// DirectoryTenants implements connector.Connector. The ADMIN check lives at each
// transport, not here — this is the raw read.
func (s *Server) DirectoryTenants(ctx context.Context) ([]directory.TenantRow, error) {
	if s.store == nil {
		return nil, errors.New("no store configured")
	}
	return s.directoryService().Tenants(ctx)
}
