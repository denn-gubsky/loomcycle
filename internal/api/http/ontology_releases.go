package http

// ontology_releases.go — GET /v1/_ontology/releases, /releases/{seq} and
// /releases/diff, and the connector methods every transport's release read goes
// through (RFC GQ P3.4).
//
// The reads live in the Document (builtin.ListOntologyReleases and its siblings).
// What lives here is who may read and in which tenant — once, so the gRPC and MCP
// twins (P3.5) cannot differ from HTTP in either.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// handleOntologyReleases serves GET /v1/_ontology/releases: every complete release
// kept, newest first. An admin may focus another tenant with ?tenant=.
func (s *Server) handleOntologyReleases(w http.ResponseWriter, r *http.Request) {
	res, err := s.ListOntologyReleases(r.Context(), connector.ListOntologyReleasesRequest{Tenant: r.URL.Query().Get("tenant")})
	if err != nil {
		writeOntologyReleaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleOntologyRelease serves GET /v1/_ontology/releases/{seq}.
func (s *Server) handleOntologyRelease(w http.ResponseWriter, r *http.Request) {
	seq, ok := releaseSeq(w, r.PathValue("seq"), "seq")
	if !ok {
		return
	}
	res, err := s.GetOntologyRelease(r.Context(), connector.GetOntologyReleaseRequest{Tenant: r.URL.Query().Get("tenant"), Seq: seq})
	if err != nil {
		writeOntologyReleaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleOntologyReleaseDiff serves GET /v1/_ontology/releases/diff?from=<seq>&to=<seq|current>.
// to defaults to current, the ontology as it stands.
func (s *Server) handleOntologyReleaseDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, ok := releaseSeq(w, q.Get("from"), "from")
	if !ok {
		return
	}
	var to int64
	if v := q.Get("to"); v != "" && v != "current" {
		if to, ok = releaseSeq(w, v, "to"); !ok {
			return
		}
	}
	res, err := s.DiffOntologyReleases(r.Context(), connector.DiffOntologyReleasesRequest{Tenant: q.Get("tenant"), From: from, To: to})
	if err != nil {
		writeOntologyReleaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// releaseSeq parses a release's sequence number, answering 400 for anything that is
// not a positive integer.
func releaseSeq(w http.ResponseWriter, v, name string) (int64, bool) {
	seq, err := strconv.ParseInt(v, 10, 64)
	if err != nil || seq <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", name+" must be a release's sequence number, a positive integer")
		return 0, false
	}
	return seq, true
}

// writeOntologyReleaseError maps a release-read refusal onto its HTTP status, with
// the shared code as the error code. Anything else is an internal failure; its text
// is not returned, since a store fault's text can carry a path or a DSN.
func writeOntologyReleaseError(w http.ResponseWriter, err error) {
	code := connector.OntologyReleaseCode(err)
	switch {
	case errors.Is(err, connector.ErrOntologyReleaseInvalid):
		writeJSONError(w, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, connector.ErrOntologyReleaseForbidden):
		writeJSONError(w, http.StatusForbidden, code, err.Error())
	case errors.Is(err, connector.ErrOntologyReleaseNotFound):
		writeJSONError(w, http.StatusNotFound, code, err.Error())
	case errors.Is(err, connector.ErrOntologyReleaseUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, code, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "release_read_failed", "the ontology releases could not be read; see the server log")
	}
}

// ontologyReleaseTenant is the check every release read makes HERE, not only at the
// route: a release keeps the ontology's text after an edit or an erasure removed it,
// so it is an admin's or a tenant operator's (substrate:tenant, not held to the run
// plane), never a member's. The tenant is the caller's own unless an admin names
// another.
func (s *Server) ontologyReleaseTenant(ctx context.Context, tenant string) (string, error) {
	if !subjectOperator(ctx) {
		return "", connector.WithMessage(connector.ErrOntologyReleaseForbidden,
			"reading ontology releases is for an admin or a tenant operator")
	}
	if s.store == nil || s.sqlMem == nil {
		return "", connector.WithMessage(connector.ErrOntologyReleaseUnavailable,
			"ontology releases need SQL Memory; none is configured")
	}
	return s.adminFocusedTenant(ctx, tenant), nil
}

// logReleaseFault logs an internal failure (one no sentinel names) before the
// handler answers 500 without its text.
func logReleaseFault(ctx context.Context, what, tenant string, err error) {
	if err != nil && connector.OntologyReleaseCode(err) == "" {
		slog.ErrorContext(ctx, "ontology release read failed", "op", what, "tenant", tenant, "err", err)
	}
}

// ListOntologyReleases implements connector.Connector.
func (s *Server) ListOntologyReleases(ctx context.Context, req connector.ListOntologyReleasesRequest) (connector.ListOntologyReleasesResult, error) {
	out := connector.ListOntologyReleasesResult{Releases: []connector.OntologyRelease{}}
	tenant, err := s.ontologyReleaseTenant(ctx, req.Tenant)
	if err != nil {
		return out, err
	}
	out.Tenant = tenant
	rels, err := s.ontologyDocument().ListOntologyReleases(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}))
	logReleaseFault(ctx, "list", tenant, err)
	if err != nil {
		return out, err
	}
	out.Releases = rels
	return out, nil
}

// GetOntologyRelease implements connector.Connector.
func (s *Server) GetOntologyRelease(ctx context.Context, req connector.GetOntologyReleaseRequest) (connector.GetOntologyReleaseResult, error) {
	tenant, err := s.ontologyReleaseTenant(ctx, req.Tenant)
	if err != nil {
		return connector.GetOntologyReleaseResult{}, err
	}
	res, err := s.ontologyDocument().GetOntologyRelease(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}), req.Seq)
	logReleaseFault(ctx, "get", tenant, err)
	res.Tenant = tenant
	return res, err
}

// DiffOntologyReleases implements connector.Connector.
func (s *Server) DiffOntologyReleases(ctx context.Context, req connector.DiffOntologyReleasesRequest) (connector.DiffOntologyReleasesResult, error) {
	tenant, err := s.ontologyReleaseTenant(ctx, req.Tenant)
	if err != nil {
		return connector.DiffOntologyReleasesResult{Changes: []connector.OntologyEntryDiff{}, Issues: []string{}}, err
	}
	res, err := s.ontologyDocument().DiffOntologyReleases(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}), req.From, req.To)
	logReleaseFault(ctx, "diff", tenant, err)
	res.Tenant = tenant
	return res, err
}
