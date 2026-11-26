package http

// ontology_rollback.go — POST /v1/_ontology/rollback: apply the plan of rolling the
// tenant ontology back to a release (RFC GQ P5.7, D8).
//
// The rollback lives in the Document (builtin.ApplyOntologyRollback): it plans again,
// refuses a missing or stale fingerprint, and makes the writes through the ontology's own
// guards. What lives here is who may roll back and in which tenant, and how each refusal
// is answered — the same answers the change accept gives (ontology_proposals.go), since a
// rollback is a batch of the same writes.

import (
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
	// Checked here as well as at the route: a rollback rewrites the tenant's vocabulary,
	// the operator's decision (as the plan and the accept are).
	if !subjectOperator(r.Context()) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "rolling the ontology back is for an admin or a tenant operator")
		return
	}
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "rolling the ontology back needs SQL Memory; none is configured")
		return
	}
	tenant := s.ontologyTenant(r)
	rep, err := s.ontologyDocument().ApplyOntologyRollback(s.ontologyDocCtx(r.Context(), memInject{Tenant: tenant}), body.Seq, body.PlanFingerprint)
	if err != nil {
		s.answerRollbackError(w, r, tenant, rep, err)
		return
	}
	if len(rep.Applied) > 0 {
		s.recordOntologyRollback(r, tenant, rep)
	}
	writeJSON(w, http.StatusOK, struct {
		Tenant   string                 `json:"tenant"`
		Rollback builtin.RollbackReport `json:"rollback"`
	}{tenant, rep})
}

// answerRollbackError answers a rollback that did not complete: a refusal that wrote
// nothing, or one that went part of the way (recorded, with what was applied).
func (s *Server) answerRollbackError(w http.ResponseWriter, r *http.Request, tenant string, rep builtin.RollbackReport, err error) {
	var planRefusal *builtin.ChangePlanRefusal
	switch {
	case errors.Is(err, builtin.ErrRollbackPartiallyApplied):
		s.recordOntologyRollback(r, tenant, rep)
		writeJSONErrorBody(w, http.StatusConflict, struct {
			Code    string                 `json:"code"`
			Error   string                 `json:"error"`
			Applied []builtin.RollbackStep `json:"applied"`
		}{"partially_applied", err.Error(), rep.Applied})
	case errors.As(err, &planRefusal):
		writeJSONErrorBody(w, http.StatusConflict, struct {
			Code        string `json:"code"`
			Error       string `json:"error"`
			Fingerprint string `json:"fingerprint"`
		}{planRefusal.Code, planRefusal.Message, planRefusal.Fingerprint})
	case errors.Is(err, builtin.ErrChangeRenameJobPending):
		writeJSONError(w, http.StatusConflict, "rename_job_pending", err.Error())
	case errors.Is(err, builtin.ErrChangeEntryLocked):
		writeJSONError(w, http.StatusConflict, "entry_locked", err.Error())
	case errors.Is(err, builtin.ErrChangeProposalInvalid):
		writeJSONError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
	case connector.OntologyPlanCode(err) != "":
		writeOntologyPlanError(w, err)
	default:
		// A store fault's text can carry a path or a DSN: logged, not returned.
		slog.ErrorContext(r.Context(), "ontology rollback failed", "tenant", tenant, "release", rep.Release, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "ontology_write_failed", "the rollback could not be applied; see the server log")
	}
}

// recordOntologyRollback logs an applied rollback and writes its audit record, as an
// applied change is recorded (op rollback, the ops it applied).
func (s *Server) recordOntologyRollback(r *http.Request, tenant string, rep builtin.RollbackReport) {
	applied := make([]string, 0, len(rep.Applied))
	for _, st := range rep.Applied {
		applied = append(applied, st.Op+" "+st.Entry)
	}
	slog.InfoContext(r.Context(), "ontology rolled back", "tenant", tenant, "release", rep.Release,
		"applied", len(rep.Applied), "matches", rep.Matches, "risk", rep.Risk)
	if s.reviewAudit == nil {
		return
	}
	ev := audit.Event{Action: "ontology_change_applied", TargetTenant: tenant, OntologyOp: "rollback",
		OntologyApplied: applied, OntologyPlanRisk: rep.Risk,
		TargetName: "release " + strconv.FormatInt(rep.Release, 10)}
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	if err := s.reviewAudit.Record(ev); err != nil {
		slog.ErrorContext(r.Context(), "audit: ontology rollback record failed", "release", rep.Release, "err", err)
	}
}
