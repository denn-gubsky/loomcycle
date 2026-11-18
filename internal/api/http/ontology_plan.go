package http

// ontology_plan.go — POST /v1/_ontology/plan, and the connector method every
// transport's plan goes through (RFC GQ P1).
//
// The plan itself lives in the Document (builtin.PlanOntology): it reads, applies the
// changes in memory, counts, and writes nothing. What lives here is who may ask and in
// which tenant — once, so a later gRPC or MCP twin cannot differ from HTTP in either.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// ontologyPlanBodyLimit bounds the request: at most 50 changes, each an edit body of
// at most 32 KiB in the Document's check, but one plan's whole body is held here.
const ontologyPlanBodyLimit = 64 << 10

// handleOntologyPlan serves POST /v1/_ontology/plan. The body is {changes: [...]};
// an admin may focus another tenant with ?tenant=, as on the other ontology routes.
func (s *Server) handleOntologyPlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Changes []connector.OntologyChange `json:"changes"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, ontologyPlanBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"expected {\"changes\": [{\"op\": \"retire|rename|move|edit|delete\", \"entry\": \"<chunk id>\", …}]}, at most 64 KiB")
		return
	}
	res, err := s.PlanOntology(r.Context(), connector.PlanOntologyRequest{
		Tenant: r.URL.Query().Get("tenant"), Changes: body.Changes,
	})
	if err != nil {
		writeOntologyPlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeOntologyPlanError maps a plan refusal onto its HTTP status, with the shared
// code as the error code. Anything else is an internal failure; its text is not
// returned, since a store fault's text can carry a path or a DSN.
func writeOntologyPlanError(w http.ResponseWriter, err error) {
	code := connector.OntologyPlanCode(err)
	switch {
	case errors.Is(err, connector.ErrOntologyPlanInvalid):
		writeJSONError(w, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, connector.ErrOntologyPlanForbidden):
		writeJSONError(w, http.StatusForbidden, code, err.Error())
	case errors.Is(err, connector.ErrOntologyPlanNotFound):
		writeJSONError(w, http.StatusNotFound, code, err.Error())
	case errors.Is(err, connector.ErrOntologyPlanUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, code, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "plan_failed", "the plan could not be computed; see the server log")
	}
}

// PlanOntology implements connector.Connector: the one path every transport's plan
// takes.
//
// The caller is checked HERE, not only at the route: the counts cover every scope of
// the tenant, so the plan is an admin's or a tenant operator's (substrate:tenant, not
// held to the run plane), never a member's. The tenant is the caller's own unless an
// admin names another.
func (s *Server) PlanOntology(ctx context.Context, req connector.PlanOntologyRequest) (connector.PlanOntologyResult, error) {
	if !subjectOperator(ctx) {
		return connector.PlanOntologyResult{}, connector.WithMessage(connector.ErrOntologyPlanForbidden,
			"planning ontology changes is for an admin or a tenant operator")
	}
	if s.store == nil || s.sqlMem == nil {
		return connector.PlanOntologyResult{}, connector.WithMessage(connector.ErrOntologyPlanUnavailable,
			"planning ontology changes needs SQL Memory; none is configured")
	}
	tenant := s.adminFocusedTenant(ctx, req.Tenant)
	res, err := s.ontologyDocument().PlanOntology(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}), req.Changes)
	res.Tenant = tenant
	if err != nil && connector.OntologyPlanCode(err) == "" {
		slog.ErrorContext(ctx, "ontology plan failed", "tenant", tenant, "err", err)
	}
	return res, err
}
