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
// operator-authored. Before bindOperatorEnv any author could store one. The
// pool refuses to dial such a version unless the operator opted out with
// LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0, so each is a server that is (or,
// opted out, will be) cut off until an admin re-saves or retires it. Nothing
// is changed.
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
	// A nil Cfg reports the default (refuse), never the opt-out.
	consequence := "it WILL NOT be dialed (set LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0 to dial it temporarily)"
	if m.Cfg != nil && m.Cfg.Env.MCPDialUnattributedEnv {
		consequence = "it is still dialed only because LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0"
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
			out = append(out, fmt.Sprintf("mcp server %q v%d (def_id %s, tenant %q, %s) reads the server environment in %s but was not saved by an admin; %s — to fix, an admin acting in that tenant re-saves it (create with the same overlay mints an attributed version), else retire it",
				v.Name, v.Version, v.DefID, v.TenantID, state, strings.Join(locs, ", "), consequence))
		}
	}
	return out, nil
}
