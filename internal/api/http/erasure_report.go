package http

// erasure_report.go — the HTTP face of GET /v1/_erasure.
//
// The logic lives in internal/erasure so MCP, gRPC and the adapters run the SAME
// code. What stays here is the one thing a transport genuinely owns: deciding
// WHICH tenant this caller may act in. That is an auth decision and cannot be
// pushed down — the service takes an already-resolved tenant and never guesses.

import (
	"context"
	"errors"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
)

// erasureService builds the shared service from this server's stores.
func (s *Server) erasureService() *erasure.Service {
	return &erasure.Service{Store: s.store, SqlMem: s.sqlMem}
}

// resolveErasureTenant applies the tenant rule both erasure routes share.
//
// An admin with no ?tenant= means "all tenants" everywhere else in this API, and
// here that cannot be served: a subject id is only meaningful WITHIN a tenant —
// the same string in two tenants is two different people. Refusing rather than
// defaulting, because the default is the trap: `all` carries tenant "", which
// every store read would interpret as the DEFAULT tenant and answer confidently
// about the wrong people. On the write path it would DELETE them.
//
// Has() rather than != "": it distinguishes an explicit `?tenant=` (the default
// tenant, i.e. the whole deployment on a single-tenant install) from an omitted
// one. Without that, a single-tenant admin could not ask at all.
func (s *Server) resolveErasureTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: a subject id is only unique within one. "+
				"Pass ?tenant=<id>, or ?tenant= for the default tenant.")
		return "", false
	}
	return tenant, true
}

// handleErasureReport serves GET /v1/_erasure?subject=…
//
// Gated ScopeTenant: answering a data-subject request is the tenant operator's
// job, and it aggregates a view this principal can already reach rather than
// granting new reach.
func (s *Server) handleErasureReport(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_store", "no store configured")
		return
	}
	tenant, ok := s.resolveErasureTenant(w, r)
	if !ok {
		return
	}
	rep, err := s.erasureService().Report(r.Context(), tenant, r.URL.Query().Get("subject"))
	if errors.Is(err, erasure.ErrNoSubject) {
		writeJSONError(w, http.StatusBadRequest, "missing_subject",
			"pass ?subject=<user id> — the principal whose footprint to report")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "erasure_report_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ErasureReport implements connector.Connector, so MCP and gRPC run the same
// code the HTTP route does. The tenant arrives resolved — the transport that
// called in owns that decision.
func (s *Server) ErasureReport(ctx context.Context, tenant, subject string) (erasure.Report, error) {
	if s.store == nil {
		return erasure.Report{}, errors.New("no store configured")
	}
	return s.erasureService().Report(ctx, tenant, subject)
}

// ErasureExecute implements connector.Connector.
func (s *Server) ErasureExecute(ctx context.Context, req erasure.ExecuteRequest) (erasure.Result, error) {
	if s.store == nil {
		return erasure.Result{}, errors.New("no store configured")
	}
	return s.erasureService().Execute(ctx, req)
}
