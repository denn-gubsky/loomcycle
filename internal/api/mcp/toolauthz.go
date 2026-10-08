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
	// directory: a read-only derived view of who is in the caller's OWN tenant.
	// Tenant-confinable because the handler takes the tenant from the PRINCIPAL and
	// offers no wire field, so a tenant session cannot inspect another tenant's
	// subject. Its op=tenants sub-op is separately refused for a non-admin inside
	// the handler — that one op IS cross-tenant, and the tool is listed here rather
	// than in adminOnlyTools so the useful ops stay reachable.
	"directory": true,
	// erasure: a data-subject report/erasure confined to the caller's own tenant.
	// Tenant-confinable rather than admin-only because the handler takes the
	// tenant from the PRINCIPAL and offers no wire field for it, so a tenant
	// session physically cannot reach another tenant's subject — the same posture
	// as credentialdef. The destructive half is guarded by dry_run defaulting true
	// plus a confirm that must equal the subject, not by the scope.
	"erasure": true,
	// Run lifecycle — tenant flows via the run identity (applyPrincipal on
	// the wire identity lands in RFC AG Phase 1).
	"spawn_run":   true,
	"spawn_runs":  true,
	"cancel_run":  true,
	"get_run":     true,
	"compact_run": true,
	"retune_run":  true,
	// review_run: the connector gates the verdict on the run's tenant and
	// session owner, like steering.
	"review_run": true,
	// configured_run: create / edit / start / discard a draft; every op goes
	// through the connector's tenant gate and keeps the draft's identity.
	"configured_run": true,
	"list_runs":      true,

	// Agent management.
	"register_agent":   true,
	"unregister_agent": true,
	"list_agents":      true,

	// Def authoring — each stamps the row's tenant from ctx and opaque-404s
	// cross-tenant reads.
	"agentdef":          true,
	"skilldef":          true,
	"teamdef":           true, // RFC AP — tenant-confined team-workflow substrate
	"hookdef":           true, // tenant-confined reusable hook definitions
	"mcpserverdef":      true,
	"scheduledef":       true,
	"a2aservercarddef":  true,
	"a2aagentdef":       true,
	"webhookdef":        true,
	"memorybackenddef":  true,
	"documentsourcedef": true, // RFC CE — tenant-confined remote-document-source substrate
	"volumedef":         true,
	"credentialdef":     true, // RFC AR — tenant/user-confined secure credential store

	// Per-(scope, scope_id, tenant) data tools.
	"memory":     true,
	"channel":    true,
	"channeldef": true,
	"path":       true,
	"document":   true,
	// RFC BE History — the ctx tenant stamp + the tool's own scope fold confine
	// a tenant session to its own tenant; the cross-tenant `global` scope is
	// refused by the admin-gated history policy (grantOperatorPolicies).
	"history": true,
	// decision: it holds no tenant's data (it reads only the state passed in the
	// call), and the connector resolves the provider key and the bill for the
	// caller's own principal. It spends tokens, so it additionally needs the
	// scope that creates a run (toolRequiredScope).
	"decision": true,

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

// userSelfServiceTools is the RFC CN allowlist for an ISOLATED substrate:user
// session: the ONLY meta-tools an isolated user may list + call over /v1/_mcp. An
// isolated user is admitted to open a session solely to self-serve its OWN
// scope=user credentials from the `loomcycle mcp` thin client; the credentialdef
// dispatch applies the scope=user constraint (credential.ConstrainToUserScope), so
// this session grants nothing beyond the user's own tokens. Every entry MUST also
// be in tenantConfinableTools (an isolated user is a strict subset of a tenant
// session's surface); the drift test asserts that.
var userSelfServiceTools = map[string]bool{
	"credentialdef": true, // RFC CN — a user's own scope=user credential store.
}

// toolRequiredScope is a scope a principal must hold to list + call a tool, ON
// TOP of its class's allowlist above. The allowlists say which tools a class of
// principal can be confined on; they do not look at what the token was granted.
// A non-isolated member token reaches /v1/_mcp whatever its scopes are (the
// route's member path), so without this a token minted with only runs:read
// could start, steer and cancel runs here, or spend tokens on a decision, while
// POST /v1/runs and the Run RPC refuse it.
//
// Each entry is the scope the tool's gRPC twin needs (grpcConsumerScopes), which
// mirrors the HTTP route's. A tool with no entry is governed by its allowlist
// alone: the def-authoring and data tools, whose HTTP routes a member also
// reaches on the member path.
var toolRequiredScope = map[string]string{
	// Starting a run, and every write on a run's state.
	"spawn_run":            auth.ScopeRunsCreate,
	"spawn_runs":           auth.ScopeRunsCreate,
	"cancel_run":           auth.ScopeRunsCreate,
	"compact_run":          auth.ScopeRunsCreate,
	"retune_run":           auth.ScopeRunsCreate,
	"review_run":           auth.ScopeRunsCreate,
	"configured_run":       auth.ScopeRunsCreate,
	"interruption_resolve": auth.ScopeRunsCreate,
	// Reading runs.
	"get_run":                auth.ScopeRunsRead,
	"list_runs":              auth.ScopeRunsRead,
	"stream_user_run_states": auth.ScopeRunsRead,
	// The per-user channel surface.
	"publish_channel":   auth.ScopeChannelPublish,
	"ack_channel":       auth.ScopeChannelPublish,
	"subscribe_channel": auth.ScopeChannelRead,
	"peek_channel":      auth.ScopeChannelRead,
	// Asking a decision model outside a run spends tokens: the scope
	// POST /v1/_decide and the Decide RPC require.
	"decision": auth.ScopeRunsCreate,
}

// principalMayCallTool reports whether the principal on ctx may list/invoke
// toolName over the /v1/_mcp transport (RFC AG §3.3):
//
//   - No principal (stdio / open mode): process-local operator-trust → every tool.
//   - Admin principal (substrate:admin, incl. the legacy token): every tool.
//   - Isolated substrate:user principal (RFC CN): only the user self-service
//     allowlist — everything else, including the tenant-confinable tools, stays
//     closed.
//   - Non-admin, non-isolated (substrate:tenant / RFC CB member) principal: the
//     tenant-confinable allowlist; everything else — including an unclassified new
//     tool — is admin-only (deny-by-default).
//   - Any non-admin principal additionally needs a tool's toolRequiredScope,
//     when it has one.
func principalMayCallTool(ctx context.Context, toolName string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true
	}
	if auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	if required, gated := toolRequiredScope[toolName]; gated && !auth.HasScope(p.Scopes, required) {
		return false
	}
	if auth.IsIsolated(p, ok) {
		return userSelfServiceTools[toolName]
	}
	return tenantConfinableTools[toolName]
}

// scopeNeededFor names the scope a refused principal lacks for toolName, for
// the refusal's text: the tool's own required scope when that is what failed,
// else substrate:admin (the tool is outside the principal's allowlist).
func scopeNeededFor(ctx context.Context, toolName string) string {
	if required, gated := toolRequiredScope[toolName]; gated {
		if p, ok := auth.PrincipalFromContext(ctx); ok && !auth.HasScope(p.Scopes, required) {
			return required
		}
	}
	return auth.ScopeAdmin
}
