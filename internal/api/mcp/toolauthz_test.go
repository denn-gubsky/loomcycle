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
	denied := []string{"operatortokendef", "restore_snapshot", "pause_runtime", "get_runtime_state", "list_channels"}
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

// member is a non-isolated token without substrate:tenant: the kind of
// principal the /v1/_mcp route admits on its member path whatever it was
// granted, so the per-tool gate is the only thing holding it to its scopes.
func member(scopes ...string) context.Context {
	return auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "bob", Scopes: scopes})
}

// TestPrincipalMayCallTool_AMemberNeedsTheToolsOwnScope: a token is held to
// what it was granted. A read-only member cannot start, steer or cancel a run
// over MCP, as it cannot over HTTP or gRPC.
func TestPrincipalMayCallTool_AMemberNeedsTheToolsOwnScope(t *testing.T) {
	runWrites := []string{"spawn_run", "spawn_runs", "cancel_run", "compact_run", "retune_run", "review_run", "configured_run", "interruption_resolve"}
	runReads := []string{"get_run", "list_runs", "stream_user_run_states"}
	chanWrites := []string{"publish_channel", "ack_channel"}
	chanReads := []string{"subscribe_channel", "peek_channel", "list_team_channels", "peek_team_channel"}

	for _, c := range []struct {
		name    string
		scopes  []string
		allowed [][]string
		refused [][]string
	}{
		{"runs:read only", []string{auth.ScopeRunsRead}, [][]string{runReads}, [][]string{runWrites, chanWrites, chanReads}},
		{"runs:create only", []string{auth.ScopeRunsCreate}, [][]string{runWrites}, [][]string{runReads, chanWrites, chanReads}},
		{"channel:read only", []string{auth.ScopeChannelRead}, [][]string{chanReads}, [][]string{runWrites, runReads, chanWrites}},
		{"channel:publish only", []string{auth.ScopeChannelPublish}, [][]string{chanWrites}, [][]string{runWrites, runReads, chanReads}},
		{"no scope at all", nil, nil, [][]string{runWrites, runReads, chanWrites, chanReads}},
		{"substrate:tenant", []string{auth.ScopeTenant}, [][]string{runWrites, runReads, chanWrites, chanReads}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := member(c.scopes...)
			for _, group := range c.allowed {
				for _, tool := range group {
					if !principalMayCallTool(ctx, tool) {
						t.Errorf("%q must be allowed", tool)
					}
				}
			}
			for _, group := range c.refused {
				for _, tool := range group {
					if principalMayCallTool(ctx, tool) {
						t.Errorf("%q must be refused", tool)
					}
				}
			}
			// A tool with no scope of its own stays governed by the allowlist.
			if !principalMayCallTool(ctx, "document") {
				t.Errorf("a member must keep the data tools, whatever its scopes")
			}
		})
	}
}

// TestPrincipalMayCallTool_AnIsolatedUserIsNotWidened: an isolated user's
// scope implies runs:create and runs:read, and must still reach nothing but
// its self-service tool. The scope check narrows; it never admits.
func TestPrincipalMayCallTool_AnIsolatedUserIsNotWidened(t *testing.T) {
	ctx := member(auth.ScopeUser)
	for tool := range toolRequiredScope {
		if principalMayCallTool(ctx, tool) {
			t.Errorf("an isolated user must not reach %q", tool)
		}
	}
	if !principalMayCallTool(ctx, "credentialdef") {
		t.Errorf("an isolated user must keep credentialdef")
	}
}

// TestToolRequiredScope_NamesRealTenantTools: an entry for a tool that is not
// dispatched, or that a member could not reach anyway, guards nothing and
// hides that a rename left the real tool ungated.
func TestToolRequiredScope_NamesRealTenantTools(t *testing.T) {
	for tool, scope := range toolRequiredScope {
		if _, ok := handlersByName[tool]; !ok {
			t.Errorf("toolRequiredScope has %q, which is not a dispatchable tool", tool)
		}
		if !tenantConfinableTools[tool] {
			t.Errorf("toolRequiredScope has %q, which is admin-only and needs no scope of its own", tool)
		}
		if !auth.ValidScope(scope) {
			t.Errorf("toolRequiredScope[%q] = %q, which is not a scope", tool, scope)
		}
	}
}
