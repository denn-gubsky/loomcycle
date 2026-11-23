package mcp

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// The owner's ruling on RFC GW P9 D-P9-13 (2026-11-23): the `workspace` MCP
// tool runs commands on a workspace worker, so it is an operator's grant, not
// a member's by default. A tenant operator reaches it; a member reaches it
// only when an access-control list allows it `invoke mcp-tool:workspace`.

func workspaceGrantACL(t *testing.T, subject string) *authz.Effective {
	t.Helper()
	acl := authz.ACL{Name: "ws", Rules: []authz.Rule{{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"mcp-tool:workspace"}}}}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: subject},
		Bindings: []authz.Binding{{ACL: "ws", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"ws": {ACL: acl}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func memberCtx(eff *authz.Effective, scopes ...string) context.Context {
	p := auth.Principal{TenantID: "acme", Subject: "mia", Scopes: scopes, Effective: eff}
	return auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), authz.ModeCompat), "mcp"), p)
}

func TestWorkspaceMCP_AMemberIsRefusedByDefault(t *testing.T) {
	ctx := memberCtx(nil, auth.ScopeRunsCreate, auth.ScopeRunsRead)
	if principalMayCallTool(ctx, "workspace") {
		t.Error("a plain member token reaches the workspace tool")
	}
	// The member still reaches what it did: spawn_run needs the same scope.
	if !principalMayCallTool(ctx, "spawn_run") {
		t.Error("the member lost spawn_run")
	}
}

func TestWorkspaceMCP_AMemberGrantedByAnACLMayUseIt(t *testing.T) {
	ctx := memberCtx(workspaceGrantACL(t, "mia"), auth.ScopeRunsCreate, auth.ScopeRunsRead)
	if !principalMayCallTool(ctx, "workspace") {
		t.Error("a member an ACL allows `invoke mcp-tool:workspace` is refused")
	}
	// The grant does not lift the scope: without runs:create it stays shut.
	if principalMayCallTool(memberCtx(workspaceGrantACL(t, "mia"), auth.ScopeRunsRead), "workspace") {
		t.Error("an ACL grant reached the tool without runs:create")
	}
}

func TestWorkspaceMCP_ATenantOperatorMayUseIt(t *testing.T) {
	ctx := memberCtx(nil, auth.ScopeTenant, auth.ScopeRunsCreate)
	if !principalMayCallTool(ctx, "workspace") {
		t.Error("a tenant operator is refused the workspace tool")
	}
}

// The member baseline does not list it; the tenant operator's does.
func TestWorkspaceMCP_IsOnTheOperatorBaselineNotTheMembers(t *testing.T) {
	has := func(name string) bool {
		for _, r := range authz.Baselines()[name].Rules {
			if slices.Contains(r.Resources, "mcp-tool:workspace") {
				return true
			}
		}
		return false
	}
	if has(authz.BaselineMember) {
		t.Error("the member baseline grants mcp-tool:workspace")
	}
	if !has(authz.BaselineTenantOperator) {
		t.Error("the tenant-operator baseline does not grant mcp-tool:workspace")
	}
}
