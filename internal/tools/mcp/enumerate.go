package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// withTenant stamps tenant onto ctx as the RunIdentity tenant so the pool —
// which keys its cache and its build callback off tools.RunIdentity(ctx) — dials
// THIS run's tenant. The enumerator runs at run-creation time, BEFORE the entry
// site stamps the full RunIdentity, so a bare ctx would otherwise carry tenant
// "" and the handshake would dial the wrong (shared) server. Preserves any other
// identity fields already present.
func withTenant(ctx context.Context, tenant string) context.Context {
	ident := tools.RunIdentity(ctx)
	if ident.TenantID == tenant {
		return ctx
	}
	ident.TenantID = tenant
	return tools.WithRunIdentity(ctx, ident)
}

// ActiveDefReader returns the active definition JSON for a dynamic MCP server,
// scoped to (tenant, name). Implemented by an adapter over the store in
// cmd/loomcycle; modelled as a narrow interface so this low-level package need
// not import internal/store. ok=false means "no active row for (tenant, name)".
type ActiveDefReader interface {
	ActiveMCPServerDef(ctx context.Context, tenant, name string) (def json.RawMessage, ok bool)
}

// DynamicToolsForRun enumerates the ADVERTISABLE tools for the dynamic
// (substrate-authored) MCP servers visible to a run in `tenant`. It is the
// run-start counterpart to the boot-time static-MCP registration loop, and the
// fix for F33.
//
// Per server (reg.NamesForTenant: the tenant's own names + shared "" names):
//   - If the active def carries cached discovered_tools → advertise them
//     directly. No handshake — this is the fast path and stays the common case.
//   - Else if the server is REFERENCED by this run's tools (wantServers)
//     → handshake ONCE (bounded by handshakeTimeout) via the pool to fetch
//     tools/list, advertise the result, and leave the pool entry warm for the
//     subsequent Execute. This is F33: a dynamic server whose discovery was
//     best-effort-skipped or failed at registration (an unreachable peer, a
//     `discover:false` create, or a def that exceeded the size cap) otherwise
//     advertises ZERO tools, so an agent whose tools is only that
//     server's wildcard never enters tool-calling mode and emits its call as
//     inert text.
//   - Else (empty cache AND not referenced) → skip. We never handshake a server
//     the run does not use, so a single down peer can't slow every run.
//
// wantServers holds the SANITISED server names the run references (see
// sanitiseServerName) so it lines up with the advertised tool-name segment.
// A nil/empty wantServers disables the F33 handshake path (cache-only) — the
// exact pre-fix behaviour, which keeps cache-only callers unchanged.
//
// Best-effort throughout: a store miss, an unmarshal error, or a failed
// handshake drops that one server and continues; the run still gets every other
// server's tools. logf may be nil.
func DynamicToolsForRun(
	ctx context.Context,
	pool *Pool,
	reg *DynamicRegistry,
	reader ActiveDefReader,
	tenant string,
	wantServers map[string]bool,
	handshakeTimeout time.Duration,
	logf func(string, ...any),
) []tools.Tool {
	if pool == nil || reg == nil || reader == nil {
		return nil
	}
	var out []tools.Tool
	for _, name := range reg.NamesForTenant(tenant) {
		// Resolve the active def with the same precedence as lookup.MCPServer:
		// the run's tenant first, then the shared "" base.
		raw, ok := reader.ActiveMCPServerDef(ctx, tenant, name)
		if !ok && tenant != "" {
			raw, ok = reader.ActiveMCPServerDef(ctx, "", name)
		}
		if !ok {
			continue
		}
		var def lookup.SubstrateMCPServer
		if err := json.Unmarshal(raw, &def); err != nil {
			continue
		}
		if len(def.DiscoveredTools) > 0 {
			for _, dt := range def.DiscoveredTools {
				out = append(out, NewTool(pool, name, ToolDescriptor{
					Name:        dt.Name,
					Description: dt.Description,
					InputSchema: dt.InputSchema,
				}))
			}
			continue
		}
		// F33: empty cache. Only pay the handshake cost for a server this run
		// actually references.
		if !wantServers[sanitiseServerName(name)] {
			continue
		}
		hsCtx, cancel := context.WithTimeout(withTenant(ctx, tenant), handshakeTimeout)
		_, descs, err := pool.Get(hsCtx, name)
		cancel()
		if err != nil {
			if logf != nil {
				logf("mcp[%s]: run-start discovery failed: %v — agent references it but no tools advertised this run (lazy first-call fallback still applies)", name, err)
			}
			continue
		}
		for _, d := range descs {
			out = append(out, NewTool(pool, name, d))
		}
		if logf != nil {
			logf("mcp[%s]: run-start discovery advertised %d tool(s) (F33: referenced server had no cached tools)", name, len(descs))
		}
	}
	return out
}
