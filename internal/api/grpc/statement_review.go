// statement_review.go — the gRPC twins of GET /v1/_statements/review,
// POST /v1/_statements/rule, POST /v1/_statements/revert and
// GET /v1/_statements/rulings (RFC FS P0). The
// connector holds the rules — who may review, as whom, in which tenant, and the
// rulings themselves — so this transport only maps the shapes and the refusals.
package grpc

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// ListReviewQueue lists the statements awaiting review, or their counts.
func (s *Server) ListReviewQueue(ctx context.Context, req *loomcyclepb.ListReviewQueueRequest) (*loomcyclepb.ListReviewQueueResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ListReviewQueue requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.ListReviewQueue(ctx, connector.ListReviewQueueRequest{
		Tenant: req.GetTenant(), Scope: req.GetScope(), ScopeID: req.GetScopeId(), Filter: reviewFilterFromProto(req.GetFilter()),
		Summarize: req.GetSummarize(), Limit: int(req.GetLimit()), After: req.GetAfter(), Order: req.GetOrder(),
	})
	if err != nil {
		return nil, statementReviewErrToStatus(err)
	}
	out := &loomcyclepb.ListReviewQueueResponse{
		Tenant: res.Tenant, Scope: res.Scope, StatementsJson: rawList(res.Statements),
		Truncated: res.Truncated, NextCursor: res.NextCursor, Total: int32(res.Total),
	}
	for _, g := range res.Summary {
		out.Summary = append(out.Summary, &loomcyclepb.ReviewQueueGroup{
			Kind: g.Kind, Method: g.Method, FromPathPrefix: g.FromPathPrefix, JudgeVerdict: g.JudgeVerdict, Count: int32(g.Count),
			Filter: reviewFilterToProto(g.Filter),
		})
	}
	return out, nil
}

// RuleStatements accepts or rejects statements in bulk.
func (s *Server) RuleStatements(ctx context.Context, req *loomcyclepb.RuleStatementsRequest) (*loomcyclepb.RuleStatementsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "RuleStatements requires a connector; this Server was constructed without one")
	}
	in := connector.RuleStatementsRequest{
		Tenant: req.GetTenant(), Scope: req.GetScope(), ScopeID: req.GetScopeId(), Action: req.GetAction(), IDs: req.GetIds(),
		Reason: req.GetReason(), Max: int(req.GetMax()),
	}
	if req.GetHasFilter() {
		f := reviewFilterFromProto(req.GetFilter())
		in.Filter = &f
	}
	if req.DryRun != nil {
		v := req.GetDryRun()
		in.DryRun = &v
	}
	if req.ExpectCount != nil {
		v := int(req.GetExpectCount())
		in.ExpectCount = &v
	}
	res, err := s.connector.RuleStatements(ctx, in)
	if err != nil {
		return nil, statementReviewErrToStatus(err)
	}
	out := &loomcyclepb.RuleStatementsResponse{
		Tenant: res.Tenant, Scope: res.Scope, Action: res.Action, DryRun: res.DryRun, RulingId: res.RulingID,
		Matched: int32(res.Matched), Selected: int32(res.Selected), Skipped: int32(res.Skipped), ToRule: int32(res.ToRule),
		Ruled: int32(res.Ruled), Failed: int32(res.Failed), SampleJson: rawList(res.Sample), Items: rulingItemsToProto(res.Items),
	}
	for _, b := range res.Breakdown {
		out.Breakdown = append(out.Breakdown, &loomcyclepb.RulingBreakdown{Kind: b.Kind, Method: b.Method, Count: int32(b.Count)})
	}
	return out, nil
}

// RevertRuling undoes a bulk ruling where nothing changed since.
func (s *Server) RevertRuling(ctx context.Context, req *loomcyclepb.RevertRulingRequest) (*loomcyclepb.RevertRulingResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "RevertRuling requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.RevertRuling(ctx, connector.RevertRulingRequest{
		Tenant: req.GetTenant(), Scope: req.GetScope(), ScopeID: req.GetScopeId(), RulingID: req.GetRulingId(),
	})
	if err != nil {
		return nil, statementReviewErrToStatus(err)
	}
	return &loomcyclepb.RevertRulingResponse{
		Tenant: res.Tenant, Scope: res.Scope, RulingId: res.RulingID, Action: res.Action,
		Reverted: int32(res.Reverted), Changed: int32(res.Changed), Skipped: int32(res.Skipped),
		Items: rulingItemsToProto(res.Items), RevertId: res.RevertID,
	}, nil
}

// ListRulings pages the scope's bulk rulings and their reverts, newest first.
func (s *Server) ListRulings(ctx context.Context, req *loomcyclepb.ListRulingsRequest) (*loomcyclepb.ListRulingsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ListRulings requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.ListRulings(ctx, connector.ListRulingsRequest{
		Tenant: req.GetTenant(), Scope: req.GetScope(), ScopeID: req.GetScopeId(), Limit: int(req.GetLimit()), After: req.GetAfter(),
	})
	if err != nil {
		return nil, statementReviewErrToStatus(err)
	}
	out := &loomcyclepb.ListRulingsResponse{Tenant: res.Tenant, Scope: res.Scope, NextCursor: res.NextCursor}
	for _, r := range res.Rulings {
		sel := &loomcyclepb.RulingSelector{
			Filter: reviewFilterToProto(r.Selector.Filter), HasFilter: r.Selector.Filter != nil,
			IdsCount: int32(r.Selector.IDsCount), Max: int32(r.Selector.Max), Reverts: r.Selector.Reverts,
		}
		out.Rulings = append(out.Rulings, &loomcyclepb.RulingRecord{
			Id: r.ID, Action: r.Action, RuledBy: r.RuledBy, Actor: r.Actor, Reason: r.Reason, Selector: sel,
			Matched: int32(r.Matched), Ruled: int32(r.Ruled), Failed: int32(r.Failed), Skipped: int32(r.Skipped),
			CreatedAt: r.CreatedAt, Reverted: r.Reverted, RevertedAt: r.RevertedAt, RevertedBy: r.RevertedBy,
		})
	}
	return out, nil
}

func reviewFilterFromProto(f *loomcyclepb.ReviewQueueFilter) connector.ReviewQueueFilter {
	out := connector.ReviewQueueFilter{
		Status: f.GetStatus(), Kind: f.GetKind(), Kinds: f.GetKinds(), Method: f.GetMethod(), Actor: f.GetActor(),
		DocumentID: f.GetDocumentId(), UnderPath: f.GetUnderPath(),
		FromDocumentID: f.GetFromDocumentId(), ToDocumentID: f.GetToDocumentId(),
		FromUnderPath: f.GetFromUnderPath(), ToUnderPath: f.GetToUnderPath(), JudgeVerdict: f.GetJudgeVerdict(),
		MethodExact: f.GetMethodExact(), KindExact: f.GetKindExact(), FromPathPrefix: f.GetFromPathPrefix(), FromUnfiled: f.GetFromUnfiled(),
	}
	if f != nil && f.MinConfidence != nil {
		v := f.GetMinConfidence()
		out.MinConfidence = &v
	}
	if f != nil && f.CreatedBefore != nil {
		v := f.GetCreatedBefore()
		out.CreatedBefore = &v
	}
	if f != nil && f.CreatedAfter != nil {
		v := f.GetCreatedAfter()
		out.CreatedAfter = &v
	}
	if f != nil && f.HasJudgeVerdict != nil {
		v := f.GetHasJudgeVerdict()
		out.HasJudgeVerdict = &v
	}
	return out
}

// reviewFilterToProto is reviewFilterFromProto's inverse; nil for nil.
func reviewFilterToProto(f *connector.ReviewQueueFilter) *loomcyclepb.ReviewQueueFilter {
	if f == nil {
		return nil
	}
	return &loomcyclepb.ReviewQueueFilter{
		Status: f.Status, Kind: f.Kind, Kinds: f.Kinds, Method: f.Method, Actor: f.Actor,
		DocumentId: f.DocumentID, UnderPath: f.UnderPath, FromDocumentId: f.FromDocumentID, ToDocumentId: f.ToDocumentID,
		FromUnderPath: f.FromUnderPath, ToUnderPath: f.ToUnderPath, MinConfidence: f.MinConfidence,
		CreatedBefore: f.CreatedBefore, CreatedAfter: f.CreatedAfter, HasJudgeVerdict: f.HasJudgeVerdict,
		JudgeVerdict: f.JudgeVerdict, MethodExact: f.MethodExact, KindExact: f.KindExact,
		FromPathPrefix: f.FromPathPrefix, FromUnfiled: f.FromUnfiled,
	}
}

func rawList(in []json.RawMessage) [][]byte {
	out := make([][]byte, len(in))
	for i, r := range in {
		out[i] = r
	}
	return out
}

func rulingItemsToProto(in []connector.RulingItem) []*loomcyclepb.RulingItem {
	out := make([]*loomcyclepb.RulingItem, len(in))
	for i, it := range in {
		out[i] = &loomcyclepb.RulingItem{Id: it.ID, Status: it.Status, Error: it.Error, Message: it.Message}
	}
	return out
}

// statementReviewErrToStatus maps a review refusal onto the gRPC code matching
// HTTP's status, with the shared code as the ErrorInfo reason.
func statementReviewErrToStatus(err error) error {
	reason := connector.StatementReviewCode(err)
	code := codes.Internal
	switch {
	case errors.Is(err, connector.ErrStatementReviewInvalid):
		code = codes.InvalidArgument
	case errors.Is(err, connector.ErrStatementReviewForbidden):
		code = codes.PermissionDenied
	case errors.Is(err, connector.ErrStatementReviewRulingNotFound):
		code = codes.NotFound
	case errors.Is(err, connector.ErrStatementReviewQueueChanged):
		code = codes.FailedPrecondition
	case errors.Is(err, connector.ErrStatementReviewUnavailable):
		code = codes.Unavailable
	default:
		reason = "review_failed"
	}
	st := status.New(code, err.Error())
	if withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain}); derr == nil {
		return withDetails.Err()
	}
	return st.Err()
}
