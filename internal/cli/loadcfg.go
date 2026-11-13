package cli

import (
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// byTierProvider is what the introspection commands print for an agent whose
// provider is chosen at run time. Not a provider id, and deliberately shaped so
// it cannot be mistaken for one.
const byTierProvider = "(by tier)"

// agentModelForDisplay reports how an agent will get its provider and model, for
// the introspection commands (validate, agents list).
//
// THREE outcomes, not two, and collapsing them into two is the bug this exists
// for. config.ResolveAgentModel answers the PIN path: an agent naming a provider
// or model (or an alias for one) resolves here and now. An agent naming a `tier:`
// is resolved at RUN time by the resolver against the live availability matrix,
// which a CLI process has no access to — so the pin path legitimately finds
// nothing for it. Reporting that as a config error made `loomcycle validate` exit
// 2 on every bundled agent preset (chat, memory, document-agent, agent-teams)
// naming an agent the running server resolves fine. The exit code was the small
// half: validate returns at the FIRST agent, so MCP servers and everything after
// went unchecked, and `agents list --json` bailed mid-array leaving output no
// parser accepts.
//
// TIER IS CHECKED FIRST, mirroring the runtime — internal/api/http.resolveAgentDef
// takes the tier path before it ever looks at a pin or at `defaults:`. Ordering it
// the other way (pin path first, tier only when that errors) looks equivalent and
// is not: for a tier agent in a config that also sets `defaults:`, the pin path
// succeeds and returns the DEFAULTS, so the command would confidently print a
// provider the server will never use for it.
//
// An agent with neither a pin nor a tier still fails, because that one is a real
// config error — the resolver refuses it too ("has neither pin nor tier").
func agentModelForDisplay(cfg *config.Config, name string) (provider, model string, err error) {
	// Config validation rejects pin+tier on one agent, so a non-empty tier here
	// means the tier path is the ONLY path this agent has.
	if def, ok := cfg.Agents[name]; ok && def.Tier != "" {
		return byTierProvider, "tier:" + def.Tier, nil
	}
	provider, model, pattern, err := cfg.ResolveAgentModel(name)
	if err != nil {
		return "", "", err
	}
	// RFC BG: a model_pattern alias resolves against the live catalog at run
	// time, so show the glob rather than the empty string it resolves to here.
	if pattern != "" {
		model = pattern
	}
	return provider, model, nil
}
