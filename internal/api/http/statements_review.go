package http

// statements_review.go — the statement review queue, bulk rulings and their
// undo (RFC FS P0): GET /v1/_statements/review, POST /v1/_statements/rule and
// POST /v1/_statements/revert, and the shared connector methods every
// transport's review goes through.
//
// The queue and the rulings live in the Document (builtin.ListReviewQueue,
// RuleStatements, RevertRuling). What lives here is who may review, as whom,
// in which tenant and scope, and the audit record — once, so gRPC and MCP,
// which reach the connector past their own gates, cannot differ from HTTP.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// statementRuleBodyMax bounds a ruling body: a thousand ids and a filter.
const statementRuleBodyMax = 256 << 10

// SetStatementReviewAudit wires the sink bulk rulings and their reverts are
// recorded to. Nil (no audit path) leaves them available and logged only: a
// ruling deletes nothing, and the ruling itself is recorded in the scope.
func (s *Server) SetStatementReviewAudit(sink audit.Sink) { s.reviewAudit = sink }

// handleStatementReviewQueue serves GET /v1/_statements/review: the queue's
// filters as query parameters (kinds may repeat or be comma-separated;
// created_before / created_after take unix nanoseconds or RFC 3339).
func (s *Server) handleStatementReviewQueue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := connector.ListReviewQueueRequest{
		Tenant: q.Get("tenant"), Scope: q.Get("scope"), After: q.Get("after"), Order: q.Get("order"),
	}
	var err error
	if req.Filter, err = reviewFilterFromQuery(q); err == nil {
		req.Summarize, err = optionalBool(q, "summarize")
	}
	if err == nil && q.Get("limit") != "" {
		req.Limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	res, err := s.ListReviewQueue(r.Context(), req)
	if err != nil {
		writeStatementReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// reviewFilterFromQuery reads the queue filter from query parameters.
func reviewFilterFromQuery(q url.Values) (connector.ReviewQueueFilter, error) {
	f := connector.ReviewQueueFilter{
		Status: q.Get("status"), Kind: q.Get("kind"), Method: q.Get("method"), Actor: q.Get("actor"),
		DocumentID: q.Get("document_id"), UnderPath: q.Get("under_path"),
		FromDocumentID: q.Get("from_document_id"), ToDocumentID: q.Get("to_document_id"),
		FromUnderPath: q.Get("from_under_path"), ToUnderPath: q.Get("to_under_path"),
		JudgeVerdict: q.Get("judge_verdict"),
	}
	for _, v := range q["kinds"] {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				f.Kinds = append(f.Kinds, k)
			}
		}
	}
	if v := q.Get("min_confidence"); v != "" {
		c, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return f, errors.New("min_confidence must be a number between 0 and 1")
		}
		f.MinConfidence = &c
	}
	for _, t := range []struct {
		name string
		dst  **int64
	}{{"created_before", &f.CreatedBefore}, {"created_after", &f.CreatedAfter}} {
		if v := q.Get(t.name); v != "" {
			n, err := connector.ParseReviewInstant(v)
			if err != nil {
				return f, errors.New(t.name + ": " + err.Error())
			}
			*t.dst = &n
		}
	}
	if q.Get("has_judge_verdict") != "" {
		b, err := optionalBool(q, "has_judge_verdict")
		if err != nil {
			return f, err
		}
		f.HasJudgeVerdict = &b
	}
	return f, nil
}

func optionalBool(q url.Values, name string) (bool, error) {
	v := q.Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, errors.New(name + " must be true or false")
	}
	return b, nil
}

// handleStatementRule serves POST /v1/_statements/rule; the body is
// connector.RuleStatementsRequest. An admin may focus another tenant with
// ?tenant= (or the body's tenant).
func (s *Server) handleStatementRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		connector.RuleStatementsRequest
		Filter *ruleBodyFilter `json:"filter,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, statementRuleBodyMax)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			`expected {"action": "accept|reject", "ids": [...] | "filter": {...}, "reason", "dry_run", "expect_count", "max"}`)
		return
	}
	req := body.RuleStatementsRequest
	if body.Filter != nil {
		f, err := body.Filter.resolve()
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		req.Filter = &f
	}
	if req.Tenant == "" {
		req.Tenant = r.URL.Query().Get("tenant")
	}
	res, err := s.RuleStatements(r.Context(), req)
	if err != nil {
		writeStatementReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ruleBodyFilter is a ruling body's filter: the queue filter, with the two
// time bounds taken as a number or as text (unix nanoseconds, or RFC 3339), as
// the queue's query takes them. A JavaScript client cannot send a nanosecond
// created_at as an exact number.
type ruleBodyFilter struct {
	connector.ReviewQueueFilter
	CreatedBefore json.RawMessage `json:"created_before,omitempty"`
	CreatedAfter  json.RawMessage `json:"created_after,omitempty"`
}

func (f *ruleBodyFilter) resolve() (connector.ReviewQueueFilter, error) {
	out := f.ReviewQueueFilter
	for _, t := range []struct {
		name string
		raw  json.RawMessage
		dst  **int64
	}{{"created_before", f.CreatedBefore, &out.CreatedBefore}, {"created_after", f.CreatedAfter, &out.CreatedAfter}} {
		if len(t.raw) == 0 || string(t.raw) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(t.raw, &text) != nil {
			text = string(t.raw)
		}
		n, err := connector.ParseReviewInstant(text)
		if err != nil {
			return out, errors.New("filter." + t.name + ": " + err.Error())
		}
		*t.dst = &n
	}
	return out, nil
}

// handleStatementRevert serves POST /v1/_statements/revert {ruling_id}.
func (s *Server) handleStatementRevert(w http.ResponseWriter, r *http.Request) {
	var req connector.RevertRulingRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", `expected {"ruling_id": "…"}`)
		return
	}
	if req.Tenant == "" {
		req.Tenant = r.URL.Query().Get("tenant")
	}
	res, err := s.RevertRuling(r.Context(), req)
	if err != nil {
		writeStatementReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeStatementReviewError maps a review refusal onto its HTTP status, with
// the shared code as the error code.
func writeStatementReviewError(w http.ResponseWriter, err error) {
	code := connector.StatementReviewCode(err)
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, connector.ErrStatementReviewInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, connector.ErrStatementReviewForbidden):
		status = http.StatusForbidden
	case errors.Is(err, connector.ErrStatementReviewRulingNotFound):
		status = http.StatusNotFound
	case errors.Is(err, connector.ErrStatementReviewQueueChanged):
		status = http.StatusConflict
	case errors.Is(err, connector.ErrStatementReviewUnavailable):
		status = http.StatusServiceUnavailable
	default:
		code = "review_failed"
	}
	writeJSONError(w, status, code, err.Error())
}

// reviewer decides whether the caller may use the review surface at all, and as
// whom it rules: an admin or a tenant operator rules as the operator; a
// non-isolated member as a reviewer, which each statement then asks for its
// `review` grant. Open mode has no principal and is the operator. An isolated
// user and a token held to the run plane are refused: neither is a reviewer of
// the tenant's documents.
func reviewer(ctx context.Context) (builtin.RulingAuthority, *auth.Principal, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return builtin.RuleAsOperator, nil, nil
	}
	switch {
	case p.Restrictions.RunPlaneOnly():
		return 0, &p, connector.WithMessage(connector.ErrStatementReviewForbidden,
			"a token held to certain agents does not review statements")
	case auth.HasScope(p.Scopes, auth.ScopeTenant):
		return builtin.RuleAsOperator, &p, nil
	case auth.IsIsolated(p, ok):
		return 0, &p, connector.WithMessage(connector.ErrStatementReviewForbidden,
			"reviewing the tenant's statements is for an operator or a member, not an isolated user")
	}
	return builtin.RuleAsReviewer, &p, nil
}

// reviewScope is the document scope a review acts on: tenant (the default)
// or the caller's own user scope.
func reviewScope(scope string) (string, error) {
	switch scope {
	case "", "tenant":
		return "tenant", nil
	case "user":
		return "user", nil
	}
	return "", connector.WithMessage(connector.ErrStatementReviewInvalid, `scope must be "tenant" or "user"`)
}

// reviewRuler is reviewer for a ruling or a revert: a member must hold the
// `review` grant on some document, or there is nothing it could rule on.
func reviewRuler(ctx context.Context) (builtin.RulingAuthority, *auth.Principal, error) {
	as, p, err := reviewer(ctx)
	if err == nil && as == builtin.RuleAsReviewer && !authz.GrantsSomewhere(p.Effective, "document", "review") {
		err = connector.WithMessage(connector.ErrStatementReviewForbidden,
			"ruling on statements needs an operator, or the access-control action review on the documents' paths, which you do not hold")
	}
	return as, p, err
}

// reviewCtx is the ctx a review runs under: the off-run Document ctx of
// POST /v1/_document (the principal's own subject; an admin's tenant focus).
func (s *Server) reviewCtx(ctx context.Context, tenant string) (context.Context, string) {
	return substrateBrowseCtx(ctx, "", tenant), s.adminFocusedTenant(ctx, tenant)
}

// ListReviewQueue implements connector.Connector.
func (s *Server) ListReviewQueue(ctx context.Context, req connector.ListReviewQueueRequest) (connector.ListReviewQueueResult, error) {
	if _, _, err := reviewer(ctx); err != nil {
		return connector.ListReviewQueueResult{}, err
	}
	scope, err := reviewScope(req.Scope)
	if err != nil {
		return connector.ListReviewQueueResult{}, err
	}
	if s.store == nil || s.sqlMem == nil {
		return connector.ListReviewQueueResult{}, connector.WithMessage(connector.ErrStatementReviewUnavailable,
			"the review queue needs SQL Memory; none is configured")
	}
	dctx, tenant := s.reviewCtx(ctx, req.Tenant)
	res, err := s.ontologyDocument().ListReviewQueue(dctx, scope, req)
	res.Tenant = tenant
	return res, err
}

// RuleStatements implements connector.Connector.
func (s *Server) RuleStatements(ctx context.Context, req connector.RuleStatementsRequest) (connector.RuleStatementsResult, error) {
	as, p, err := reviewRuler(ctx)
	if err != nil {
		return connector.RuleStatementsResult{}, err
	}
	scope, err := reviewScope(req.Scope)
	if err != nil {
		return connector.RuleStatementsResult{}, err
	}
	if s.store == nil || s.sqlMem == nil {
		return connector.RuleStatementsResult{}, connector.WithMessage(connector.ErrStatementReviewUnavailable,
			"ruling on statements needs SQL Memory; none is configured")
	}
	dctx, tenant := s.reviewCtx(ctx, req.Tenant)
	res, err := s.ontologyDocument().RuleStatements(dctx, scope, req, as)
	res.Tenant = tenant
	if err == nil && !res.DryRun {
		ev := s.reviewEvent(p, "statement_ruling", tenant, scope, res.RulingID)
		ev.RulingAction, ev.RulingIDs, ev.RulingReason = req.Action, req.IDs, strings.TrimSpace(req.Reason)
		if req.Filter != nil {
			ev.RulingFilter, _ = json.Marshal(req.Filter)
		}
		ev.RulingCounts = map[string]int{"matched": res.Matched, "skipped": res.Skipped, "ruled": res.Ruled, "failed": res.Failed}
		s.recordReview(ctx, ev)
	}
	return res, err
}

// RevertRuling implements connector.Connector.
func (s *Server) RevertRuling(ctx context.Context, req connector.RevertRulingRequest) (connector.RevertRulingResult, error) {
	as, p, err := reviewRuler(ctx)
	if err != nil {
		return connector.RevertRulingResult{}, err
	}
	scope, err := reviewScope(req.Scope)
	if err != nil {
		return connector.RevertRulingResult{}, err
	}
	if s.store == nil || s.sqlMem == nil {
		return connector.RevertRulingResult{}, connector.WithMessage(connector.ErrStatementReviewUnavailable,
			"reverting a ruling needs SQL Memory; none is configured")
	}
	dctx, tenant := s.reviewCtx(ctx, req.Tenant)
	res, err := s.ontologyDocument().RevertRuling(dctx, scope, req, as)
	res.Tenant = tenant
	if err == nil {
		ev := s.reviewEvent(p, "statement_ruling_revert", tenant, scope, req.RulingID)
		ev.RulingAction = res.Action
		ev.RulingCounts = map[string]int{"reverted": res.Reverted, "changed": res.Changed, "skipped": res.Skipped}
		s.recordReview(ctx, ev)
	}
	return res, err
}

// reviewEvent is the audit record's common part: who, from the principal —
// never the body — and on what.
func (s *Server) reviewEvent(p *auth.Principal, action, tenant, scope, rulingID string) audit.Event {
	ev := audit.Event{Action: action, TargetTenant: tenant, RulingID: rulingID, RulingScope: scope}
	if p != nil {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	return ev
}

// recordReview logs a ruling or a revert and writes its audit record. A
// failed write is logged, not returned: the statements are ruled either way,
// and the scope's own record of the ruling stands.
func (s *Server) recordReview(ctx context.Context, ev audit.Event) {
	slog.InfoContext(ctx, "statement review", "action", ev.Action, "ruling_id", ev.RulingID, "tenant", ev.TargetTenant,
		"scope", ev.RulingScope, "actor", ev.ActorTenant+"/"+ev.ActorSubject, "counts", ev.RulingCounts)
	if s.reviewAudit == nil {
		return
	}
	if err := s.reviewAudit.Record(ev); err != nil {
		slog.ErrorContext(ctx, "audit: statement review record failed", "action", ev.Action, "ruling_id", ev.RulingID, "err", err)
	}
}
