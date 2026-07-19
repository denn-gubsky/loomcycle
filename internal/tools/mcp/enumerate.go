package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
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

// StaticToolsForRun recovers the tools of STATIC (yaml `mcp_servers:`) servers that
// were SKIPPED at boot — the run-start counterpart to the boot handshake loop, and
// the static-server sibling of DynamicToolsForRun.
//
// Why it exists. A static server's tools enter the tool catalog ONLY via the boot
// handshake loop (cmd/loomcycle/main.go). If the peer is unreachable during
// loomcycle's boot window it is skipped and its tools never register — invisible to
// every run until a loomcycle restart. The lazy resolver (lazy.go) does NOT close
// this: it recovers an actual tool CALL but never ADVERTISES a tool to a fresh
// model, so an agent whose only access is that server never enters tool-calling
// mode and can't trigger the lazy path. This advertises + wires the tools at run
// start instead, so a sidecar that comes up AFTER loomcycle self-heals on the next
// run that references it.
//
// skipped is the set of static server names that failed the boot handshake (in
// `loomcycle mcp` mode, where eager init is skipped entirely, it is EVERY static
// server). Only those are considered — a boot-registered server is already in the
// candidateTools floor, so re-advertising it here would be wasted work + a needless
// per-tenant handshake.
//
// Per skipped server THIS run references (wantServers, from the agent's declared
// tools):
//   - cached tools/list in the pool (a prior run already re-probed) → advertise
//     from cache, no handshake — the fast path once the peer is up.
//   - else → one bounded handshake under the run's tenant; on success advertise +
//     leave the entry warm; on failure drop it and continue (the lazy resolver
//     still backstops the actual call).
//
// A server the run does NOT reference is never handshaked, so a single down peer
// can't slow every run. The operator's per-server tools filter (srv.Tools) is
// applied exactly as the boot loop does. Best-effort throughout; logf may be nil.
func StaticToolsForRun(
	ctx context.Context,
	pool *Pool,
	servers map[string]config.MCPServer,
	skipped map[string]bool,
	tenant string,
	wantServers map[string]bool,
	handshakeTimeout time.Duration,
	logf func(string, ...any),
) []tools.Tool {
	if pool == nil || len(skipped) == 0 {
		return nil
	}
	var out []tools.Tool
	for name := range skipped {
		// Only pay for a server this run actually uses (mirror the F33 gate).
		if !wantServers[sanitiseServerName(name)] {
			continue
		}
		srv, ok := servers[name]
		if !ok {
			continue // config no longer declares it (defensive)
		}
		// Fast path: a prior run already re-probed this peer → advertise from the
		// pool's cache without a wire round-trip.
		if cached := pool.PeekTools(tenant, name); len(cached) > 0 {
			for _, d := range ApplyToolsFilter(cached, srv.Tools) {
				out = append(out, NewTool(pool, name, d))
			}
			continue
		}
		// Empty cache → one bounded handshake under the run's tenant (so pool.Get
		// dials the tenant's spec, mirroring DynamicToolsForRun).
		hsCtx, cancel := context.WithTimeout(withTenant(ctx, tenant), handshakeTimeout)
		_, descs, err := pool.Get(hsCtx, name)
		cancel()
		if err != nil {
			if logf != nil {
				logf("mcp[%s]: run-start re-probe failed (skipped at boot): %v — lazy first-call fallback still applies", name, err)
			}
			continue
		}
		for _, d := range ApplyToolsFilter(descs, srv.Tools) {
			out = append(out, NewTool(pool, name, d))
		}
		if logf != nil {
			logf("mcp[%s]: run-start re-probe advertised %d tool(s) (recovered a server skipped at boot)", name, len(descs))
		}
	}
	return out
}
