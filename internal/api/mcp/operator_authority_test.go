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

// An admin's MCP session is the operator's own off-run call: no run id rides
// on its ctx, so the def tools keep its operator authority. A run it starts
// with spawn_run goes through RunOnce, which stamps one on every tool call the
// run's model makes (see the http package's RunOnce test), and loses it.
func TestMCPPrincipalCtx_AnAdminSessionCarriesNoRunID(t *testing.T) {
	admin := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	if id := tools.RunID(mcpPrincipalCtx(admin)); id != "" {
		t.Errorf("an admin MCP session's ctx carries run id %q", id)
	}
	if id := tools.RunID(mcpPrincipalCtx(context.Background())); id != "" {
		t.Errorf("the stdio / open-mode MCP ctx carries run id %q", id)
	}
}
