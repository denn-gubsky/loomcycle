package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// startMCPSurfaces is boot phase 9: the stdio MCP server in `loomcycle mcp`
// mode, and the HTTP MCP transport at /v1/_mcp in every mode.
func (b *boot) startMCPSurfaces() {
	mcpMode := b.mcpMode
	storeIface := b.storeIface
	srv := b.srv
	bgCtx := b.bgCtx

	// Optional operator default for the spawn_run transport timeout
	// (RFC P). 0/unset → disabled (defer to the run's own budget).
	// Read outside the mcpMode block: the HTTP MCP transport below is
	// always wired (both server and mcp modes) and the RFC R thin client
	// (`mcp --upstream`) proxies to it, so it needs the same bound.
	mcpSpawnRunTimeoutMS := 0
	if v := os.Getenv("LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			mcpSpawnRunTimeoutMS = n
		}
	}
	if mcpMode {
		// Optional operator override for the stdio dispatch concurrency
		// cap (RFC O). 0/unset → the package default (16).
		mcpMaxConcurrentCalls := 0
		if v := os.Getenv("LOOMCYCLE_MCP_MAX_CONCURRENT_CALLS"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				mcpMaxConcurrentCalls = n
			}
		}
		mcpSrv := lcmcp.New(lcmcp.Config{
			Connector:          srv, // *http.Server satisfies connector.Connector
			Runner:             srv, // *http.Server satisfies runner.Runner (streaming)
			Store:              storeIface,
			Logf:               log.Printf, // log goes to stderr; never stdout (stdout is the JSON-RPC wire)
			ServerName:         "loomcycle",
			ServerVersion:      buildVersion,
			MaxConcurrentCalls: mcpMaxConcurrentCalls,
			SpawnRunTimeoutMS:  mcpSpawnRunTimeoutMS,
		})
		// Run the MCP stdio loop in a goroutine. When stdin closes
		// (Claude Code disconnects), Serve returns; we log and let
		// the HTTP listener keep serving until the process is sent
		// a signal. Operators who want MCP-disconnect = process-exit
		// can wrap loomcycle in a supervisor that watches for the
		// log line.
		go func() {
			// Sourced from the registry (lcmcp.MetaToolCount) so the count
			// can't drift — the hardcoded "20" went stale once the registry
			// grew past 40. Same rationale as the CLI help count.
			log.Printf("mcp: stdio server starting (%d tools registered)", lcmcp.MetaToolCount())
			if err := mcpSrv.Serve(bgCtx, os.Stdin, os.Stdout); err != nil {
				log.Printf("mcp: serve ended: %v", err)
			} else {
				log.Printf("mcp: stdio closed; HTTP listener continues")
			}
		}()
	}

	// v0.8.15.3: HTTP MCP transport. Always wired (both server and
	// mcp modes), per RFC decision C-open-question rationale:
	// service-to-service consumers don't use the stdio wrapper and
	// need a network endpoint. Security posture is identical to all
	// other /v1/* endpoints — bearer-authed via s.authMiddleware in
	// the server's Mux(). This /v1/_mcp endpoint is what the RFC R
	// thin client (`loomcycle mcp --upstream`) proxies to.
	mcpHTTPHandler := lcmcp.NewHTTPHandler(lcmcp.Config{
		Connector:     srv, // same *http.Server instance used by stdio MCP
		Runner:        srv,
		Store:         storeIface,
		Logf:          log.Printf,
		ServerName:    "loomcycle",
		ServerVersion: buildVersion,
		// RFC P spawn_run timeout must apply to the HTTP transport too —
		// the RFC R thin client (`mcp --upstream`) proxies to /v1/_mcp, so
		// without this the now-recommended topology would have no spawn_run
		// bound (the exp3 wedge class, one layer up). MaxConcurrentCalls is
		// intentionally NOT passed: it builds the stdio Serve loop's
		// semaphore and is meaningless here — the HTTP transport is already
		// per-request concurrent at the http.Server level, and run
		// concurrency is bounded downstream by the runtime semaphore +
		// per-tenant fairness.
		SpawnRunTimeoutMS: mcpSpawnRunTimeoutMS,
	})
	srv.SetMCPHTTPHandler(mcpHTTPHandler)
	// Background session sweeper. Reuses the dynamic-agent sweeper
	// pattern: bound to bgCtx, exits on shutdown, idempotent under
	// concurrent invocations. 5-minute cadence is well under the
	// 30-min session inactivity TTL — leaves at least ~25 min between
	// a session's last access and its earliest possible reaping.
	lcmcp.RunHTTPSessionSweeper(bgCtx, mcpHTTPHandler.Sessions(), 5*time.Minute, log.Printf)
	log.Printf("mcp: HTTP transport enabled at POST /v1/_mcp (session sweeper interval=5m, TTL=30m)")
}

// logSessionLockGC reports the session-lock GC settings. It is its own step,
// apart from startSessionGC, because the boot log has always printed this
// line after the MCP transport's.
func (b *boot) logSessionLockGC() {
	log.Printf("session-lock GC: interval=%s max_idle=%s", b.sessionGCInterval, b.sessionGCMaxIdle)
}

// startResolveProbe registers the snapshot-restore force-probe and starts
// the periodic resolver probe loop.
func (b *boot) startResolveProbe() {
	cfg := b.cfg
	pr := b.pr
	resolver := b.resolver
	bgCtx := b.bgCtx

	// Periodic probe loop: re-runs the resolver's reachability +
	// model-list probe on the configured cadence so a provider
	// recovering from an outage shows up in the matrix without a
	// loomcycle restart. Default 15 minutes; clamped to [60s, 1h] in
	// the config loader. The first probe already ran inside
	// buildResolver above (synchronously, before traffic begins) —
	// this goroutine handles all subsequent rounds.
	probeInterval := cfg.Env.ResolveProbeInterval
	if probeInterval <= 0 {
		probeInterval = 15 * time.Minute
	}
	// v0.8.17: register the force-probe callback. The snapshot restore
	// handler (POST /v1/_snapshots/{id}/restore) calls
	// resolver.ForceProbe(ctx) to refresh the matrix before the
	// operator can call Resume — the pause-resume-snapshot RFC says
	// the resolver state is excluded from snapshots, so a fresh probe
	// closes the gap.
	resolver.SetForceProbeCallback(func(ctx context.Context) {
		runResolveProbeOnce(ctx, resolver, pr, cfg)
	})
	go runResolveProbeLoop(bgCtx, resolver, pr, cfg, probeInterval)
	log.Printf("resolve probe: interval=%s", probeInterval)
}

// wireConfigReload registers the runtime config-reload sections and the
// loader that re-assembles the config stack for POST /v1/_config/reload.
func (b *boot) wireConfigReload() {
	cfg := b.cfg
	pr := b.pr
	storeIface := b.storeIface
	srv := b.srv
	resolver := b.resolver
	bgCtx := b.bgCtx

	// RFC CK — wire the runtime config-reload registry now that the resolver,
	// provider set, and background ctx exist. Each SectionReloader rebuilds its
	// subsystem IN PLACE from a validated candidate (the Server's handle pointers
	// never change, so the probe loop + all readers keep working). load re-runs
	// LoadLayers over the SAME captured layer stack (disk files re-read fresh).
	// POST /v1/_config/reload drives it.
	reloaders := []lchttp.SectionReloader{
		{
			// Provider set + resolver policy.
			Sections: []string{"providers", "models", "tiers", "provider_priority"},
			Reload: func(newCfg *config.Config) error {
				if err := pr.Reload(newCfg); err != nil {
					return err
				}
				resolver.ReloadPolicy(newCfg.ProviderPriority, convertTiers(newCfg.Tiers, newCfg.Models))
				// runResolveProbeOnce reads pr.currentCfg() (now the reloaded config),
				// so this immediately probes the possibly-changed provider set.
				runResolveProbeOnce(bgCtx, resolver, pr, newCfg)
				return nil
			},
		},
	}
	if cfg.Env.SchedulerEnabled {
		// Static scheduled_runs: re-materialize into the substrate (idempotent
		// add/change; a yaml-REMOVED schedule keeps its row by design — see
		// BootstrapStaticSchedules). The sweeper polls the store, so re-bootstrap
		// alone suffices; the scheduler's own tick/allowlist knobs stay boot-fixed.
		reloaders = append(reloaders, lchttp.SectionReloader{
			Sections: []string{"scheduled_runs"},
			Reload: func(newCfg *config.Config) error {
				bs := &builtin.ScheduleDef{Store: storeIface, Cfg: newCfg}
				_, err := bs.BootstrapStaticSchedules(bgCtx)
				return err
			},
		})
	}
	// Concurrency: global semaphore caps + per-user caps + the providers' call
	// lanes. Owns `providers` too because the lanes are built from
	// providers[*].max_concurrent / lanes / max_models_in_flight, and `models`
	// because a lane's model_caps key may be an alias. LAST, because it cannot
	// fail: a reloader that fails after it would leave its new caps live while
	// the config holder keeps the old ones, and a later reload back to the old
	// values would then see no change to apply.
	reloaders = append(reloaders, lchttp.SectionReloader{
		Sections: []string{"concurrency", "providers", "models"},
		Reload:   func(newCfg *config.Config) error { srv.ReloadConcurrency(newCfg); return nil },
	})
	// The reload LOADER re-runs the full boot assembly (re-glob siblings, re-read
	// CONFIG_DIR/CONFIG_FILES) rather than replaying the captured boot `layers`, so
	// a config or section file added/removed since boot takes effect on reload
	// without a restart. verbose=false: don't re-log the layering on every reload.
	srv.SetConfigReloader(func() (*config.Config, error) {
		reloadLayers, err := assembleConfigLayers(b.presetFlags, b.cfgPaths, false)
		if err != nil {
			return nil, err
		}
		return config.LoadLayers(reloadLayers...)
	}, reloaders...)
}
