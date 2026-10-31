package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The three review RPCs hand their requests to the connector, where who may
// review and the rulings live, and render what comes back field for field.
func TestStatementReview_RPCsDelegateToTheConnector(t *testing.T) {
	mc := &mockConnector{
		reviewQueueRes: connector.ListReviewQueueResult{Tenant: "acme", Scope: "tenant",
			Statements: []jsonBytes{[]byte(`{"id":"s1"}`)}, Truncated: true, NextCursor: "c1",
			Summary: []connector.ReviewQueueGroup{{Kind: "cites", Method: "rule:citation", FromPathPrefix: "/board/reg", JudgeVerdict: "accept", Count: 7}}, Total: 7},
		ruleRes: connector.RuleStatementsResult{Tenant: "acme", Scope: "tenant", Action: "reject", RulingID: "r1", Matched: 3, Selected: 3,
			Skipped: 1, ToRule: 2, Ruled: 1, Failed: 1, Sample: []jsonBytes{[]byte(`{"id":"s2"}`)},
			Breakdown: []connector.RulingBreakdown{{Kind: "cites", Method: "rule:citation", Count: 1}},
			Items:     []connector.RulingItem{{ID: "s2", Status: "rejected"}, {ID: "s3", Error: "not_a_proposal", Message: "m"}}},
		revertRes: connector.RevertRulingResult{Tenant: "acme", Scope: "tenant", RulingID: "r1", Action: "reject", Reverted: 1, Changed: 1, Skipped: 2,
			Items: []connector.RulingItem{{ID: "s2", Status: "proposed"}}},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	ctx := context.Background()

	minConf, before, verdict := 0.5, int64(42), true
	q, err := client.ListReviewQueue(ctx, &loomcyclepb.ListReviewQueueRequest{Tenant: "globex", Scope: "tenant", Summarize: true, Limit: 9, After: "a", Order: "verified",
		Filter: &loomcyclepb.ReviewQueueFilter{Kind: "cites", Kinds: []string{"repeals"}, Method: "rule:", Actor: "kb/linker", FromUnderPath: "/board", ToDocumentId: "d2",
			MinConfidence: &minConf, CreatedBefore: &before, HasJudgeVerdict: &verdict, JudgeVerdict: "accept"}})
	if err != nil {
		t.Fatal(err)
	}
	f := mc.reviewQueueReq.Filter
	if mc.reviewQueueReq.Tenant != "globex" || !mc.reviewQueueReq.Summarize || mc.reviewQueueReq.Limit != 9 || mc.reviewQueueReq.Order != "verified" ||
		f.Kind != "cites" || len(f.Kinds) != 1 || f.Method != "rule:" || f.Actor != "kb/linker" || f.FromUnderPath != "/board" || f.ToDocumentID != "d2" ||
		f.MinConfidence == nil || *f.MinConfidence != 0.5 || f.CreatedBefore == nil || *f.CreatedBefore != 42 || f.CreatedAfter != nil ||
		f.HasJudgeVerdict == nil || !*f.HasJudgeVerdict || f.JudgeVerdict != "accept" {
		t.Errorf("connector got %+v", mc.reviewQueueReq)
	}
	if q.GetTenant() != "acme" || len(q.GetStatementsJson()) != 1 || string(q.GetStatementsJson()[0]) != `{"id":"s1"}` || !q.GetTruncated() ||
		q.GetNextCursor() != "c1" || q.GetTotal() != 7 || q.GetSummary()[0].GetFromPathPrefix() != "/board/reg" || q.GetSummary()[0].GetCount() != 7 {
		t.Errorf("queue response = %+v", q)
	}

	// No filter and no dry_run on the wire: an ids ruling, a dry run.
	if _, err := client.RuleStatements(ctx, &loomcyclepb.RuleStatementsRequest{Action: "accept", Ids: []string{"s1"}}); err != nil {
		t.Fatal(err)
	}
	if mc.ruleReq.Filter != nil || mc.ruleReq.DryRun != nil || mc.ruleReq.ExpectCount != nil || len(mc.ruleReq.IDs) != 1 {
		t.Errorf("an ids ruling reached the connector as %+v", mc.ruleReq)
	}
	dry, expect := false, int32(3)
	r, err := client.RuleStatements(ctx, &loomcyclepb.RuleStatementsRequest{Tenant: "globex", Action: "reject", Reason: "why", HasFilter: true,
		DryRun: &dry, ExpectCount: &expect, Max: 500})
	if err != nil {
		t.Fatal(err)
	}
	if got := mc.ruleReq; got.Filter == nil || got.DryRun == nil || *got.DryRun || got.ExpectCount == nil || *got.ExpectCount != 3 ||
		got.Max != 500 || got.Reason != "why" || got.Tenant != "globex" {
		t.Errorf("connector got %+v", got)
	}
	if r.GetRulingId() != "r1" || r.GetMatched() != 3 || r.GetSkipped() != 1 || r.GetToRule() != 2 || r.GetRuled() != 1 || r.GetFailed() != 1 ||
		len(r.GetSampleJson()) != 1 || r.GetBreakdown()[0].GetCount() != 1 || len(r.GetItems()) != 2 || r.GetItems()[1].GetError() != "not_a_proposal" {
		t.Errorf("rule response = %+v", r)
	}

	v, err := client.RevertRuling(ctx, &loomcyclepb.RevertRulingRequest{RulingId: "r1", Scope: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if mc.revertReq.RulingID != "r1" || v.GetReverted() != 1 || v.GetChanged() != 1 || v.GetSkipped() != 2 || v.GetItems()[0].GetStatus() != "proposed" {
		t.Errorf("revert = %+v (connector got %+v)", v, mc.revertReq)
	}

	for sentinel, want := range map[error]codes.Code{
		connector.ErrStatementReviewInvalid:        codes.InvalidArgument,
		connector.ErrStatementReviewForbidden:      codes.PermissionDenied,
		connector.ErrStatementReviewQueueChanged:   codes.FailedPrecondition,
		connector.ErrStatementReviewRulingNotFound: codes.NotFound,
		connector.ErrStatementReviewUnavailable:    codes.Unavailable,
	} {
		mc.reviewErr = connector.WithMessage(sentinel, "why")
		_, err := client.RuleStatements(ctx, &loomcyclepb.RuleStatementsRequest{Action: "accept"})
		st := status.Convert(err)
		reason := ""
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reason = info.GetReason()
			}
		}
		if st.Code() != want || reason != connector.StatementReviewCode(sentinel) {
			t.Errorf("%v maps to %v %q, want %v %q", sentinel, st.Code(), reason, want, connector.StatementReviewCode(sentinel))
		}
	}
}

// jsonBytes is a statement as the connector returns it.
type jsonBytes = json.RawMessage

// The review RPCs are on the member path, as their routes are: a member is let
// through to the connector, which asks its `review` grant; an isolated user and
// a token limited to certain agents are refused at the gate.
func TestStatementReview_MembersReachTheConnector(t *testing.T) {
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	for _, rpc := range []string{"ListReviewQueue", "RuleStatements", "RevertRuling"} {
		method := grpcMethodPrefix + rpc
		if got := requiredScopeForRPC(method); got != auth.ScopeTenant {
			t.Fatalf("%s: required scope = %q, want %q", rpc, got, auth.ScopeTenant)
		}
		for what, tc := range map[string]struct {
			ctx  context.Context
			want bool
		}{
			"an admin":           {as([]string{auth.ScopeAdmin}, auth.Restrictions{}), true},
			"a tenant operator":  {as([]string{auth.ScopeTenant}, auth.Restrictions{}), true},
			"a member":           {as([]string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, auth.Restrictions{}), true},
			"an isolated user":   {as([]string{auth.ScopeUser}, auth.Restrictions{}), false},
			"a restricted token": {as([]string{auth.ScopeTenant}, auth.Restrictions{Agents: []string{"judge"}}), false},
		} {
			if err := enforceScope(tc.ctx, method); (err == nil) != tc.want {
				t.Errorf("%s %s: enforceScope = %v, want allowed=%v", rpc, what, err, tc.want)
			}
		}
	}
}
