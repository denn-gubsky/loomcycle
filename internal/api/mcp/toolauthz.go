package mcp

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// mcpErrForbidden is the JSON-RPC error code returned when a principal calls a
// meta-tool its scopes don't permit. It sits in the implementation-defined
// server-error range (-32000..-32099) so a client can distinguish "forbidden"
// from -32601 "unknown tool" and -32603 "internal error".
const mcpErrForbidden = -32001

// tenantConfinableTools is the explicit allowlist of /v1/_mcp meta-tools a
// non-admin (substrate:tenant) principal may list + call. Membership means the
// underlying tool keys on RunIdentity.TenantID — so the mcpPrincipalCtx tenant
// stamp plus the tool's own tenant filter confine it — NOT that this map grants
// any capability itself.
//
// A tool ABSENT from this set requires substrate:admin: deny-by-default, so a
// newly added meta-tool is admin-only until it is explicitly classified
// tenant-safe here (RFC AG §2 / §5). This mirrors requiredScopeFor's
// default-deny arm on the HTTP plane — the enforcement-correctness lives in this
// map, not the router, so an unclassified tool must fail closed.
//
// The /v1/_mcp route gate is substrate:tenant (RFC AG Phase 2), so a tenant
// token can open a session — and this map is the load-bearing per-operation
// authz that keeps it confined: a tenant session lists + may call only these
// tools; the admin-only ones are filtered out + 403'd by principalMayCallTool.
var tenantConfinableTools = map[string]bool{
	// Run lifecycle — tenant flows via the run identity (applyPrincipal on
	// the wire identity lands in RFC AG Phase 1).
	"spawn_run":   true,
	"spawn_runs":  true,
	"cancel_run":  true,
	"get_run":     true,
	"compact_run": true,
	"list_runs":   true,

	// Agent management.
	"register_agent":   true,
	"unregister_agent": true,
	"list_agents":      true,

	// Def authoring — each stamps the row's tenant from ctx and opaque-404s
	// cross-tenant reads.
	"agentdef":         true,
	"skilldef":         true,
	"teamdef":          true, // RFC AP — tenant-confined team-workflow substrate
	"mcpserverdef":     true,
	"scheduledef":      true,
	"a2aservercarddef": true,
	"a2aagentdef":      true,
	"webhookdef":       true,
	"memorybackenddef": true,
	"volumedef":        true,
	"credentialdef":    true, // RFC AR — tenant/user-confined secure credential store

	// Per-(scope, scope_id, tenant) data tools.
	"memory":     true,
	"channel":    true,
	"channeldef": true,
	"path":       true,
	"document":   true,

	// Per-run / per-user — tenant inherited; the underlying tool applies its
	// own own-subject / cross-tenant-404 gate.
	"evaluation":             true,
	"context":                true,
	"interruption_resolve":   true,
	"publish_channel":        true,
	"subscribe_channel":      true,
	"peek_channel":           true,
	"ack_channel":            true,
	"stream_user_run_states": true,

	// Hook management — RFC AG Phase 2 promotion. The connector's hook methods
	// derive the owning tenant from the principal on ctx (tenantScopeFromCtx):
	// register stamps the tenant, list/delete are tenant-scoped (opaque-404
	// cross-tenant), and a tenant-scoped hook fires only on its own tenant's runs.
	// So a tenant operator manages ONLY its own hooks — same posture as the
	// already-ScopeTenant HTTP /v1/hooks routes. The privileged host-WIDEN
	// capability stays gated by the operator-yaml owner allowlist, not this map.
	"register_hook": true,
	"list_hooks":    true,
	"delete_hook":   true,
}

// adminOnlyTools enumerates the runtime-global / operator-plane meta-tools that
// have NO tenant dimension and so cannot be confined — they stay admin-only
// (RFC AG §2). The gate does NOT consult this set (it relies on
// tenantConfinableTools + deny-by-default); it exists so the drift test can
// assert every dispatchable tool is *consciously* classified as one or the
// other, and that the two sets are disjoint. Adding a meta-tool without
// classifying it here turns the drift test red.
var adminOnlyTools = map[string]bool{
	"operatortokendef": true, // token minting — no tenant dimension.

	// Runtime-global control + introspection.
	"pause_runtime":     true,
	"resume_runtime":    true,
	"get_runtime_state": true,
	"resolve_probe":     true,

	// Snapshots capture / restore cross-tenant state.
	"create_snapshot":  true,
	"list_snapshots":   true,
	"get_snapshot":     true,
	"export_snapshot":  true,
	"restore_snapshot": true,
	"delete_snapshot":  true,

	// Operator aggregate over every scope.
	"list_channels": true,
}

// principalMayCallTool reports whether the principal on ctx may list/invoke
// toolName over the /v1/_mcp transport (RFC AG §3.3):
//
//   - No principal (stdio / open mode): process-local operator-trust → every tool.
//   - Admin principal (substrate:admin, incl. the legacy token): every tool.
//   - Non-admin (substrate:tenant) principal: only the tenant-confinable
//     allowlist; everything else — including an unclassified new tool — is
//     admin-only (deny-by-default).
func principalMayCallTool(ctx context.Context, toolName string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true
	}
	if auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	return tenantConfinableTools[toolName]
}
