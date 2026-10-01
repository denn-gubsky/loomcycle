package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The stdio `loomcycle mcp` and an open-mode /v1/_mcp session carry no
// principal — the launcher IS the operator — so the def tools must see operator
// authority there (a trigger may execute in a named tenant, a remote peer may
// name a free api_key_env). An authenticated session must not: it has a
// principal, and its scopes decide.
//
// Fails-before: the no-principal ctx carried no marker, so the def tools
// treated the stdio operator as a tenant-less non-admin.
func TestMCPPrincipalCtx_OnlyTheUnauthenticatedOperatorCarriesOperatorMarker(t *testing.T) {
	if !tools.IsUnauthenticatedOperator(mcpPrincipalCtx(context.Background())) {
		t.Error("the no-principal (stdio / open-mode) MCP ctx does not carry the unauthenticated-operator marker")
	}
	for name, p := range map[string]auth.Principal{
		"admin":  {TenantID: "", Subject: "ops", Scopes: []string{auth.ScopeAdmin}},
		"tenant": {TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}},
	} {
		ctx := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), p))
		if tools.IsUnauthenticatedOperator(ctx) {
			t.Errorf("an authenticated %s session carries the unauthenticated-operator marker", name)
		}
	}
}
