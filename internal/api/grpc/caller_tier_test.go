package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Over gRPC, whose substrate plane holds no tenant memory scope, each caller
// works in its own user scope, where a member curates as the operator does
// (RFC FY Part B): accepting a classification there passes the ruling gate for
// both. The member's refusals elsewhere are the HTTP and MCP tests'; the tier
// each gRPC caller gets is TestSubstrateGRPCCtx_StampsTheCallersTier.
func TestSubstrateGRPCCtx_EachCallerRulesInItsOwnUserScope(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	doc := &builtin.Document{Store: st, SqlMem: mgr}
	call := func(ctx context.Context, body string) (string, bool) {
		res, err := doc.Execute(ctx, json.RawMessage(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return res.Text, res.IsError
	}

	op := substrateGRPCUserCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
	member := substrateGRPCUserCtx(auth.WithPrincipal(context.Background(), grpcTierMember(t, "acme", "bob")))
	accept := func(ctx context.Context) string {
		out, isErr := call(ctx, `{"op":"create_document","scope":"user","title":"mine","path":"/mine"}`)
		if isErr {
			t.Fatalf("create_document: %s", out)
		}
		var created struct {
			DocumentID string `json:"document_id"`
		}
		_ = json.Unmarshal([]byte(out), &created)
		text, _ := call(ctx, `{"op":"accept_classification","scope":"user","document_id":"`+created.DocumentID+`"}`)
		return text
	}
	if text := accept(member); strings.Contains(text, "only an operator") {
		t.Errorf("a member's call was refused a classification in its own user scope: %s", text)
	}
	if text, isErr := call(member, `{"op":"lease","scope":"user","name":"linker"}`); isErr {
		t.Errorf("a member's lease on its own user scope was refused: %s", text)
	}
	// The tenant operator's call passes the ruling gate: what it hears is that
	// nothing is pending, not who may rule.
	if text := accept(op); strings.Contains(text, "only an operator") {
		t.Errorf("the tenant operator's call was refused as a non-operator: %s", text)
	}
}

func grpcTierMember(t *testing.T, tenant, subject string) auth.Principal {
	t.Helper()
	rules := []authz.Rule{
		{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"memory:*", "sqlmem:*"}},
		{Effect: authz.Allow, Actions: []string{"read", "write", "delete", "link", "propose"}, Resources: []string{"document:*"}},
	}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: tenant, ID: subject},
		Bindings: []authz.Binding{{ACL: "m", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"m": {ACL: authz.ACL{Name: "m", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	return auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes, Effective: eff}
}

// TestSubstrateGRPCCtx_StampsTheCallersTier: the gRPC substrate call's tier
// comes from its principal, as on the HTTP plane.
func TestSubstrateGRPCCtx_StampsTheCallersTier(t *testing.T) {
	isolated, _ := auth.GrantableUserScopes("isolated")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	as := func(p auth.Principal) context.Context { return auth.WithPrincipal(context.Background(), p) }
	for name, c := range map[string]struct {
		ctx  context.Context
		want tools.Tier
	}{
		"open mode":       {context.Background(), tools.TierOperatorPlane},
		"admin":           {as(auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}), tools.TierOperatorPlane},
		"tenant operator": {as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}), tools.TierOperatorPlane},
		"member":          {as(auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes}), tools.TierMember},
		"isolated":        {as(auth.Principal{TenantID: "acme", Subject: "eve", Scopes: isolated}), tools.TierIsolated},
	} {
		for plane, ctx := range map[string]context.Context{"substrateGRPCCtx": substrateGRPCCtx(c.ctx), "substrateGRPCUserCtx": substrateGRPCUserCtx(c.ctx)} {
			if got := tools.CallerTier(ctx); got != c.want {
				t.Errorf("%s via %s: tier = %s, want %s", name, plane, got, c.want)
			}
		}
	}
}
