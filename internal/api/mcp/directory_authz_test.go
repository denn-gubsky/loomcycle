package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestDirectory_TenantsIsRefusedForANonAdmin pins the one sub-op that is
// cross-tenant.
//
// `directory` is classified tenant-confinable so its useful ops stay reachable —
// the handler takes the tenant from the principal and offers no wire field, so
// op=users and op=inspect physically cannot escape. But op=tenants IS cross-tenant
// information, and it is REFUSED for a non-admin rather than filtered: an empty
// list would be a lie, and a one-entry list would still confirm the caller's own
// tenant in a shape they cannot distinguish from "you are the only tenant".
func TestDirectory_TenantsIsRefusedForANonAdmin(t *testing.T) {
	env := &handlerEnv{connector: &mockConnector{}, logf: func(string, ...any) {}}

	tenantCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
	res, err := handleDirectory(tenantCtx, env, json.RawMessage(`{"op":"tenants"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a substrate:tenant principal enumerated tenants — the list itself is " +
			"cross-tenant information")
	}
	txt := dirText(res)
	if !strings.Contains(txt, "operator-admin") {
		t.Errorf("the refusal does not say what is required: %s", txt)
	}

	// The confined ops stay reachable for that same principal — a refusal that took
	// the whole tool with it would be the wrong fix.
	for _, args := range []string{`{"op":"users"}`, `{"op":"inspect","subject":"alice"}`} {
		res, err := handleDirectory(tenantCtx, env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Errorf("%s was refused for a tenant principal: %s", args, dirText(res))
		}
	}

	// An admin may enumerate.
	adminCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin},
	})
	res, err = handleDirectory(adminCtx, env, json.RawMessage(`{"op":"tenants"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("an admin was refused op=tenants: %s", dirText(res))
	}
}

// TestDirectory_TenantComesFromThePrincipal — the handler must not accept a tenant
// argument, because a subject id is only unique within one tenant.
func TestDirectory_TenantComesFromThePrincipal(t *testing.T) {
	mc := &mockConnector{}
	env := &handlerEnv{connector: mc, logf: func(string, ...any) {}}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
	// A wire tenant is passed and must be IGNORED, not honoured.
	if _, err := handleDirectory(ctx, env,
		json.RawMessage(`{"op":"inspect","subject":"alice","tenant":"globex"}`)); err != nil {
		t.Fatal(err)
	}
	if got := mc.dirTenant.Load(); got != nil && got.(string) != "acme" {
		t.Errorf("connector saw tenant %q — a wire field overrode the principal", got)
	}
}

// dirText pulls the first content block's text from a tool result.
func dirText(res *loommcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}
