package builtin

import (
	"context"
	"fmt"
	"maps"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// agentdef_toolmodes.go — tool_modes (RFC GL) on AgentDef create and fork.
//
// A read mode narrows a tool grant, so the two authoring rules are the two a
// narrowing marker already has here:
//
//   - a fork may add a read mode and may not remove one (forkToolModes), as a
//     fork may make an agent no-trace or lock its overrides and may not undo
//     either. Removing it widens what every run of the lineage can do to the
//     store.
//   - a run may not author an agent holding whole a tool it holds itself only
//     in read mode (ceilToolModes): the capability ceiling every other
//     authority field is held to. Otherwise a read-only agent able to author
//     agents could mint a writer and spawn it.

// forkToolModes refuses a fork whose merged tool_modes drops a read mode its
// parent (or the operator's static definition of the agent) held.
func forkToolModes(name string, merged mergedDef, before map[string]string) error {
	for _, tool := range sortedModeTools(before) {
		if before[tool] == config.ToolModeRead && merged.ToolModes[tool] != config.ToolModeRead {
			return fmt.Errorf("AgentDef tool: a fork cannot remove the read mode of %s on %q (tool_modes.%s: read); keep it in tool_modes, "+
				"or have an operator create the agent anew", tool, name, tool)
		}
	}
	return nil
}

// unionToolModes is every tool either map gives a mode, a read mode winning.
func unionToolModes(a, b map[string]string) map[string]string {
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	for tool, mode := range b {
		if out[tool] != config.ToolModeRead {
			out[tool] = mode
		}
	}
	return out
}

// ceilToolModes holds the new definition to the author's own tool modes: a
// tool the author holds in read mode is granted to the new agent in read mode
// too, unless the version a fork is made from already held it whole.
func ceilToolModes(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	authorModes := tools.PolicyOf(ctx).ToolModes
	for _, tool := range sortedModeTools(authorModes) {
		if authorModes[tool] != config.ToolModeRead || !grantsTool(def.Tools, tool) ||
			def.ToolModes[tool] == config.ToolModeRead {
			continue
		}
		if base != nil && grantsTool(base.Tools, tool) && base.ToolModes[tool] == "" {
			continue
		}
		return fmt.Errorf("tool_modes: the new agent would hold %s whole, which is not within %s grant (%s in read mode). "+
			"A new agent can only be granted authority its author holds: set tool_modes {%s: read}, drop %s from tools, "+
			"or ask an operator to author this agent", tool, held, tool, tool, tool)
	}
	return nil
}

// grantsTool reports whether a raw tools declaration grants tool, the way the
// runtime grants it (an exact name or a trailing-* glob).
func grantsTool(declared []string, tool string) bool {
	set := make(map[string]bool, len(declared))
	for _, t := range declared {
		set[t] = true
	}
	return policy.Matches(tool, set)
}

func sortedModeTools(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
