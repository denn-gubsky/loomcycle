package mcp

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Operator-trust constants. The stdio MCP server runs in the operator's
// process — when Claude Code (or any other MCP orchestrator) spawns
// `loomcycle mcp ...`, the operator IS the launcher and has full process
// authority. Builtin tools (Memory, Channel, AgentDef, SkillDef, Evaluation,
// Context) gate on per-agent ACLs that don't exist for MCP-direct
// callers — there's no yaml agent definition behind a `tools/call`
// invocation. We synthesise a permissive policy that mirrors operator
// access, with a stable synthetic agent name so memory/channel ops that
// require `scope_id` derivation (scope=agent) have a deterministic key.
//
// This is intentionally distinct from sub-agent runs spawned via
// `spawn_run` — those carry the requested agent's real policies because
// they go through RunOnce, which builds ctx from cfg.Agents. The
// operatorCtx / mcpPrincipalCtx path applies ONLY to direct builtin-tool
// dispatch from the MCP wire.
const (
	// operatorAgentName is the synthetic agent name attached to ctx
	// for MCP-direct builtin invocations. Memory ops with scope=agent
	// use this as the scope_id; Channel ops similarly. Distinct from
	// realistic user agent names so operator-direct memory doesn't
	// collide with any production agent's keys.
	operatorAgentName = "mcp-operator"

	// operatorUserID + operatorAgentID populate the synthetic
	// RunIdentityValue for the no-principal (stdio / open-mode) path.
	// AUTHENTICATED MCP calls use principal.Subject as the UserID
	// instead — see mcpPrincipalCtx (RFC AG §3.1a).
	operatorUserID  = "mcp-operator"
	operatorAgentID = "a_mcp-operator"
)

// mcpPrincipalCtx enriches ctx for an MCP-direct builtin invocation, keyed off
// the AUTHENTICATED principal (RFC AG). It is the per-principal replacement for
// operatorCtx in wrapBuiltin:
//
//   - No principal (stdio / open mode): process-local operator, nothing to
//     align with → operatorCtx, byte-identical to the historical behavior.
//   - Authenticated principal (legacy / minted / config-declared): identity is
//     the principal's OWN (UserID = principal.Subject, TenantID =
//     principal.TenantID), so USER-SCOPED tools (document / memory / path) key
//     on the same id the off-run HTTP path (substrateAdminUserCtx) uses
//     (§3.1a — the fix for the RFC AM Document-Assistant mismatch, where an
//     MCP-created document landed under the synthetic "mcp-operator" user and
//     was invisible in the Web UI). The agent-scope scope_id stays the
//     synthetic operatorAgentName, so agent-scoped Memory/defs keep their
//     determinism. The operator plane (mint / cross-tenant) is admin-only.
//
// The /v1/_mcp route gate is substrate:tenant (RFC AG Phase 2), so a non-admin
// tenant principal does reach here; its branch stamps the tenant + withholds the
// operator plane, and the per-tool gate (principalMayCallTool) keeps it off the
// admin-only meta-tools.
func mcpPrincipalCtx(ctx context.Context) context.Context {
	p, ok := auth.PrincipalFromContext(ctx)
	// No principal (stdio / open mode), or a zero-Subject principal we can't
	// key on: fall back to the synthetic operator identity. Mirrors the HTTP
	// off-run path's substrateAdminUserCtx guard so the two stay in lockstep.
	if !ok || p.Subject == "" {
		return operatorCtx(ctx)
	}
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:   p.Subject,
		AgentID:  operatorAgentID,
		TenantID: p.TenantID,
	})
	ctx = tools.WithAgentName(ctx, operatorAgentName)
	return grantOperatorPolicies(ctx, operatorAgentName, auth.HasScope(p.Scopes, auth.ScopeAdmin))
}

// operatorCtx is the no-principal (stdio / open-mode) path: a full-operator
// context with the synthetic identity. Authenticated MCP calls go through
// mcpPrincipalCtx instead. Without these policies every call to Memory /
// Channel / AgentDef / Evaluation / Context fails default-deny (the underlying
// tools read per-agent policy from ctx and find zero values).
func operatorCtx(ctx context.Context) context.Context {
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:  operatorUserID,
		AgentID: operatorAgentID,
	})
	ctx = tools.WithAgentName(ctx, operatorAgentName)
	return grantOperatorPolicies(ctx, operatorAgentName, true)
}

// grantOperatorPolicies attaches the builtin-tool capability policies for an
// MCP-direct caller (identity must already be stamped). Def-family scopes are
// "any" for BOTH admin and tenant — tenant confinement is the TenantID stamp +
// the lookup's tenant filter, not a def-scope narrowing (RFC AG §3.1). The
// OPERATOR PLANE (OperatorTokenDef mint, cross-agent Evaluation, cross-tenant
// history) is granted ONLY when isAdmin; a non-admin (tenant) principal leaves
// those zero (default-deny) — so even if such a tool were reached it refuses.
func grantOperatorPolicies(ctx context.Context, agentName string, isAdmin bool) context.Context {
	// AgentTools wildcard ceiling (F11): without it, agentdef/skilldef create
	// with a non-empty allowed_tools overlay refuses ("caller's effective
	// allowed_tools not on ctx"). Operator-trust → the same wildcard the HTTP
	// /v1/_agentdef admin path uses (substrateAdminCtx).
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	// Memory: full scope access (QuotaBytes=0 → global default cap).
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "global"},
	})
	// Channel: open ACL ("*" matches every channel name).
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"*"},
		Subscribe: []string{"*"},
	})

	// Def families: "any" name within the stamped tenant (same for admin +
	// tenant; the tenant filter at the lookup confines a tenant principal).
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{Scopes: []string{"any"}})
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: []string{"any"}})

	if isAdmin {
		// Operator plane.
		ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{
			Scopes: []string{"submit_self", "submit_descendants", "submit_any", "read_any"},
		})
		ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"any"}})
		ctx = tools.WithOperatorTokenDefPolicy(ctx, tools.OperatorTokenDefPolicyValue{Admin: true})
	} else {
		// Tenant principal: own-only evaluation; cross-tenant history + the
		// mint plane stay default-deny (zero).
		ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{Scopes: []string{"submit_self"}})
	}
	return ctx
}
