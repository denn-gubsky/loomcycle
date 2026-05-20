package mcp

import (
	"context"

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
// operatorCtx path applies ONLY to direct builtin-tool dispatch from
// the MCP wire.
const (
	// operatorAgentName is the synthetic agent name attached to ctx
	// for MCP-direct builtin invocations. Memory ops with scope=agent
	// use this as the scope_id; Channel ops similarly. Distinct from
	// realistic user agent names so operator-direct memory doesn't
	// collide with any production agent's keys.
	operatorAgentName = "mcp-operator"

	// operatorUserID + operatorAgentID populate the synthetic
	// RunIdentityValue. memory scope=user resolves to this; the
	// AgentDefPolicy's SelfName matches operatorAgentName.
	operatorUserID  = "mcp-operator"
	operatorAgentID = "a_mcp-operator"
)

// operatorCtx enriches the supplied ctx with the policy values required
// for MCP-direct builtin-tool invocations. Without these, every call to
// Memory / Channel / AgentDef / Evaluation / Context fails with
// default-deny refusals because the underlying tools check per-agent
// policy from ctx and find zero values.
//
// Use this for the 6 builtin-wrapper handlers ONLY. Do NOT use it for
// run-lifecycle handlers (spawn_run / cancel_run / etc.) — those go
// through RunOnce which builds the right ctx for each agent from
// cfg.Agents.
func operatorCtx(ctx context.Context) context.Context {
	// Identity. Synthetic but consistent — repeated MCP calls share
	// the same scope_id for scope=agent memory and the same UserID
	// for scope=user. Operators can audit by querying memory rows
	// where scope_id = "mcp-operator".
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:  operatorUserID,
		AgentID: operatorAgentID,
	})
	ctx = tools.WithAgentName(ctx, operatorAgentName)

	// Memory: full scope access. QuotaBytes=0 falls back to the
	// global default (LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES) — operators
	// who want a tighter cap on MCP-direct writes can set the env.
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "global"},
	})

	// Channel: open ACL. "*" wildcard matches every channel name.
	// Channels map is left nil — the tool falls back to defaults
	// (TTL=0, max_messages=0=unbounded, scope=agent) for channels
	// not in operator yaml. Operators wanting fine-grained MCP
	// channel control should declare the channels in `channels:`
	// (which populates ChannelPolicy.Channels per-run, but for
	// MCP-direct we synthesise — see channel.go for the resolve).
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"*"},
		Subscribe: []string{"*"},
	})

	// AgentDef: "any" scope — operators may mutate any def. SelfName
	// matches the synthetic agent name so the (rarely used) "self"
	// scope check still resolves correctly if an operator later
	// narrows the policy in code.
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: operatorAgentName,
	})

	// SkillDef (v0.8.22): "any" scope, same operator-trust grant as
	// AgentDef. Skills have no agent identity, so no SelfName field.
	ctx = tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{
		Scopes: []string{"any"},
	})

	// Evaluation: all 4 valid scope values. submit_any + read_any
	// are the load-bearing ones; submit_self + submit_descendants
	// are included for completeness in case an operator wants the
	// "self" path explicitly.
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{
		Scopes: []string{"submit_self", "submit_descendants", "submit_any", "read_any"},
	})

	// Context.history: "any" — read every agent's transcript. This
	// is the operator-trust grant the policy comment describes as
	// "admin/debug only" — exactly the MCP operator's role.
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{
		Scopes: []string{"any"},
	})

	return ctx
}
