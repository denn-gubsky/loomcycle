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
	for _, rpc := range []string{"ListReviewQueue", "RuleStatements", "RevertRuling", "ListRulings", "ApplyReviewPolicy"} {
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

// ListRulings, the exact selectors, a group's filter and scope_id cross the
// RPCs field for field.
func TestStatementReview_RulingsSelectorsAndScopeIDCrossTheWire(t *testing.T) {
	verdict := false
	group := &connector.ReviewQueueFilter{KindExact: "cites", MethodExact: "rule:citation", FromUnfiled: true, HasJudgeVerdict: &verdict}
	mc := &mockConnector{
		reviewQueueRes: connector.ListReviewQueueResult{Summary: []connector.ReviewQueueGroup{{Kind: "cites", Count: 2, Filter: group}}},
		revertRes:      connector.RevertRulingResult{RevertID: "v1"},
		rulingsRes: connector.ListRulingsResult{Tenant: "acme", Scope: "user", NextCursor: "n1", Rulings: []connector.RulingRecord{
			{ID: "v1", Action: "revert", RuledBy: "operator:ops", Actor: "ops", Selector: connector.RulingSelector{Reverts: "r1"}, Matched: 2, Ruled: 2, CreatedAt: 9},
			{ID: "r1", Action: "accept", RuledBy: "operator:ops", Actor: "ops", Reason: "ok", Selector: connector.RulingSelector{Filter: group, Max: 50},
				Matched: 3, Ruled: 2, Failed: 1, Skipped: 4, CreatedAt: 7, Reverted: true, RevertedAt: 9, RevertedBy: "v1"},
		}},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	ctx := context.Background()

	q, err := client.ListReviewQueue(ctx, &loomcyclepb.ListReviewQueueRequest{Scope: "user", ScopeId: "bob", Summarize: true,
		Filter: &loomcyclepb.ReviewQueueFilter{MethodExact: "rule:citation", KindExact: "cites", FromPathPrefix: "/board/reg"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := mc.reviewQueueReq; r.ScopeID != "bob" || r.Filter.MethodExact != "rule:citation" || r.Filter.KindExact != "cites" || r.Filter.FromPathPrefix != "/board/reg" {
		t.Errorf("connector got %+v", r)
	}
	if g := q.GetSummary()[0].GetFilter(); g.GetKindExact() != "cites" || g.GetMethodExact() != "rule:citation" || !g.GetFromUnfiled() ||
		g.HasJudgeVerdict == nil || g.GetHasJudgeVerdict() {
		t.Errorf("group filter = %+v", g)
	}
	if _, err := client.RuleStatements(ctx, &loomcyclepb.RuleStatementsRequest{Action: "accept", Scope: "user", ScopeId: "bob", HasFilter: true,
		Filter: &loomcyclepb.ReviewQueueFilter{FromUnfiled: true}}); err != nil {
		t.Fatal(err)
	}
	if r := mc.ruleReq; r.ScopeID != "bob" || r.Filter == nil || !r.Filter.FromUnfiled {
		t.Errorf("connector got %+v", r)
	}
	v, err := client.RevertRuling(ctx, &loomcyclepb.RevertRulingRequest{RulingId: "r1", Scope: "user", ScopeId: "bob"})
	if err != nil || mc.revertReq.ScopeID != "bob" || v.GetRevertId() != "v1" {
		t.Errorf("revert = %+v %v (connector got %+v)", v, err, mc.revertReq)
	}
	l, err := client.ListRulings(ctx, &loomcyclepb.ListRulingsRequest{Tenant: "globex", Scope: "user", ScopeId: "bob", Limit: 2, After: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if r := mc.rulingsReq; r.Tenant != "globex" || r.Scope != "user" || r.ScopeID != "bob" || r.Limit != 2 || r.After != "c" {
		t.Errorf("connector got %+v", r)
	}
	if l.GetNextCursor() != "n1" || len(l.GetRulings()) != 2 {
		t.Fatalf("rulings = %+v", l)
	}
	rev, ruling := l.GetRulings()[0], l.GetRulings()[1]
	if rev.GetAction() != "revert" || rev.GetSelector().GetReverts() != "r1" || rev.GetSelector().GetHasFilter() || rev.GetCreatedAt() != 9 {
		t.Errorf("revert record = %+v", rev)
	}
	if ruling.GetReason() != "ok" || !ruling.GetSelector().GetHasFilter() || ruling.GetSelector().GetFilter().GetKindExact() != "cites" ||
		ruling.GetSelector().GetMax() != 50 || ruling.GetFailed() != 1 || ruling.GetSkipped() != 4 || !ruling.GetReverted() ||
		ruling.GetRevertedAt() != 9 || ruling.GetRevertedBy() != "v1" || ruling.GetActor() != "ops" || ruling.GetRuledBy() != "operator:ops" {
		t.Errorf("ruling record = %+v", ruling)
	}
}

// ApplyReviewPolicy hands its request to the connector — no filter on the wire
// is no filter, and no dry_run a dry run — and renders the result field for
// field; a refusal carries the shared code.
func TestStatementReview_ApplyReviewPolicyDelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{applyRes: connector.ApplyReviewPolicyResult{Tenant: "acme", Scope: "tenant", DryRun: false, Source: "review", Version: 7,
		RulingID: "r9", Matched: 6, Skipped: 1, Accept: 2, Reject: 1, Judge: 1, Human: 1, ToRule: 2, Deferred: 1, Ruled: 2, Marked: 1,
		Rules:  []connector.PolicyRuleCount{{Rule: 1, Action: "accept", Text: "accept kind:cites", Count: 2}, {Rule: 0, Action: "human", Count: 1}},
		Sample: []jsonBytes{[]byte(`{"id":"s1","review_rule":1}`)}, Items: []connector.RulingItem{{ID: "s1", Status: "asserted"}}}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	ctx := context.Background()
	if _, err := client.ApplyReviewPolicy(ctx, &loomcyclepb.ApplyReviewPolicyRequest{}); err != nil {
		t.Fatal(err)
	}
	if mc.applyReq.Filter != nil || mc.applyReq.DryRun != nil || mc.applyReq.ExpectCount != nil {
		t.Errorf("a bare call reached the connector as %+v", mc.applyReq)
	}
	dry, expect := false, int32(6)
	r, err := client.ApplyReviewPolicy(ctx, &loomcyclepb.ApplyReviewPolicyRequest{Tenant: "globex", Scope: "user", ScopeId: "u1",
		Filter: &loomcyclepb.ReviewQueueFilter{Kind: "cites"}, DryRun: &dry, ExpectCount: &expect, Max: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := mc.applyReq; got.Tenant != "globex" || got.Scope != "user" || got.ScopeID != "u1" || got.Filter == nil || got.Filter.Kind != "cites" || got.DryRun == nil || *got.DryRun ||
		got.ExpectCount == nil || *got.ExpectCount != 6 || got.Max != 2 {
		t.Errorf("connector got %+v", got)
	}
	if r.GetRulingId() != "r9" || r.GetSource() != "review" || r.GetVersion() != 7 || r.GetMatched() != 6 || r.GetSkipped() != 1 ||
		r.GetAccept() != 2 || r.GetReject() != 1 || r.GetJudge() != 1 || r.GetHuman() != 1 || r.GetToRule() != 2 || r.GetDeferred() != 1 ||
		r.GetRuled() != 2 || r.GetMarked() != 1 || len(r.GetRules()) != 2 || r.GetRules()[0].GetText() != "accept kind:cites" ||
		len(r.GetSampleJson()) != 1 || r.GetItems()[0].GetStatus() != "asserted" {
		t.Errorf("response = %+v", r)
	}
	mc.reviewErr = connector.WithMessage(connector.ErrStatementReviewForbidden, "operators only")
	_, err = client.ApplyReviewPolicy(ctx, &loomcyclepb.ApplyReviewPolicyRequest{})
	if st := status.Convert(err); st.Code() != codes.PermissionDenied {
		t.Errorf("a refusal = %v", err)
	}
}
