package http

// ontology_subject_merge.go — POST /v1/_ontology/subjects/merge, and the shared
// connector method every transport's merge goes through (RFC FR P2).
//
// The merge itself lives in the Document (builtin.MergeSubjects): one SQL
// transaction, then the `merged` event. What lives here is who may ask for one
// and in which tenant — once, so gRPC and MCP, which reach the connector without
// the HTTP route gate, cannot differ from HTTP in either.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// handleOntologySubjectMerge serves POST /v1/_ontology/subjects/merge.
//
// The body is {duplicate, survivor}; an admin may focus another tenant with
// ?tenant=, as on the other ontology routes.
func (s *Server) handleOntologySubjectMerge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Duplicate string `json:"duplicate"`
		Survivor  string `json:"survivor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"expected {\"duplicate\": \"…\", \"survivor\": \"…\"}, each a subject's document id or natural key")
		return
	}
	res, err := s.MergeSubjects(r.Context(), connector.MergeSubjectsRequest{
		Tenant: r.URL.Query().Get("tenant"), Duplicate: body.Duplicate, Survivor: body.Survivor,
	})
	if err != nil {
		writeSubjectMergeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeSubjectMergeError maps a merge refusal onto its HTTP status, with the
// shared code as the error code.
func writeSubjectMergeError(w http.ResponseWriter, err error) {
	code := connector.SubjectMergeCode(err)
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, connector.ErrSubjectMergeInvalid), errors.Is(err, connector.ErrSubjectMergeSame),
		errors.Is(err, connector.ErrSubjectMergeNotSubject):
		status = http.StatusBadRequest
	case errors.Is(err, connector.ErrSubjectMergeForbidden):
		status = http.StatusForbidden
	case errors.Is(err, connector.ErrSubjectMergeNotInScope):
		status = http.StatusNotFound
	case errors.Is(err, connector.ErrSubjectMergeAlreadyMerged), errors.Is(err, connector.ErrSubjectMergeConflict),
		errors.Is(err, connector.ErrSubjectMergePrincipalConflict):
		status = http.StatusConflict
	case errors.Is(err, connector.ErrSubjectMergeUnavailable):
		status = http.StatusServiceUnavailable
	default:
		code = "merge_failed"
	}
	writeJSONError(w, status, code, err.Error())
}

// MergeSubjects implements connector.Connector: the one path every transport's
// merge takes.
//
// The caller is checked HERE, not only at the HTTP route: gRPC and MCP reach
// this method past their own gates, and a merge is an operator decision — an
// admin, or a tenant operator in its own tenant, never a token held to the run
// plane. The tenant is the caller's own unless an admin names another.
func (s *Server) MergeSubjects(ctx context.Context, req connector.MergeSubjectsRequest) (connector.MergeSubjectsResult, error) {
	if !subjectOperator(ctx) {
		return connector.MergeSubjectsResult{}, connector.WithMessage(connector.ErrSubjectMergeForbidden,
			"merging subjects is for an admin or a tenant operator")
	}
	if s.store == nil || s.sqlMem == nil {
		return connector.MergeSubjectsResult{}, connector.WithMessage(connector.ErrSubjectMergeUnavailable,
			"merging subjects needs SQL Memory; none is configured")
	}
	tenant := s.adminFocusedTenant(ctx, req.Tenant)
	mi := memInject{Tenant: tenant}
	res, err := s.ontologyDocument().MergeSubjects(s.ontologyDocCtx(ctx, mi), "tenant", req.Duplicate, req.Survivor)
	res.Tenant = tenant
	return res, err
}

// subjectOperator reports whether the caller may make an operator's decision
// about a tenant subject (merge two, bind one to a user): an admin or a tenant
// operator (substrate:tenant, which an admin's scope implies), not a token held
// to the run plane. Open mode has no principal and is an operator.
func subjectOperator(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true
	}
	return auth.HasScope(p.Scopes, auth.ScopeTenant) && !p.Restrictions.RunPlaneOnly()
}
