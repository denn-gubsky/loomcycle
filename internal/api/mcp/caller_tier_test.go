package mcp

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

// A member's MCP session is not the operator plane (RFC FY Part B): the
// Document rulings that decide for the tenant refuse it, as they do over HTTP,
// while the same calls from a tenant operator's session pass the gate. Before,
// every MCP session held the wildcard tools ceiling, which was the only thing
// those gates asked.
func TestMCPPrincipalCtx_AMemberIsRefusedTheOperatorsDocumentRulings(t *testing.T) {
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

	op := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
	member := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), mcpTierMember(t, "acme", "bob")))
	out, isErr := call(op, `{"op":"create_document","scope":"tenant","title":"shared","path":"/shared"}`)
	if isErr {
		t.Fatalf("the operator's create_document: %s", out)
	}
	var created struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(out), &created)

	accept := `{"op":"accept_classification","scope":"tenant","document_id":"` + created.DocumentID + `"}`
	for _, c := range []struct{ name, body, want string }{
		{"accept a classification", accept, "only an operator"},
		{"define a tenant-wide type", `{"op":"define_type","scope":"tenant","name":"task"}`, "operator"},
		{"lease the tenant scope", `{"op":"lease","scope":"tenant","name":"linker"}`, "lease"},
	} {
		if text, isErr := call(member, c.body); !isErr || !strings.Contains(text, c.want) {
			t.Errorf("a member's session may %s: %s", c.name, text)
		}
	}
	if text, isErr := call(member, `{"op":"lease","scope":"user","name":"linker"}`); isErr {
		t.Errorf("a member's lease on its own user scope was refused: %s", text)
	}
	// The tenant operator's session passes the ruling gate: what it hears is
	// that nothing is pending, not who may rule.
	if text, _ := call(op, accept); strings.Contains(text, "only an operator") {
		t.Errorf("the tenant operator's session was refused as a non-operator: %s", text)
	}
}

func mcpTierMember(t *testing.T, tenant, subject string) auth.Principal {
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

// TestMCPPrincipalCtx_StampsTheCallersTier: the MCP session's tier comes from
// its principal, as on the HTTP plane.
func TestMCPPrincipalCtx_StampsTheCallersTier(t *testing.T) {
	isolated, _ := auth.GrantableUserScopes("isolated")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	as := func(p auth.Principal) context.Context { return auth.WithPrincipal(context.Background(), p) }
	for name, c := range map[string]struct {
		ctx  context.Context
		want tools.Tier
	}{
		"stdio":           {context.Background(), tools.TierOperatorPlane},
		"admin":           {as(auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}), tools.TierOperatorPlane},
		"tenant operator": {as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}), tools.TierOperatorPlane},
		"member":          {as(auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes}), tools.TierMember},
		"isolated":        {as(auth.Principal{TenantID: "acme", Subject: "eve", Scopes: isolated}), tools.TierIsolated},
		// No subject routes an authenticated session to operatorCtx; its tier is
		// still its principal's.
		"member without a subject": {as(auth.Principal{TenantID: "acme", Scopes: memberScopes}), tools.TierMember},
	} {
		if got := tools.CallerTier(mcpPrincipalCtx(c.ctx)); got != c.want {
			t.Errorf("%s: tier = %s, want %s", name, got, c.want)
		}
	}
}
