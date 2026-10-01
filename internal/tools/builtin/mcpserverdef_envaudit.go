package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// OperatorEnvWarnings audits every stored MCP server def version (all
// tenants, read-only) for a ${NAME} reference in a version that is not
// operator-authored. Before bindOperatorEnv any author could store one, and
// the pool now refuses to dial such a version, so each is a server an
// operator should either have an admin re-save or retire. Nothing is changed.
//
// A line names the def and the fields holding the reference, never a value.
func (m *MCPServerDef) OperatorEnvWarnings(ctx context.Context) ([]string, error) {
	names, err := m.Store.MCPServerDefListNames(ctx)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		active[n.ActiveDefID] = true
	}
	for _, n := range names {
		if seen[n.Name] {
			continue // ListByName already returns every tenant's versions
		}
		seen[n.Name] = true
		versions, err := m.Store.MCPServerDefListByName(ctx, n.Name)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			var ov mcpServerOverlay
			if json.Unmarshal(v.Definition, &ov) != nil || ov.OperatorAuthored {
				continue
			}
			locs := loommcp.EnvRefLocations(ov.URL, ov.Headers, ov.Command, ov.Args, ov.Env)
			if len(locs) == 0 {
				continue
			}
			state := "inactive"
			switch {
			case v.Retired:
				state = "retired"
			case active[v.DefID]:
				state = "ACTIVE"
			}
			out = append(out, fmt.Sprintf("mcp server %q v%d (def_id %s, tenant %q, %s) reads the server environment in %s but was not saved by an admin, so it is not dialed — have an admin acting in that tenant re-save it, else retire it",
				v.Name, v.Version, v.DefID, v.TenantID, state, strings.Join(locs, ", ")))
		}
	}
	return out, nil
}
