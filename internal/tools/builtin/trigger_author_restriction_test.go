package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A schedule or webhook fires later with no token on ctx, so the def carries
// its author's operator-key and isolation bits and the fired run is stamped
// from them. The author's bits are the RUN's own (restored from the row on
// resume, where there is no principal) combined with the live principal's —
// the more restrictive wins.

// authorCtx is one shape of ctx an author can present.
type authorCtx struct {
	name           string
	principal      *auth.Principal
	run            tools.RunIdentityValue
	wantRestricted bool
	wantIsolated   bool
}

func authorCtxCases() []authorCtx {
	// A granular token without the operator-key scope, topped at substrate:user.
	restricted := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	// substrate:tenant carries the operator-key scope and is not isolated.
	loose := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}}
	confinedRun := tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme", UserID: "alice", OperatorKeyRestricted: true, Isolated: true}
	plainRun := tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme", UserID: "bob"}
	return []authorCtx{
		{name: "resumed confined run, no principal", run: confinedRun, wantRestricted: true, wantIsolated: true},
		{name: "confined run under a looser principal", principal: &loose, run: confinedRun, wantRestricted: true, wantIsolated: true},
		{name: "restricted principal off-run", principal: &restricted, run: tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"}, wantRestricted: true, wantIsolated: true},
		{name: "loose principal, unconfined run", principal: &loose, run: plainRun},
		{name: "open mode", run: tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"}},
	}
}

func (c authorCtx) apply(base context.Context) context.Context {
	ctx := tools.WithRunIdentity(base, c.run)
	if c.principal != nil {
		ctx = auth.WithPrincipal(ctx, *c.principal)
	}
	return ctx
}

// capturedBits reads the two bits off the definition a create/fork returned.
func capturedBits(t *testing.T, res tools.Result) (restricted, isolated bool) {
	t.Helper()
	if res.IsError {
		t.Fatalf("authoring refused: %s", res.Text)
	}
	def, _ := decodeResult(t, res.Text)["definition"].(map[string]any)
	if def == nil {
		t.Fatalf("result carries no definition: %s", res.Text)
	}
	restricted, _ = def["operator_key_restricted"].(bool)
	isolated, _ = def["isolated"].(bool)
	return restricted, isolated
}

func checkBits(t *testing.T, what string, c authorCtx, res tools.Result) {
	t.Helper()
	restricted, isolated := capturedBits(t, res)
	if restricted != c.wantRestricted || isolated != c.wantIsolated {
		t.Errorf("%s (%s): captured operator_key_restricted=%v isolated=%v, want %v/%v",
			what, c.name, restricted, isolated, c.wantRestricted, c.wantIsolated)
	}
}

func TestScheduleDef_CapturesTheMostRestrictiveOfRunAndPrincipal(t *testing.T) {
	for _, c := range authorCtxCases() {
		t.Run(c.name, func(t *testing.T) {
			tool, base, cleanup := scheduleDefFixture(t)
			defer cleanup()
			tool.Cfg.Env.OperatorKeyRestriction = true
			ctx := c.apply(base)

			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"author-sched","overlay":{"agent":"job-search-batch","schedule":"0 9 * * 1","user_id":"alice"}}`))
			checkBits(t, "create", c, res)
			res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"author-sched","overlay":{"user_id":"bob"}}`))
			checkBits(t, "fork", c, res)
		})
	}
}

func TestWebhookDef_CapturesTheMostRestrictiveOfRunAndPrincipal(t *testing.T) {
	for _, c := range authorCtxCases() {
		t.Run(c.name, func(t *testing.T) {
			tool, base, cleanup := webhookDefFixture(t)
			defer cleanup()
			tool.Cfg.Env.OperatorKeyRestriction = true
			ctx := c.apply(base)

			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"author-hook","overlay":{"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}}}`))
			checkBits(t, "create", c, res)
			// A fork of the static entry bootstraps the operator's row first;
			// the fork itself is this author's and must carry its bits.
			res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"rate_limit":{"requests_per_minute":120}}}`))
			checkBits(t, "fork", c, res)
		})
	}
}
