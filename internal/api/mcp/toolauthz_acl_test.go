package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EY: an explicit ACL deny on a meta-tool holds for every class, admin
// included; enforce mode refuses an unmatched tool and a missing set.
func TestPrincipalMayCallTool_ACLDenyHoldsForAdmin(t *testing.T) {
	deny := authz.ACL{Name: "t", Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"mcp-tool:operatortokendef"}}}}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: "ops"}, Bindings: []authz.Binding{{ACL: "t", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"t": {ACL: deny}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	admin := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}, Effective: eff}
	ctx := auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), authz.ModeCompat), "mcp"), admin)
	if principalMayCallTool(ctx, "operatortokendef") {
		t.Error("an ACL deny must hold for an admin (deny overrides)")
	}
	if !principalMayCallTool(ctx, "pause_runtime") {
		t.Error("a tool no rule names stays on the legacy answer in compat")
	}
	enforce := auth.WithPrincipal(authz.WithMode(context.Background(), authz.ModeEnforce), admin)
	if principalMayCallTool(enforce, "pause_runtime") {
		t.Error("enforce: a tool no rule allows is refused")
	}
	noSet := auth.WithPrincipal(authz.WithMode(context.Background(), authz.ModeEnforce), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	if principalMayCallTool(noSet, "spawn_run") {
		t.Error("enforce: no compiled set refuses")
	}
	compatNoSet := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	if !principalMayCallTool(compatNoSet, "spawn_run") {
		t.Error("compat: no compiled set is the pre-EY answer")
	}
}
