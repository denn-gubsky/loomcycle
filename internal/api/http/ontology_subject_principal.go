package http

// ontology_subject_principal.go — POST /v1/_ontology/subjects/principal, and the
// shared connector method every transport's subject binding goes through
// (RFC FR P4).
//
// The binding itself lives in the Document (builtin.BindTenantSubjectPrincipal):
// one user per subject, one subject per user, under the scope's binding lease.
// What lives here is who may ask, in which tenant, and which identities count as a
// user of that tenant — the Document does not hold the user store, so it leaves
// that check to the transport, and it is made once here so gRPC and MCP cannot
// differ from HTTP.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// handleOntologySubjectPrincipal serves POST /v1/_ontology/subjects/principal.
//
// The body is {subject, principal}; principal "" clears the binding. An admin may
// focus another tenant with ?tenant=, as on the other ontology routes.
func (s *Server) handleOntologySubjectPrincipal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Subject   string `json:"subject"`
		Principal string `json:"principal"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"expected {\"subject\": \"…\", \"principal\": \"…\"}: the subject's document id or natural key, and a user's subject id (\"\" clears)")
		return
	}
	res, err := s.BindSubjectPrincipal(r.Context(), connector.BindSubjectPrincipalRequest{
		Tenant: r.URL.Query().Get("tenant"), Subject: body.Subject, Principal: body.Principal,
	})
	if err != nil {
		writeSubjectBindError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeSubjectBindError maps a binding refusal onto its HTTP status, with the
// shared code as the error code.
func writeSubjectBindError(w http.ResponseWriter, err error) {
	code := connector.SubjectBindCode(err)
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, connector.ErrSubjectBindInvalid), errors.Is(err, connector.ErrSubjectBindNotSubject):
		status = http.StatusBadRequest
	case errors.Is(err, connector.ErrSubjectBindForbidden):
		status = http.StatusForbidden
	case errors.Is(err, connector.ErrSubjectBindNotFound), errors.Is(err, connector.ErrSubjectBindPrincipalNotFound):
		status = http.StatusNotFound
	case errors.Is(err, connector.ErrSubjectBindConflict):
		status = http.StatusConflict
	case errors.Is(err, connector.ErrSubjectBindUnavailable):
		status = http.StatusServiceUnavailable
	default:
		code = "bind_failed"
	}
	writeJSONError(w, status, code, err.Error())
}

// BindSubjectPrincipal implements connector.Connector: the one path every
// transport's subject binding takes.
//
// The caller is checked HERE, as for a merge: gRPC and MCP reach this method past
// their own gates, and binding a subject to a person decides what an erasure of
// that person finds. The tenant is the caller's own unless an admin names another.
func (s *Server) BindSubjectPrincipal(ctx context.Context, req connector.BindSubjectPrincipalRequest) (connector.BindSubjectPrincipalResult, error) {
	var out connector.BindSubjectPrincipalResult
	if !subjectOperator(ctx) {
		return out, connector.WithMessage(connector.ErrSubjectBindForbidden,
			"binding a subject to a user is for an admin or a tenant operator")
	}
	subject, principal := strings.TrimSpace(req.Subject), strings.TrimSpace(req.Principal)
	if subject == "" {
		return out, connector.WithMessage(connector.ErrSubjectBindInvalid,
			"subject is required: the subject's document id, root chunk id or natural key")
	}
	if s.store == nil || s.sqlMem == nil {
		return out, connector.WithMessage(connector.ErrSubjectBindUnavailable,
			"binding a subject needs SQL Memory and a store; none is configured")
	}
	tenant := s.adminFocusedTenant(ctx, req.Tenant)
	out.Tenant = tenant
	if principal != "" {
		// A user of the tenant is a row in its users table, whatever its status — the
		// rule the user-admin routes use (GET /v1/_users/{subject}/identities, the
		// gate's login management). A pending user is a person awaiting approval and
		// a disabled one a person on the way to erasure, which is exactly when the
		// binding is read. A config-declared principal with no row, or a subject that
		// only ever ran, is not a registered user and cannot be bound.
		if _, err := s.store.UserGet(ctx, tenant, principal); err != nil {
			if isNotFound(err) {
				return out, connector.WithMessage(connector.ErrSubjectBindPrincipalNotFound,
					fmt.Sprintf("no user %q is registered in this tenant: a subject is bound to one of the tenant's users", principal))
			}
			return out, fmt.Errorf("bind_subject_principal: reading the user: %w", err)
		}
	}
	b, err := s.ontologyDocument().BindTenantSubjectPrincipal(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}), subject, principal)
	if err != nil {
		return out, subjectBindError(err)
	}
	out.DocumentID, out.RootChunkID, out.NaturalKey, out.Path = b.DocumentID, b.RootChunkID, b.NaturalKey, b.Path
	out.Principal, out.Previous, out.Changed = b.Principal, b.Previous, b.Changed
	return out, nil
}

// subjectBindError classifies the Document's binding errors onto the shared
// sentinels; one it does not know is returned as it is (an internal failure).
func subjectBindError(err error) error {
	var conflict *builtin.SubjectBindingConflictError
	switch {
	case connector.SubjectBindCode(err) != "":
		return err
	case errors.Is(err, builtin.ErrSubjectNotFound):
		return connector.WithMessage(connector.ErrSubjectBindNotFound, err.Error())
	case errors.Is(err, builtin.ErrNotASubject):
		return connector.WithMessage(connector.ErrSubjectBindNotSubject, err.Error())
	case errors.As(err, &conflict):
		return connector.WithMessage(connector.ErrSubjectBindConflict, err.Error())
	}
	return err
}
