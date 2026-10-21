package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
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

// PrivateHostWarnings audits every active MCP server def (all tenants,
// read-only) that a tenant registered at runtime: http, not operator-authored,
// with a url host that is localhost or a private IP literal the operator has
// not vouched for in LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST. Such a server
// dials through the egress guard whatever LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS
// says, so every call to it is refused. That includes an admin's def saved
// before versions carried the operator_authored bit. Nothing is changed.
//
// A host name that resolves to a private address is not judged here (DNS can
// change before the dial); its dial error names the same fixes.
func (m *MCPServerDef) PrivateHostWarnings(ctx context.Context) ([]string, error) {
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
	var vouched []string
	if m.Cfg != nil {
		vouched = m.Cfg.Env.HTTPPrivateHostAllowlist
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
			if v.Retired || !active[v.DefID] {
				continue
			}
			var ov mcpServerOverlay
			if json.Unmarshal(v.Definition, &ov) != nil || ov.OperatorAuthored ||
				(ov.Transport != "http" && ov.Transport != "streamable-http") {
				continue
			}
			u, err := url.Parse(ov.URL)
			if err != nil {
				continue
			}
			host := u.Hostname()
			if !hostIsLocalOrPrivateIP(host) || hostAllowed(host, vouched) || netguard.IPLiteralAllowed(host, vouched) {
				continue
			}
			out = append(out, fmt.Sprintf("mcp server %q v%d (def_id %s, tenant %q) was registered at runtime and points at %s, a private address; it dials through the egress guard, so every call to it is refused — "+
				"to allow it, add %s to LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST, or have an admin re-save the definition off-run",
				v.Name, v.Version, v.DefID, v.TenantID, host, host))
		}
	}
	return out, nil
}

// hostIsLocalOrPrivateIP reports whether a url host is localhost or an IP
// literal the egress guard treats as private.
func hostIsLocalOrPrivateIP(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	ip := net.ParseIP(h)
	return ip != nil && netguard.IsPrivateIP(ip)
}
