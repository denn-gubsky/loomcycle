package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// TestToolClassification_EveryDispatchableToolClassified is the drift guard for
// RFC AG §3.3: every tool in handlersByName must be CONSCIOUSLY classified as
// either tenant-confinable or admin-only, and the two sets must be disjoint and
// contain no phantom (non-dispatchable) names. Adding a meta-tool to
// handlersByName without classifying it turns this test red — which is the
// point: the gate fails closed (deny-by-default → admin), but a silent
// admin-only default for a tool that SHOULD be tenant-safe is a usability bug we
// want caught at compile-test time, not in production.
func TestToolClassification_EveryDispatchableToolClassified(t *testing.T) {
	for name := range handlersByName {
		inTenant := tenantConfinableTools[name]
		inAdmin := adminOnlyTools[name]
		switch {
		case inTenant && inAdmin:
			t.Errorf("tool %q is in BOTH tenantConfinableTools and adminOnlyTools (must be exactly one)", name)
		case !inTenant && !inAdmin:
			t.Errorf("tool %q is dispatchable but UNCLASSIFIED — add it to tenantConfinableTools or adminOnlyTools (RFC AG §3.3)", name)
		}
	}
	// No phantom classifications: every classified name must be dispatchable.
	for name := range tenantConfinableTools {
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("tenantConfinableTools has %q which is not in handlersByName (stale entry)", name)
		}
	}
	for name := range adminOnlyTools {
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("adminOnlyTools has %q which is not in handlersByName (stale entry)", name)
		}
	}
}

// TestPrincipalMayCallTool_NoPrincipal pins the stdio / open-mode path: with no
// authenticated principal on ctx, every tool — including admin-only ones — is
// callable (process-local operator-trust). Regressing this would break the
// stdio MCP server (Claude Code etc.).
func TestPrincipalMayCallTool_NoPrincipal(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"document", "agentdef", "operatortokendef", "restore_snapshot"} {
		if !principalMayCallTool(ctx, name) {
			t.Errorf("no-principal path must allow %q (operator-trust)", name)
		}
	}
}

// TestPrincipalMayCallTool_Admin: an admin principal (incl. the legacy token,
// which carries ScopeAdmin) may call every tool.
func TestPrincipalMayCallTool_Admin(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	for name := range handlersByName {
		if !principalMayCallTool(ctx, name) {
			t.Errorf("admin principal must be allowed %q", name)
		}
	}
}

// TestPrincipalMayCallTool_NonAdmin is the load-bearing assertion for RFC AG
// Phase 2: a substrate:tenant principal may call the tenant-confinable tools
// (document/agentdef/memory/spawn_run/…) but NOT the admin-only ones
// (operatortokendef/restore_snapshot/pause_runtime/list_channels/register_hook).
// This is the enforcement the route-flip relies on.
func TestPrincipalMayCallTool_NonAdmin(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"substrate:tenant"}})

	allowed := []string{"document", "agentdef", "skilldef", "memory", "channel", "path", "spawn_run", "context", "evaluation"}
	for _, name := range allowed {
		if !principalMayCallTool(ctx, name) {
			t.Errorf("tenant principal must be allowed tenant-confinable tool %q", name)
		}
	}
	denied := []string{"operatortokendef", "restore_snapshot", "pause_runtime", "get_runtime_state", "list_channels", "register_hook", "delete_hook"}
	for _, name := range denied {
		if principalMayCallTool(ctx, name) {
			t.Errorf("tenant principal must NOT be allowed admin-only tool %q (RFC AG §2)", name)
		}
	}
}

// TestPrincipalMayCallTool_DenyByDefault is the fail-closed guard: a tool a
// non-admin can't be classified for (e.g. a not-yet-added tool name) is denied.
// Mirrors requiredScopeFor's default-deny arm — a forgotten admin meta-tool must
// not leak to tenants (RFC AG §5).
func TestPrincipalMayCallTool_DenyByDefault(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"substrate:tenant"}})
	if principalMayCallTool(ctx, "some_future_unclassified_admin_tool") {
		t.Errorf("an unclassified tool must default to admin-only for a tenant principal (deny-by-default)")
	}
}
