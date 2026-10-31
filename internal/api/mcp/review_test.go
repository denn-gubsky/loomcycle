package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// review hands each op's arguments to the connector as they are: who may
// review, the tenant and the rulings live there. A refusal comes back as the
// HTTP error body's {code, error}, and an unknown op never reaches it.
func TestReviewTool_DelegatesEachOpToTheConnector(t *testing.T) {
	mc := &mockConnector{
		reviewQueueRes: connector.ListReviewQueueResult{Tenant: "acme", Total: 3},
		ruleRes:        connector.RuleStatementsResult{RulingID: "r1", Ruled: 2},
		revertRes:      connector.RevertRulingResult{RulingID: "r1", Reverted: 2},
	}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		res, err := handleReview(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(`{"op":"queue","tenant":"globex","summarize":true,"limit":7,"filter":{"method":"rule:","from_under_path":"/board","min_confidence":0.5,
		"created_after":"2026-10-31T00:00:00Z","created_before":"1793481550963216000","has_judge_verdict":false}}`)
	if res.IsError {
		t.Fatalf("queue: %s", res.Content[0].Text)
	}
	q := mc.reviewQueueReq
	if q.Tenant != "globex" || !q.Summarize || q.Limit != 7 || q.Filter.Method != "rule:" || q.Filter.FromUnderPath != "/board" ||
		q.Filter.MinConfidence == nil || *q.Filter.MinConfidence != 0.5 || q.Filter.CreatedAfter == nil || *q.Filter.CreatedAfter != 1793404800000000000 ||
		q.Filter.CreatedBefore == nil || *q.Filter.CreatedBefore != 1793481550963216000 || q.Filter.HasJudgeVerdict == nil || *q.Filter.HasJudgeVerdict {
		t.Errorf("connector got %+v", q)
	}

	if res := call(`{"op":"rule","action":"reject","reason":"why","ids":["s1","s2"],"dry_run":false,"expect_count":2,"max":10}`); res.IsError {
		t.Fatalf("rule: %s", res.Content[0].Text)
	}
	r := mc.ruleReq
	if r.Action != "reject" || r.Reason != "why" || len(r.IDs) != 2 || r.Filter != nil || r.DryRun == nil || *r.DryRun || r.ExpectCount == nil || *r.ExpectCount != 2 || r.Max != 10 {
		t.Errorf("connector got %+v", r)
	}
	// No dry_run on the call: the connector sees none, which is a dry run.
	call(`{"op":"rule","action":"accept","filter":{}}`)
	if mc.ruleReq.DryRun != nil || mc.ruleReq.Filter == nil {
		t.Errorf("a rule without dry_run reached the connector as %+v", mc.ruleReq)
	}

	if res := call(`{"op":"revert","ruling_id":"r1","scope":"tenant"}`); res.IsError || mc.revertReq.RulingID != "r1" {
		t.Errorf("revert: %+v %+v", res, mc.revertReq)
	}

	mc.reviewQueueReq = connector.ListReviewQueueRequest{}
	if res := call(`{"op":"queue","filter":{"created_after":"yesterday"}}`); !res.IsError || mc.reviewQueueReq.Summarize {
		t.Errorf("a bad time bound: %+v", res)
	}
	if res := call(`{"op":"purge"}`); !res.IsError {
		t.Errorf("an unknown op was accepted")
	}

	mc.reviewErr = connector.WithMessage(connector.ErrStatementReviewQueueChanged, "the selection now matches 5 statements, not the 4 expected")
	res = call(`{"op":"rule","action":"accept","ids":["s1"],"dry_run":false,"expect_count":4}`)
	var refusal struct{ Code, Error string }
	if !res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &refusal) != nil || refusal.Code != "queue_changed" || !strings.Contains(refusal.Error, "5 statements") {
		t.Errorf("a refusal was not surfaced with its code: %+v", res)
	}
}

// Who may call it on this transport: an admin, a tenant operator and a member
// (whose `review` grant the connector asks per statement). An isolated user and
// a token limited to certain agents may not, and do not see it listed.
func TestReviewTool_MembersMayCallItIsolatedUsersMayNot(t *testing.T) {
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	for what, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"an admin":           {as([]string{auth.ScopeAdmin}, auth.Restrictions{}), true},
		"a tenant operator":  {as([]string{auth.ScopeTenant}, auth.Restrictions{}), true},
		"a member":           {as([]string{auth.ScopeRunsRead}, auth.Restrictions{}), true},
		"an isolated user":   {as([]string{auth.ScopeUser}, auth.Restrictions{}), false},
		"a restricted token": {as([]string{auth.ScopeTenant}, auth.Restrictions{Agents: []string{"judge"}}), false},
	} {
		if got := principalMayCallTool(tc.ctx, "review"); got != tc.want {
			t.Errorf("%s may call review = %v, want %v", what, got, tc.want)
		}
	}
}
