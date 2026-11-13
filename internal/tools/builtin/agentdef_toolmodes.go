package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// admitToolModesChange refuses a version of name that would drop a read mode
// the lineage holds now: its active version's, its newest version's (a retire
// clears the active pointer) or the operator's static definition's. Checked on
// create, fork and promote, so promoting an older whole version or forking one
// pinned by def_id cannot drop it either; forkToolModes only sees the parent.
// Only the operator plane may lift a read mode (tools.IsOperatorPlane: an
// off-run operator call, never a run and never a tenant operator). A version
// that no longer grants the tool is not refused: it holds less.
func (a *AgentDef) admitToolModesChange(ctx context.Context, op, tenant, name string, def mergedDef) error {
	held, err := lineageToolModes(ctx, a.Store, tenant, name)
	if err != nil {
		return fmt.Errorf("%s: reading the tool modes of %q: %w", op, name, err)
	}
	if static, ok := a.Cfg.Agents[name]; ok {
		held = unionToolModes(held, static.ToolModes)
	}
	for _, tool := range sortedModeTools(held) {
		if held[tool] != config.ToolModeRead || def.ToolModes[tool] == config.ToolModeRead || !grantsTool(def.Tools, tool) {
			continue
		}
		if tools.IsOperatorPlane(ctx) {
			continue
		}
		return fmt.Errorf("%s: %q holds %s in read mode (tool_modes.%s: read), and only an operator may remove that; keep it in this version", op, name, tool, tool)
	}
	return nil
}

// lineageToolModes is the read modes name holds in tenant now: its active
// version's and its newest version's. A definition that does not parse cannot
// be shown to hold no mode, so it is an error.
func lineageToolModes(ctx context.Context, st store.Store, tenant, name string) (map[string]string, error) {
	modesOf := func(def json.RawMessage) (map[string]string, error) {
		var d struct {
			ToolModes map[string]string `json:"tool_modes"`
		}
		if err := json.Unmarshal(def, &d); err != nil {
			return nil, err
		}
		return d.ToolModes, nil
	}
	var held map[string]string
	var nf *store.ErrNotFound
	active, err := st.AgentDefGetActive(ctx, tenant, name)
	switch {
	case err == nil:
		m, perr := modesOf(active.Definition)
		if perr != nil {
			return nil, perr
		}
		held = unionToolModes(held, m)
	case !errors.As(err, &nf):
		return nil, err
	}
	rows, err := st.AgentDefListByName(ctx, name) // newest first
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.TenantID != tenant {
			continue
		}
		m, perr := modesOf(r.Definition)
		if perr != nil {
			return nil, perr
		}
		return unionToolModes(held, m), nil
	}
	return held, nil
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
// too. Unlike the other capability fields, what the forked version held is no
// exemption: a fork may pin any older version by def_id, so it would let a
// read-mode author mint a whole-tool version from one.
func ceilToolModes(ctx context.Context, def mergedDef, _ *mergedDef, held string) error {
	authorModes := tools.PolicyOf(ctx).ToolModes
	for _, tool := range sortedModeTools(authorModes) {
		if authorModes[tool] != config.ToolModeRead || !grantsTool(def.Tools, tool) ||
			def.ToolModes[tool] == config.ToolModeRead {
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
