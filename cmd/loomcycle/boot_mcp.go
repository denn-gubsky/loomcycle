package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// buildMCP is boot phase 4: the MCP client pool, the dynamic server
// registry, the eager handshake with each static server (skipped in
// `loomcycle mcp` mode) and the lazy resolver for those that failed it.
func (b *boot) buildMCP() {
	cfg := b.cfg
	mcpMode := b.mcpMode

	// MCP: spawn declared servers (stdio or http), discover their tools,
	// register each as `mcp__{server}__{tool}` alongside the built-ins.
	// Failures to spawn or handshake are logged and the server is skipped —
	// the other servers still come up.
	// v0.9.x dynamic MCP server registration substrate. In-process
	// registry consulted by the pool's build callback on every name
	// not found in the static yaml map. Mutated by the MCPServerDef
	// substrate tool's create / promote / retire ops; loaded from DB
	// at boot below so previous-deployment registrations survive
	// restart.
	dynamicMCPRegistry := mcp.NewDynamicRegistry()

	// RFC N: the pool's build callback resolves through lookup.MCPServer
	// with the run's tenant so a per-tenant dynamic registration dials its
	// OWN URL — not a shared one of the same name. A tenant-scoped dynamic
	// shadow takes precedence; otherwise the static yaml base resolves;
	// otherwise a shared ("") dynamic registration. stdio is yaml-only and
	// resolved straight from cfg (lookup.MCPServerSpec omits stdio fields).
	mcpDynView := mcpLookupView{dynamicMCPRegistry}
	mcpPool := mcp.NewPool(
		mcpPoolBuild(cfg, mcpDynView, &b.credSubstitute, slog.Default()),
		func(c mcp.Caller) {
			// Both stdio.Client and http.Client implement Close() error.
			// A future transport that doesn't gets logged so the leak is
			// at least visible to operators.
			type closer interface{ Close() error }
			cl, ok := c.(closer)
			if !ok {
				slog.Warn("mcp pool: teardown skipped (the client does not implement Close)", "client_type", fmt.Sprintf("%T", c))
				return
			}
			_ = cl.Close()
		},
		// RFC N tenantOf: the pool cache is keyed by (tenant, name). The
		// tenant is the authoritative principal's tenant carried on the run
		// ctx via RunIdentity (set from applyPrincipal / inherited by
		// sub-agents). RunIdentity, not the http auth package, keeps the
		// pool free of an import cycle. "" = shared/legacy tenant.
		func(ctx context.Context) string {
			return tools.RunIdentity(ctx).TenantID
		},
	)
	mcpPool.SetResultPolicy(mcpResultPolicy(cfg, mcpDynView))
	b.addCleanup(mcpPool.Close)

	// v0.9.x MCPServerDef tool — operator-admin-only. NOT appended to
	// allTools; wired into the HTTP server via SetMCPServerDefTool
	// below. Registry + Pool wired so create/promote installs entries
	// into both the DB AND the in-process registry the pool's build
	// callback consults; Pool is used for evict on retire / promote-
	// replaces and for the `rediscover` op's tools/list refresh.
	mcpServerDefTool := &builtin.MCPServerDef{
		Cfg:                 cfg,
		Registry:            dynamicMCPRegistry,
		Pool:                mcpPool,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	}

	// Initialise each server, apply per-server tools filter, and
	// register the resulting tools alongside the built-ins.
	//
	// Budget: 30s SHARED across all servers, not per-server. With many
	// servers configured, per-server timeouts can serially stack up past
	// K8s liveness/readiness deadlines and put the pod in a restart loop
	// before /healthz ever serves once. A shared ctx caps total startup
	// blocking; remaining servers after the budget exits are skipped with
	// a clear log line — they can be retried on demand via the lazy
	// pool resolution path on the first agent run that needs them.
	//
	// GetWithRetry handles the chicken-and-egg start order: when an MCP
	// server lives behind a dependency that boots concurrently with
	// loomcycle (e.g. a Next.js dev server compiling its /api/mcp route
	// on first request), the first handshake attempt may fail with
	// ECONNREFUSED or a 404. GetWithRetry backs off (500ms, 1s, 2s, 4s,
	// 8s, 16s) until success or ctx exhaustion. Each retry logs.
	//
	// v0.8.15.1: skip the eager init entirely in `loomcycle mcp` mode.
	// The consumer (Claude Code, custom MCP orchestrator) expects the
	// stdio JSON-RPC loop to answer initialize / tools/list within
	// its short discovery timeout — well under the ~32s exponential-
	// backoff budget GetWithRetry can spend per unreachable upstream.
	// The lazy resolver wired below handles "server wasn't in pool at
	// boot" cleanly since v0.8.1; the first agent call that needs an
	// mcp__<server>__<tool> tool triggers the handshake there. Server
	// mode keeps eager init — fail-fast is correct when the operator
	// is starting a long-lived daemon.
	// Static MCP servers that failed (or, in `loomcycle mcp` mode, never attempted)
	// their boot handshake. Threaded into the run-start re-probe
	// (mcp.StaticToolsForRun) so a peer that comes up AFTER loomcycle self-heals on
	// the next run that references it — no restart. A static server's tools reach
	// the catalog ONLY via this boot loop, and the lazy resolver advertises nothing
	// to a fresh model, so without the re-probe a boot-skipped server (e.g. a
	// sidecar that started second) stays invisible until a loomcycle restart.
	// Populated here (single-threaded boot), then read-only once the enumerator is
	// set below.
	skippedStaticMCP := map[string]bool{}
	if mcpMode {
		slog.Info("mcp mode: skipping eager upstream MCP init; run-start re-probe + lazy resolver handshake on first agent use", "servers", len(cfg.MCPServers))
		for name := range cfg.MCPServers {
			skippedStaticMCP[name] = true
		}
	} else {
		mcpInitCtx, mcpInitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		for name, srv := range cfg.MCPServers {
			_, descs, err := mcpPool.GetWithRetry(mcpInitCtx, name, slog.Default())
			if err != nil {
				slog.Warn("mcp: server skipped — run-start re-probe will retry when a run references it", "server", name, "err", err)
				skippedStaticMCP[name] = true
				continue
			}
			filtered := applyToolsFilter(descs, srv.Tools)
			for _, d := range filtered {
				b.allTools = append(b.allTools, mcp.NewTool(mcpPool, name, d))
			}
			slog.Info("mcp: server ready", "server", name, "tools_registered", len(filtered),
				"tools_offered", len(descs), "transport", srv.Transport)
		}
		mcpInitCancel()
	}

	// Lazy MCP recovery — when an agent calls a tool from a server that
	// failed initial handshake (skipped above), this resolver tries one
	// fresh pool.Get on the agent's call path. On success the server's
	// tools are memoised in the resolver and dispatched. Subsequent
	// calls hit the cache without re-handshaking.
	//
	// Without this, an MCP peer that's down at loomcycle boot stays
	// invisible for the lifetime of the process — operators have to
	// notice the "skipped" log line and restart loomcycle by hand.
	// In a server environment where peers (jobs-search-web, other MCP
	// services) restart independently, that's recurring operational
	// pain. See internal/tools/mcp/lazy.go for the state machine. Membership +
	// per-server tools resolve through the shared lookup.MCPServer
	// (static cfg.MCPServers → dynamic registry), so cfg + the registry are
	// passed straight through.
	mcpLazyResolver := mcp.NewLazyResolver(mcpPool, cfg, dynamicMCPRegistry, func(server string, count int) {
		// Message kept verbatim: docs/PLAN.md quotes it.
		slog.Info(fmt.Sprintf("mcp[%s]: lazy-registered %d tool(s) on first agent call", server, count), "server", server, "tools", count)
	}, 0)

	b.dynamicMCPRegistry = dynamicMCPRegistry
	b.mcpPool = mcpPool
	b.mcpServerDefTool = mcpServerDefTool
	b.skippedStaticMCP = skippedStaticMCP
	b.mcpLazyResolver = mcpLazyResolver
}
