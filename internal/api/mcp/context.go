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
		// RFC BX P2b defense-in-depth: stamp the isolation bit server-side from
		// the principal, exactly like every HTTP run-start site (s.isolatedForCtx)
		// — so ConfineIsolatedScope still refuses tenant/global data scopes if an
		// isolated (substrate:user-topped) principal ever reaches this MCP-direct
		// path. Today the /v1/_mcp route gate is substrate:tenant, which a
		// substrate:user token cannot satisfy, so this is unreachable now; stamping
		// it keeps the invariant true if that gate is ever widened.
		Isolated: auth.IsIsolated(p, ok),
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
	// No principal reaches here (mcpPrincipalCtx routes an authenticated
	// caller elsewhere): this is the stdio launcher or an open-mode session,
	// i.e. the operator, so the def tools grant it operator authority.
	ctx = tools.WithUnauthenticatedOperator(ctx)
	return grantOperatorPolicies(ctx, operatorAgentName, true)
}

// grantOperatorPolicies attaches the builtin-tool capability policies for an
// MCP-direct caller (identity must already be stamped). Def-family scopes are
// "any" for BOTH admin and tenant — tenant confinement is the TenantID stamp +
// the lookup's tenant filter, not a def-scope narrowing (RFC AG §3.1). The
// OPERATOR PLANE (OperatorTokenDef mint, cross-tenant history) is granted ONLY
// when isAdmin; a non-admin (tenant) principal leaves those zero (default-deny)
// — so even if such a tool were reached it refuses. Evaluation is not decided
// by isAdmin: it takes tools.OffRunEvaluationPolicy, as HTTP and gRPC do.
func grantOperatorPolicies(ctx context.Context, agentName string, isAdmin bool) context.Context {
	// The operator-only def families: an operator plane's own authority, which
	// no run holds (the spawn path strips it).
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})
	ctx = tools.WithDocumentSourceDefPolicy(ctx, tools.DocumentSourceDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName})

	// Evaluation: the grant every off-run transport shares, decided from the
	// token rather than isAdmin — a tenant operator gets every op here as it
	// does on HTTP and gRPC, and the tool confines it to its own tenant.
	p, hasPrincipal := auth.PrincipalFromContext(ctx)

	// RFC BE History: a tenant principal gets the own-tenant scopes but NOT
	// `global` — the tenant stamp on ctx + the tool's fold confine every op to
	// this tenant/subject. The operator plane has full owner-scope reach
	// INCLUDING cross-tenant `global`.
	histScopes := []string{"self", "user", "tenant"}
	if isAdmin {
		histScopes = append(histScopes, "global")
		// Operator plane. A tenant principal's mint plane stays default-deny (zero).
		ctx = tools.WithOperatorTokenDefPolicy(ctx, tools.OperatorTokenDefPolicyValue{Admin: true})
	}
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		// AgentTools wildcard ceiling (F11): without it, agentdef/skilldef create
		// with a non-empty tools overlay refuses ("caller's effective
		// tools not on ctx"). Operator-trust → the same wildcard the HTTP
		// /v1/_agentdef admin path uses (substrateAdminCtx).
		rp.Tools = []string{"*"}
		// Memory: full scope access (QuotaBytes=0 → global default cap). RFC BL P2:
		// the operator plane also gets the consolidation grant — a session with open
		// memory scope can drive the cursor/pending/supersede ops on its OWN
		// (tenant-confined) scopes, consistent with the wildcard posture here.
		//
		// It does NOT get the origin=consolidator provenance stamp: that requires an
		// actual run, and this plane has no run id. An operator writing a fact by hand
		// is not a machine distilling one from a transcript, and the column has to stay
		// a trustworthy filter for the latter. See builtin.provenanceForSet.
		//
		// `tenant` (RFC BL P4b) is granted here for BOTH admin and tenant principals.
		// This plane IS the operator — a substrate:tenant token is the tenant operator,
		// with full power inside its own tenant — and curating tenant-shared reference
		// material is exactly what it is for. Confinement is the TenantID stamped from
		// the principal, so a tenant token reaches only ITS tenant's tenant scope; the
		// scope list is not what keeps tenants apart.
		//
		// It is deliberately WIDER than an ordinary agent's grant, because the trust is
		// different in kind: an agent gets `tenant` only when an operator lists it in
		// yaml, whereas this session is the operator, authenticated as themselves.
		rp.Memory = tools.MemoryPolicyValue{
			AllowedScopes: []string{"agent", "user", "tenant", "global"},
			Consolidation: true,
		}
		// SQL Memory: never stamped before, which meant EVERY sql_* op over MCP failed
		// default-deny with "this agent has no sql_scopes configured" — at any scope,
		// since v1.2.0. Verified against a live deployment rather than inferred.
		//
		// Documents hid the gap: the Document tool talks to sqlmem directly instead of
		// through the Memory tool's policy check, so document ops over MCP worked while
		// the SQL ops beside them did not.
		//
		// `run` is omitted on purpose: an MCP-direct call has no run, so granting it
		// would trade a clear "not configured" refusal for a confusing "requires an
		// active run" one.
		rp.SqlMem = tools.SqlMemPolicyValue{
			AllowedScopes: []string{"agent", "user", "tenant"},
		}
		// Channel: unrestricted, said with the discriminator rather than a "*" in
		// the allowlist. The matcher supports an exact name and a trailing "/*"
		// prefix only, so the old []string{"*"} matched NO channel and this plane
		// silently held no channel authority at all.
		rp.Channel = tools.ChannelPolicyValue{
			AllPublish:   true,
			AllSubscribe: true,
		}
		// Def families: "any" name within the stamped tenant (same for admin +
		// tenant; the tenant filter at the lookup confines a tenant principal).
		rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}, SelfName: agentName}
		rp.Skill = tools.SkillPolicyValue{Granted: true} // every skill
		rp.VolumeDef = tools.VolumeDefPolicyValue{Scopes: []string{"any"}}
		rp.Evaluation = tools.OffRunEvaluationPolicy(p, hasPrincipal)
		rp.History = tools.HistoryPolicyValue{Scopes: histScopes}
	})
	return ctx
}
