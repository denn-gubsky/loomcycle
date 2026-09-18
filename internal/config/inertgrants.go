package config

import "fmt"

// InertToolGrant is one tool an agent holds and cannot use: the capability gate
// that tool reads grants nothing.
type InertToolGrant struct {
	Tool    string // as named in the agent's `tools` list
	Gate    string // the yaml key that governs it
	Message string // names the tool, the gate, and what to set
}

// inertGateForTool lists the tools whose gate still grants NOTHING when empty.
//
// ⚠️ THIS IS DELIBERATELY NOT EVERY GATED TOOL. memory_scopes, history_scope,
// sql_scopes and evaluation_scopes now resolve to what the caller already owns,
// so an unset one is not inert — reporting it would put a warning on every run
// of most agents, which is how a signal becomes noise and then becomes ignored.
//
// It is also not every tool with a gate: Context's history_scope and Memory's
// sql_scopes are per-OP gates inside multi-op tools, so the tool works without
// them for its other ops and a bare tool-presence check would false-positive.
// agentGateWarnings documents the same exclusions for the same reason.
var inertGateForTool = []struct {
	tool, gate, fix string
}{
	{"AgentDef", "agent_def_scopes", "e.g. [self] or [any]"},
	{"ScheduleDef", "schedule_def_scopes", "e.g. [any]"},
	{"VolumeDef", "volume_def_scopes", "e.g. [any] or [named:<volume>]"},
	{"A2AServerCardDef", "a2a_server_card_def_scopes", "e.g. [any]"},
	{"A2AAgentDef", "a2a_agent_def_scopes", "e.g. [any]"},
}

// InertToolGrants returns the tools this agent holds whose gate is empty, in a
// stable order.
//
// Channel is handled separately because its gate is two lists rather than one,
// and an agent with publish but no subscribe is half-granted rather than inert.
func InertToolGrants(a AgentDef) []InertToolGrant {
	has := func(tool string) bool {
		for _, t := range a.Tools {
			if t == tool {
				return true
			}
		}
		return false
	}
	gate := func(name string) []string {
		switch name {
		case "agent_def_scopes":
			return a.AgentDefScopes
		case "schedule_def_scopes":
			return a.ScheduleDefScopes
		case "volume_def_scopes":
			return a.VolumeDefScopes
		case "a2a_server_card_def_scopes":
			return a.A2AServerCardDefScopes
		case "a2a_agent_def_scopes":
			return a.A2AAgentDefScopes
		}
		return nil
	}

	var out []InertToolGrant
	for _, g := range inertGateForTool {
		if !has(g.tool) || len(gate(g.gate)) > 0 {
			continue
		}
		out = append(out, InertToolGrant{
			Tool: g.tool, Gate: g.gate,
			Message: fmt.Sprintf("`%s` is in this agent's tools but `%s:` is unset, so every %s call is refused. Set `%s:` (%s), or drop the tool.",
				g.tool, g.gate, g.tool, g.gate, g.fix),
		})
	}
	// Channel: granted, but neither side of the ACL carries anything.
	if has("Channel") && len(a.Channels.Publish) == 0 && len(a.Channels.Subscribe) == 0 {
		out = append(out, InertToolGrant{
			Tool: "Channel", Gate: "channels",
			Message: "`Channel` is in this agent's tools but `channels.publish` and `channels.subscribe` are both empty, so every Channel call is refused. Set one, or drop the tool.",
		})
	}
	return out
}
