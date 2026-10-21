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
// It is also not every tool with a gate: Context's history_scope, Memory's
// sql_scopes and Crypto's crypto_scopes are per-OP gates inside multi-op tools,
// so the tool works without them for its other ops and a bare tool-presence
// check would false-positive. agentGateWarnings documents the same exclusions
// for the same reason; cryptoGateWarnings says at boot what an unset
// crypto_scopes does leave out.
var inertGateForTool = []struct {
	tool, gate, fix string
	// operatorOnly marks a tool that is never given to an agent's run: it is
	// reached only through the operator surfaces, and no run path grants its
	// gate. Listing it is inert whatever the gate says, so the advice is to
	// drop it, not to set the gate.
	operatorOnly bool
}{
	{"AgentDef", "agent_def_scopes", "e.g. [self] or [any]", false},
	{"ScheduleDef", "schedule_def_scopes", "", true},
	{"VolumeDef", "volume_def_scopes", "e.g. [any] or [named:<volume>]", false},
	{"A2AServerCardDef", "a2a_server_card_def_scopes", "", true},
	{"A2AAgentDef", "a2a_agent_def_scopes", "", true},
}

// OperatorOnlyToolMessage is the advisory for an agent that lists a tool only
// the operator surfaces reach.
func OperatorOnlyToolMessage(tool, gate string) string {
	return fmt.Sprintf("`%s` is in this agent's tools, but %s is not available to agents: it is reached only through the operator surfaces (HTTP, gRPC, MCP, the Web UI), and `%s:` does not change that. Drop the tool.",
		tool, tool, gate)
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
		if g.operatorOnly && has(g.tool) {
			out = append(out, InertToolGrant{Tool: g.tool, Gate: g.gate, Message: OperatorOnlyToolMessage(g.tool, g.gate)})
			continue
		}
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

// CryptoToolName is the name the Crypto tool registers under. Declared here
// because the boot advisories below key off it and config imports no tool
// package; tools.CryptoToolName is this constant.
const CryptoToolName = "Crypto"

// cryptoGateWarnings returns the boot advisories for the Crypto grant, in both
// directions, because each half of it is useless without the other:
//
//   - the tool listed with no crypto_scopes: NOT inert — the keyless ops
//     (hashing, random bytes) work — but every key-backed op refuses, which is
//     the part an operator listing the tool most likely wanted;
//   - crypto_scopes set with the tool not listed: the grant reaches nothing,
//     since no tool reads it.
//
// It is a boot advisory and not an InertToolGrants entry for the first reason:
// a run-start "capability inert" event would claim every Crypto call refuses,
// which is false.
func cryptoGateWarnings(name string, a AgentDef, hasTool bool) []string {
	granted := len(a.CryptoScopes) > 0 && !(len(a.CryptoScopes) == 1 && a.CryptoScopes[0] == cryptoDenyAll)
	switch {
	case hasTool && len(a.CryptoScopes) == 0:
		return []string{fmt.Sprintf("agent %q: tools includes %s but crypto_scopes is empty — only the keyless %s ops will work and every key-backed op will default-deny; set crypto_scopes (e.g. [agent] or [user]), or [\"-*\"] to say keyless-only is intended", name, CryptoToolName, CryptoToolName)}
	case !hasTool && granted:
		return []string{fmt.Sprintf("agent %q: crypto_scopes is set but %s is not in tools — the grant reaches nothing; add %s to tools, or drop crypto_scopes", name, CryptoToolName, CryptoToolName)}
	}
	return nil
}
