package http

// ontology_job.go — GET /v1/_ontology/job and POST /v1/_ontology/job/resume, and the
// connector methods every transport's job status and resume go through (RFC GQ P5.6).
//
// The job lives in the Document (builtin.OntologyJob, ResumeOntologyJob). What lives here
// is who may read or resume it and in which tenant — once, so the gRPC and MCP twins
// cannot differ from HTTP in either.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// handleOntologyJob serves GET /v1/_ontology/job: the tenant's unfinished job, else the
// one updated last ({"job": null} when it never had one). An admin may focus another
// tenant with ?tenant=.
func (s *Server) handleOntologyJob(w http.ResponseWriter, r *http.Request) {
	res, err := s.GetOntologyJob(r.Context(), connector.GetOntologyJobRequest{Tenant: r.URL.Query().Get("tenant")})
	if err != nil {
		writeOntologyJobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleOntologyJobResume serves POST /v1/_ontology/job/resume. The body is optional:
// {"id": "<the job's id>"} refuses the resume unless that job is the one still
// unfinished.
func (s *Server) handleOntologyJobResume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", `expected {} or {"id": "<the job's id>"}`)
		return
	}
	res, err := s.ResumeOntologyJob(r.Context(), connector.ResumeOntologyJobRequest{Tenant: r.URL.Query().Get("tenant"), ID: body.ID})
	if err != nil {
		writeOntologyJobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeOntologyJobError maps a job refusal onto its HTTP status, with the shared code as
// the error code. Anything else is an internal failure; its text is not returned, since a
// store fault's text can carry a path or a DSN (the connector has logged it).
func writeOntologyJobError(w http.ResponseWriter, err error) {
	code := connector.OntologyJobCode(err)
	switch {
	case errors.Is(err, connector.ErrOntologyJobForbidden):
		writeJSONError(w, http.StatusForbidden, code, err.Error())
	case errors.Is(err, connector.ErrOntologyJobNotFound):
		writeJSONError(w, http.StatusNotFound, code, err.Error())
	case errors.Is(err, connector.ErrOntologyJobBusy):
		writeJSONError(w, http.StatusConflict, code, err.Error())
	case errors.Is(err, connector.ErrOntologyJobUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, code, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "job_failed", "the ontology job could not be read or resumed; see the server log")
	}
}

// ontologyJobTenant is the check every job read and resume makes HERE, not only at the
// route: the job rewrites stored rows in every scope of the tenant, so driving it — and
// reading where it is, which names the tenant's classes and tags — is an admin's or a
// tenant operator's (substrate:tenant, not held to the run plane), never a member's. The
// tenant is the caller's own unless an admin names another.
func (s *Server) ontologyJobTenant(ctx context.Context, tenant string) (string, error) {
	if !subjectOperator(ctx) {
		return "", connector.WithMessage(connector.ErrOntologyJobForbidden,
			"the ontology job is for an admin or a tenant operator")
	}
	if s.store == nil || s.sqlMem == nil {
		return "", connector.WithMessage(connector.ErrOntologyJobUnavailable,
			"ontology jobs need SQL Memory; none is configured")
	}
	return s.adminFocusedTenant(ctx, tenant), nil
}

// logJobFault logs an internal failure (one no sentinel names) before the handler
// answers 500 without its text.
func logJobFault(ctx context.Context, what, tenant string, err error) {
	if err != nil && connector.OntologyJobCode(err) == "" {
		slog.ErrorContext(ctx, "ontology job failed", "op", what, "tenant", tenant, "err", err)
	}
}

// GetOntologyJob implements connector.Connector.
func (s *Server) GetOntologyJob(ctx context.Context, req connector.GetOntologyJobRequest) (connector.OntologyJobResult, error) {
	tenant, err := s.ontologyJobTenant(ctx, req.Tenant)
	if err != nil {
		return connector.OntologyJobResult{}, err
	}
	job, err := s.ontologyDocument().OntologyJob(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}))
	logJobFault(ctx, "status", tenant, err)
	return connector.OntologyJobResult{Tenant: tenant, Job: job}, err
}

// ResumeOntologyJob implements connector.Connector.
func (s *Server) ResumeOntologyJob(ctx context.Context, req connector.ResumeOntologyJobRequest) (connector.OntologyJobResult, error) {
	tenant, err := s.ontologyJobTenant(ctx, req.Tenant)
	if err != nil {
		return connector.OntologyJobResult{}, err
	}
	job, err := s.ontologyDocument().ResumeOntologyJob(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}), req.ID)
	logJobFault(ctx, "resume", tenant, err)
	return connector.OntologyJobResult{Tenant: tenant, Job: job}, err
}
