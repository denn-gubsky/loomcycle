package http

// ontology_rollback.go — POST /v1/_ontology/rollback: apply the plan of rolling the
// tenant ontology back to a release (RFC GQ P5.7, D8), and the connector method its
// gRPC and MCP twins go through (P5.6).
//
// The rollback lives in the Document (builtin.ApplyOntologyRollback): it plans again,
// refuses a missing or stale fingerprint, and makes the writes through the ontology's own
// guards. What lives here is who may roll back and in which tenant, the audit record,
// and how each refusal is named — once, so no transport can differ from another in any
// of them. The HTTP answers are the change accept's (ontology_proposals.go), since a
// rollback is a batch of the same writes.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// handleOntologyRollback serves POST /v1/_ontology/rollback. The body is {seq,
// plan_fingerprint}; an admin may focus another tenant with ?tenant=.
func (s *Server) handleOntologyRollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seq             int64  `json:"seq"`
		PlanFingerprint string `json:"plan_fingerprint"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", `expected {"seq": <release>, "plan_fingerprint": "<the plan's fingerprint>"}`)
		return
	}
	res, err := s.ApplyOntologyRollback(r.Context(), connector.ApplyOntologyRollbackRequest{
		Tenant: r.URL.Query().Get("tenant"), Seq: body.Seq, PlanFingerprint: body.PlanFingerprint,
	})
	if err != nil {
		writeOntologyRollbackError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeOntologyRollbackError answers a rollback that did not complete: 409 with the
// refusal's code (and the current fingerprint, or the writes made), 422 for a write
// refused before anything was written, the plan's own refusals as the plan route
// answers them, and 500 without its text for anything else.
func writeOntologyRollbackError(w http.ResponseWriter, err error) {
	var ref *connector.OntologyRollbackRefusal
	switch {
	case errors.As(err, &ref) && ref.Code == connector.RollbackPartiallyApplied:
		writeJSONErrorBody(w, http.StatusConflict, struct {
			Code    string                           `json:"code"`
			Error   string                           `json:"error"`
			Applied []connector.OntologyRollbackStep `json:"applied"`
		}{ref.Code, ref.Message, ref.Applied})
	case errors.As(err, &ref) && (ref.Code == connector.RollbackPlanRequired || ref.Code == connector.RollbackPlanStale):
		writeJSONErrorBody(w, http.StatusConflict, struct {
			Code        string `json:"code"`
			Error       string `json:"error"`
			Fingerprint string `json:"fingerprint"`
		}{ref.Code, ref.Message, ref.Fingerprint})
	case errors.As(err, &ref) && ref.Code == connector.RollbackInvalid:
		writeJSONError(w, http.StatusUnprocessableEntity, ref.Code, ref.Message)
	case errors.As(err, &ref):
		writeJSONError(w, http.StatusConflict, ref.Code, ref.Message)
	case connector.OntologyPlanCode(err) != "":
		writeOntologyPlanError(w, err)
	default:
		writeJSONError(w, http.StatusInternalServerError, "ontology_write_failed", "the rollback could not be applied; see the server log")
	}
}

// ApplyOntologyRollback implements connector.Connector. The caller is checked HERE,
// not only at the route: a rollback rewrites the tenant's vocabulary, the operator's
// decision (as the plan and the accept are). An applied rollback — whole or in part —
// is audited here, so every transport's is.
func (s *Server) ApplyOntologyRollback(ctx context.Context, req connector.ApplyOntologyRollbackRequest) (connector.ApplyOntologyRollbackResult, error) {
	out := connector.ApplyOntologyRollbackResult{Rollback: connector.OntologyRollbackReport{Applied: []connector.OntologyRollbackStep{}}}
	if !subjectOperator(ctx) {
		return out, connector.WithMessage(connector.ErrOntologyPlanForbidden, "rolling the ontology back is for an admin or a tenant operator")
	}
	if s.store == nil || s.sqlMem == nil {
		return out, connector.WithMessage(connector.ErrOntologyPlanUnavailable, "rolling the ontology back needs SQL Memory; none is configured")
	}
	tenant := s.adminFocusedTenant(ctx, req.Tenant)
	rep, err := s.ontologyDocument().ApplyOntologyRollback(s.ontologyDocCtx(ctx, memInject{Tenant: tenant}), req.Seq, req.PlanFingerprint)
	out.Tenant, out.Rollback = tenant, rep
	if err != nil {
		return out, s.rollbackRefusal(ctx, tenant, rep, err)
	}
	if len(rep.Applied) > 0 {
		s.recordOntologyRollback(ctx, tenant, rep)
	}
	return out, nil
}

// rollbackRefusal names a rollback that did not complete: a refusal that wrote nothing,
// or one that went part of the way (recorded, with what was applied).
func (s *Server) rollbackRefusal(ctx context.Context, tenant string, rep builtin.RollbackReport, err error) error {
	refuse := func(code string) error {
		return &connector.OntologyRollbackRefusal{Code: code, Message: err.Error()}
	}
	var planRefusal *builtin.ChangePlanRefusal
	switch {
	case errors.Is(err, builtin.ErrRollbackPartiallyApplied):
		s.recordOntologyRollback(ctx, tenant, rep)
		return &connector.OntologyRollbackRefusal{Code: connector.RollbackPartiallyApplied, Message: err.Error(), Applied: rep.Applied}
	case errors.As(err, &planRefusal):
		return &connector.OntologyRollbackRefusal{Code: planRefusal.Code, Message: planRefusal.Message, Fingerprint: planRefusal.Fingerprint}
	case errors.Is(err, builtin.ErrChangeRenameJobPending):
		return refuse(connector.RollbackRenameJobPending)
	case errors.Is(err, builtin.ErrChangeEntryLocked):
		return refuse(connector.RollbackEntryLocked)
	case errors.Is(err, builtin.ErrChangeProposalInvalid):
		return refuse(connector.RollbackInvalid)
	case connector.OntologyPlanCode(err) != "":
		return err
	}
	// A store fault's text can carry a path or a DSN: logged, not returned.
	slog.ErrorContext(ctx, "ontology rollback failed", "tenant", tenant, "release", rep.Release, "err", err)
	return err
}

// recordOntologyRollback logs an applied rollback and writes its audit record, as an
// applied change is recorded (op rollback, the ops it applied).
func (s *Server) recordOntologyRollback(ctx context.Context, tenant string, rep builtin.RollbackReport) {
	applied := make([]string, 0, len(rep.Applied))
	for _, st := range rep.Applied {
		applied = append(applied, st.Op+" "+st.Entry)
	}
	slog.InfoContext(ctx, "ontology rolled back", "tenant", tenant, "release", rep.Release,
		"applied", len(rep.Applied), "matches", rep.Matches, "risk", rep.Risk)
	if s.reviewAudit == nil {
		return
	}
	ev := audit.Event{Action: "ontology_change_applied", TargetTenant: tenant, OntologyOp: "rollback",
		OntologyApplied: applied, OntologyPlanRisk: rep.Risk,
		TargetName: "release " + strconv.FormatInt(rep.Release, 10)}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	if err := s.reviewAudit.Record(ev); err != nil {
		slog.ErrorContext(ctx, "audit: ontology rollback record failed", "release", rep.Release, "err", err)
	}
}
