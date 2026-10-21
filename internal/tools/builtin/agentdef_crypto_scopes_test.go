package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// crypto_scopes authored over the substrate survives persist → resolve, so a
// runtime-authored signer does not come back default-deny on reload.
func TestAgentDefTool_CryptoScopesRoundTrip(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"*"} }) // operator plane

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"signer","promote":true,"overlay":{
		"system_prompt":"sign things","tools":["Crypto"],"crypto_scopes":["user","run"]}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "signer")
	if !ok {
		t.Fatal("signer not found after create+promote")
	}
	if !reflect.DeepEqual(def.CryptoScopes, []string{"user", "run"}) {
		t.Errorf("CryptoScopes = %v, want [user run]", def.CryptoScopes)
	}

	// A fork that does not send crypto_scopes keeps its parent's.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"signer","promote":true,"overlay":{"system_prompt":"sign more things"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	def, _ = lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "signer")
	if !reflect.DeepEqual(def.CryptoScopes, []string{"user", "run"}) {
		t.Errorf("after a fork that did not send it, CryptoScopes = %v, want [user run]", def.CryptoScopes)
	}
}

// The substrate write path refuses the values operator yaml refuses — even for
// the operator, whose authority the ceiling does not narrow.
func TestAgentDefTool_RefusesAnUnknownCryptoScope(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"*"} })

	for _, overlay := range []string{`{"crypto_scopes":["global"]}`, `{"crypto_scopes":["user","-*"]}`} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"signer","overlay":`+overlay+`}`))
		wantRefused(t, res, overlay, "crypto_scopes")
	}
}

// With no key scope to grant there is nothing to judge: an unset or deny-all
// crypto_scopes passes for an author holding no Crypto grant at all, and a fork
// may keep what the forked version already held.
func TestAgentDefCreate_CryptoCeilingUnsetAndForkedBase(t *testing.T) {
	tool, ctx := ceilingFixture(t) // in-run author, no Crypto policy on ctx

	if res := agentDefOp(t, tool, ctx, "create", "sdlc/plain", `{"history_scope":["user"]}`); res.IsError {
		t.Fatalf("unset crypto_scopes was judged: %s", res.Text)
	}
	if res := agentDefOp(t, tool, ctx, "create", "sdlc/none", `{"history_scope":["user"],"crypto_scopes":["-*"]}`); res.IsError {
		t.Fatalf("deny-all crypto_scopes was judged: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{"history_scope":["user"],"crypto_scopes":["agent"]}`),
		"no Crypto grant", "crypto_scopes:", `"agent"`, "ask an operator")

	// An operator mints a tenant-key signer; the in-run author holding only
	// [user] may fork it keeping [tenant], but may not add [run].
	op := operatorPlane(context.Background())
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) { rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}} })
	if res, _ := tool.Execute(op, json.RawMessage(`{"op":"create","name":"sdlc/signer","promote":true,
		"overlay":{"history_scope":["user"],"crypto_scopes":["tenant"]}}`)); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	author := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Crypto = tools.CryptoPolicyValue{Scopes: []string{"user"}} })
	if res := agentDefOp(t, tool, author, "fork", "sdlc/signer", `{"crypto_scopes":["tenant","user"]}`); res.IsError {
		t.Fatalf("fork keeping the base's scope: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, author, "fork", "sdlc/signer", `{"crypto_scopes":["tenant","run"]}`),
		"fork widening", "crypto_scopes:", `"run"`)
}
