// Command loomcycle is the loomcycle sidecar.
//
// Usage:
//
//	loomcycle --config loomcycle.yaml
//
// Build identification: the buildCommit and buildTime vars are populated
// at link time via -ldflags so a running binary can identify itself.
// Without ldflags injection they default to "unknown" — useful signal
// when an operator is debugging "is this the binary I just built?".
// See loomcycle.sh for the canonical build invocation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/cli"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/heartbeat"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	"github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"

	googlegrpc "google.golang.org/grpc"

	loomgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/localapi"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	mcpstdio "github.com/denn-gubsky/loomcycle/internal/tools/mcp/stdio"
)

// Build identification — overridden at link time via:
//
//	go build -ldflags "-X main.buildCommit=$(git rev-parse --short HEAD) \
//	                   -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ...
//
// Defaults make a forgotten -ldflags invocation visible at runtime
// rather than silently shipping an unidentifiable binary.
var (
	buildCommit = "unknown"
	buildTime   = "unknown"
)

func main() {
	// Subcommand dispatch BEFORE flag parsing — let `loomcycle
	// validate ...` flow into the CLI surface without colliding with
	// the server's own --config flag.
	//
	// First non-flag arg is the subcommand keyword. If it's one of
	// the known subcommands, hand off to internal/cli and exit.
	// Otherwise fall through to the server entry point (preserves
	// backwards compat: `loomcycle --config foo.yaml` still works).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "validate":
			os.Exit(cli.RunValidate(os.Args[2:], os.Stdout, os.Stderr))
		case "agents":
			os.Exit(cli.RunAgents(os.Args[2:], os.Stdout, os.Stderr))
		case "health":
			os.Exit(cli.RunHealth(os.Args[2:], os.Stdout, os.Stderr))
		case "migrate":
			os.Exit(cli.RunMigrate(os.Args[2:], os.Stdout, os.Stderr))
		case "help", "-h", "--help":
			cli.PrintHelp(os.Stdout)
			return
		}
	}

	cfgPath := flag.String("config", "loomcycle.yaml", "path to config YAML")
	showVersion := flag.Bool("version", false, "print build identifier and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("loomcycle commit=%s built=%s\n", buildCommit, buildTime)
		return
	}

	// Identify ourselves first thing so an operator running a stale
	// binary spots it immediately — before any "but my code says X"
	// debugging spiral. Critical when development cycle is "git pull
	// && restart" without a rebuild step in between.
	log.Printf("loomcycle build: commit=%s time=%s", buildCommit, buildTime)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// Allow running with no YAML at all if a default agent can be derived
		// from defaults. Most callers will hit the same error path though.
		log.Fatalf("config: %v", err)
	}

	pr := newProviderResolver(cfg)
	sem := concurrency.New(
		cfg.Concurrency.MaxConcurrentRuns,
		cfg.Concurrency.MaxQueueDepth,
		cfg.Concurrency.QueueTimeout(),
	)
	// All built-in tools are SANDBOXED: each refuses every call until the
	// operator explicitly enables it via env. We register every tool and
	// log a note for any that's still disabled — that way the model sees
	// a clear "tool refused" error instead of a confusing "unknown tool",
	// and operators see at startup which tools they've configured.
	// Strip loopback aliases (localhost, 127.0.0.1, etc.) from the
	// operator's static allowlist. Belt-and-braces — the IP-level
	// guard at dial time also rejects loopback, but stripping here
	// means loopback never appears in the tool's effective list and
	// operators don't get fooled by seeing "localhost" listed.
	//
	// PrivateHostAllowlist acts as the exemption: if the operator
	// has explicitly opted in to a loopback host via that env var,
	// it survives the strip on the main allowlist too. This is what
	// lets a single "localhost" entry mean "agents may reach this
	// loopback host AND the IP-private check is lifted for it".
	staticHosts := builtin.StripLocalhostAliases(
		cfg.Env.HTTPHostAllowlist,
		cfg.Env.HTTPPrivateHostAllowlist,
	)
	httpTool := &builtin.HTTP{
		HostAllowlist:        staticHosts,
		PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist,
	}
	// Skill tool reuses the same name→body registry that the static
	// bundling path (Approach A in internal/config) uses. Loading once
	// at boot keeps the in-memory map authoritative — SIGHUP-style
	// hot-reload of skills is a future enhancement.
	skillSet, err := skills.LoadSet(cfg.Env.SkillsRoot)
	if err != nil {
		log.Fatalf("skills: %v", err)
	}
	if cfg.Env.SkillsRoot != "" {
		log.Printf("skills: loaded %d from %s", len(skillSet.Names()), cfg.Env.SkillsRoot)
	}
	if cfg.Env.AgentsRoot != "" {
		// Agents-from-MD discovery happened inside config.Load (must run
		// before resolveSystemPromptFiles so the merged map flows through
		// the existing pipeline). Log the count post-merge so operators
		// see the final cfg.Agents size, which may include yaml-only
		// entries on top of the discovered ones.
		log.Printf("agents: discovered from %s — total in cfg.Agents (after yaml merge): %d", cfg.Env.AgentsRoot, len(cfg.Agents))
	}
	if len(cfg.UserTiers) > 0 {
		// v0.8.2 — log configured user_tier policies at boot so
		// operators see what's available. "default" surfaces first
		// when present (it's the required entry per validation); the
		// rest sort lexicographically.
		names := make([]string, 0, len(cfg.UserTiers))
		for n := range cfg.UserTiers {
			if n != "default" {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		if _, hasDefault := cfg.UserTiers["default"]; hasDefault {
			names = append([]string{"default"}, names...)
		}
		log.Printf("user_tiers: configured %d — %s", len(names), strings.Join(names, " / "))
	}

	allTools := []tools.Tool{
		&builtin.Read{Root: cfg.Env.ReadRoot},
		&builtin.Write{Root: cfg.Env.WriteRoot},
		&builtin.Edit{Root: cfg.Env.WriteRoot},
		httpTool,
		&builtin.WebFetch{HTTP: httpTool},
		&builtin.WebSearch{APIKey: cfg.Env.BraveAPIKey},
		&builtin.Bash{Enabled: cfg.Env.BashEnabled, Cwd: cfg.Env.BashCwd},
		&builtin.SkillTool{Set: skillSet},
	}
	// Memory tool — wired post-store so it can grab the live Store
	// reference. Registered unconditionally; access is gated per-agent
	// via memory_scopes yaml + the Memory.Store==nil branch when the
	// runtime hasn't configured a store backend.
	memoryTool := &builtin.Memory{
		MaxValueBytes:     cfg.Env.MemoryMaxValueBytes,
		DefaultQuotaBytes: cfg.Env.MemoryMaxScopeBytes,
	}
	allTools = append(allTools, memoryTool)

	// Channel tool (v0.8.4) — persistent inter-agent message bus.
	// One Bus instance per process so in-process subscribers waiting
	// in long-poll mode get sub-millisecond notification. Same
	// Store==nil branch as Memory: tool registered unconditionally,
	// declines all ops when no store configured. Per-agent ACL via
	// `channels:` yaml + the operator-declared top-level `channels:`
	// block.
	channelBus := channels.NewBus()
	channelTool := &builtin.Channel{
		Bus:           channelBus,
		MaxValueBytes: cfg.Env.ChannelsMaxValueBytes,
		LongPollCapMS: cfg.Env.ChannelsLongPollCapMS,
	}
	allTools = append(allTools, channelTool)

	// Local API MCP gateway (v0.4.0+). When `local_api.spec` is set
	// in loomcycle.yaml, parse the OpenAPI spec and register one tool
	// per operation. Each tool forwards calls to local_api.base_url
	// with the agent's `bearer` field as Authorization. Replaces the
	// curl-shaped HTTP-tool pattern Phase B agents currently use.
	if cfg.LocalAPI.SpecPath != "" {
		laTools, laWarns, err := localapi.Build(localapi.Config{
			SpecPath:       cfg.LocalAPI.SpecPath,
			BaseURL:        cfg.LocalAPI.BaseURL,
			ToolNamePrefix: cfg.LocalAPI.ToolNamePrefix,
		}, cfg.ConfigDir())
		if err != nil {
			log.Printf("local-api gateway disabled: %v", err)
		} else {
			for _, w := range laWarns {
				log.Printf("local-api: %s", w)
			}
			log.Printf("local-api: registered %d tools from %s", len(laTools), cfg.LocalAPI.SpecPath)
			allTools = append(allTools, laTools...)
		}
	}
	if cfg.Env.ReadRoot == "" {
		log.Printf("note: Read tool is registered but disabled — set LOOMCYCLE_READ_ROOT to enable")
	}
	if cfg.Env.WriteRoot == "" {
		log.Printf("note: Write + Edit tools are registered but disabled — set LOOMCYCLE_WRITE_ROOT to enable")
	}
	if len(staticHosts) == 0 && !cfg.Env.HTTPCallerAuthoritative {
		log.Printf("note: HTTP + WebFetch tools are registered but disabled — set LOOMCYCLE_HTTP_HOST_ALLOWLIST to enable (or LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1 to delegate the allowlist to the caller)")
	}
	if cfg.Env.HTTPCallerAuthoritative {
		log.Printf("note: HTTP_CALLER_AUTHORITATIVE=1 — caller's allowed_hosts is the sole policy; operator's static list is fallback only")
	}
	if len(cfg.Env.HTTPPrivateHostAllowlist) > 0 {
		log.Printf("note: HTTP_PRIVATE_HOST_ALLOWLIST=%v — these hosts may resolve to private IPs (e.g. localhost callbacks)", cfg.Env.HTTPPrivateHostAllowlist)
	}
	if cfg.Env.BraveAPIKey == "" {
		log.Printf("note: WebSearch tool is registered but disabled — set BRAVE_API_KEY to enable")
	}
	if cfg.Env.BashEnabled {
		log.Printf("WARNING: Bash tool is enabled (LOOMCYCLE_BASH_ENABLED=1). This is NOT a true sandbox — run loomcycle inside a container or VM if you expose this to untrusted prompts.")
		if cfg.Env.BashCwd == "" {
			log.Printf("note: Bash is enabled but has no cwd; every call will refuse — set LOOMCYCLE_BASH_CWD")
		}
	} else {
		log.Printf("note: Bash tool is registered but disabled — set LOOMCYCLE_BASH_ENABLED=1 to enable (NOT a true sandbox; see docs)")
	}

	// Per-agent tool policy is default-deny: an agent with no allowed_tools
	// in YAML gets ZERO tools at the dispatcher. Warn at startup so
	// operators don't discover this only when an agent inexplicably can't
	// do anything. We log per-agent rather than once so the operator sees
	// which definitions are affected.
	for name, def := range cfg.Agents {
		if len(def.AllowedTools) == 0 {
			log.Printf("note: agent %q has no allowed_tools — it will see zero tools (intentional default-deny; add allowed_tools to its YAML to expose tools)", name)
		}
	}

	// MCP: spawn declared servers (stdio or http), discover their tools,
	// register each as `mcp__{server}__{tool}` alongside the built-ins.
	// Failures to spawn or handshake are logged and the server is skipped —
	// the other servers still come up.
	mcpPool := mcp.NewPool(
		func(name string) (mcp.Caller, error) {
			srv, ok := cfg.MCPServers[name]
			if !ok {
				return nil, fmt.Errorf("mcp_servers.%s: not in config", name)
			}
			switch srv.Transport {
			case "stdio":
				return spawnStdioMCP(name, srv)
			case "http":
				return mcphttp.New(mcphttp.Config{
					URL:     srv.URL,
					Headers: srv.Headers,
				})
			default:
				return nil, fmt.Errorf("mcp_servers.%s: unknown transport %q", name, srv.Transport)
			}
		},
		func(c mcp.Caller) {
			// Both stdio.Client and http.Client implement Close() error.
			// A future transport that doesn't gets logged so the leak is
			// at least visible to operators.
			type closer interface{ Close() error }
			cl, ok := c.(closer)
			if !ok {
				log.Printf("mcp pool: teardown skipped (%T does not implement Close)", c)
				return
			}
			_ = cl.Close()
		},
	)
	defer mcpPool.Close()

	// Initialise each server, apply per-server allowed_tools filter, and
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
	mcpInitCtx, mcpInitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	for name, srv := range cfg.MCPServers {
		_, descs, err := mcpPool.GetWithRetry(mcpInitCtx, name, log.Printf)
		if err != nil {
			log.Printf("mcp[%s]: skipped — %v", name, err)
			continue
		}
		filtered := applyAllowedToolsFilter(descs, srv.AllowedTools)
		for _, d := range filtered {
			allTools = append(allTools, mcp.NewTool(mcpPool, name, d))
		}
		log.Printf("mcp[%s]: ready, %d/%d tools registered (transport=%s)",
			name, len(filtered), len(descs), srv.Transport)
	}
	mcpInitCancel()

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
	// pain. See internal/tools/mcp/lazy.go for the state machine.
	mcpServerCfgs := make(map[string]mcp.ServerCfg, len(cfg.MCPServers))
	for name, srv := range cfg.MCPServers {
		mcpServerCfgs[name] = mcp.ServerCfg{AllowedTools: srv.AllowedTools}
	}
	mcpLazyResolver := mcp.NewLazyResolver(mcpPool, mcpServerCfgs, func(server string, count int) {
		log.Printf("mcp[%s]: lazy-registered %d tool(s) on first agent call (was skipped at boot)", server, count)
	}, 0)

	// Storage: SQLite (default, compact installs) or Postgres
	// (production, hundreds of concurrent agents). Both adapters
	// implement the same store.Store interface; they're tested against
	// a shared contract suite in CI so they can't drift silently.
	storeIface, storeCloser, err := openStore(cfg)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer storeCloser()
	// Memory tool depends on the Store; wire the live backend in now
	// that the adapter is open. This keeps the per-agent registration
	// at boot (allTools assembled once) and the tool's nil-Store
	// fallback for operators running without a configured store.
	memoryTool.Store = storeIface
	channelTool.Store = storeIface
	srv := lchttp.New(cfg, pr, allTools, sem, storeIface)
	srv.SetMCPFallback(mcpLazyResolver.Resolve)

	// Build the model-resolution matrix (resolve.Resolver). Providers
	// without API keys are MARKED EXCLUDED so Snapshot() shows the
	// distinct "no key configured" state — the resolver skips them
	// like unreachable providers but operators can tell the two
	// apart in logs and dashboards. Providers with keys are probed
	// live (GET /v1/models or /api/tags) and seeded from the
	// response. The probe goroutine started below re-runs the probe
	// on the configured cadence to catch recoveries.
	resolver := buildResolver(cfg, pr)
	srv.SetResolver(resolver)

	httpServer := &http.Server{
		Addr:              cfg.Env.ListenAddr,
		Handler:           srv.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Env.AuthToken == "" {
		log.Printf("WARNING: LOOMCYCLE_AUTH_TOKEN is not set; /v1 routes are unauthenticated (dev mode)")
	}

	// Background goroutines (heartbeat sweeper + session-lock map GC)
	// share a single ctx tied to the signal handler below — graceful
	// shutdown cancels them alongside the HTTP server. We rely on the
	// goroutines to exit promptly on ctx.Done(); both are documented
	// to do so in their packages.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	if cfg.Env.HeartbeatSweeperEnabled && storeIface != nil {
		sweeper := heartbeat.New(storeIface, heartbeat.Config{
			Interval:   cfg.Env.HeartbeatSweepInterval,
			StaleAfter: cfg.Env.HeartbeatStaleAfter,
		})
		go sweeper.Run(bgCtx)
	} else {
		log.Printf("heartbeat: sweeper disabled (LOOMCYCLE_HEARTBEAT_SWEEPER=0 or no Store)")
	}

	// Memory tool TTL sweeper. Cheap periodic DELETE of expired rows.
	// The store also filters expired entries at read time, so this
	// goroutine only matters for keeping the table small over the
	// long haul.
	if cfg.Env.MemorySweepInterval > 0 && storeIface != nil {
		go runMemorySweeper(bgCtx, storeIface, cfg.Env.MemorySweepInterval)
		log.Printf("memory: sweeper interval=%s", cfg.Env.MemorySweepInterval)
	} else {
		log.Printf("memory: sweeper disabled (LOOMCYCLE_MEMORY_SWEEP_MS=0 or no Store)")
	}

	// Channel tool TTL sweeper (v0.8.4). Same shape as MemorySweeper.
	// Reads filter expired rows regardless, so the sweeper is purely
	// for keeping the channel_messages table bounded.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runChannelsSweeper(bgCtx, storeIface, cfg.Env.ChannelsSweepInterval)
		log.Printf("channels: sweeper interval=%s", cfg.Env.ChannelsSweepInterval)
	} else if storeIface != nil {
		log.Printf("channels: sweeper disabled (LOOMCYCLE_CHANNELS_SWEEP_MS=0)")
	}
	// Boot summary of operator-declared channels. Mirrors the
	// "user_tiers: configured N — ..." line shape so operators see
	// the framework-primitive state at startup.
	if n := len(cfg.Channels); n > 0 {
		names := make([]string, 0, n)
		for name := range cfg.Channels {
			names = append(names, name)
		}
		sort.Strings(names)
		log.Printf("channels: configured %d — %s", n, strings.Join(names, " / "))
	}

	// Session-lock map GC. Defaults: prune entries idle ≥ 10 min, on
	// a 5-min tick. Disabled when both interval and max-idle resolve
	// to zero (operator can opt out by setting both to 0).
	sessionGCInterval := cfg.Env.SessionLockGCInterval
	if sessionGCInterval <= 0 {
		sessionGCInterval = 5 * time.Minute
	}
	sessionGCMaxIdle := cfg.Env.SessionLockMaxIdle
	if sessionGCMaxIdle <= 0 {
		sessionGCMaxIdle = 10 * time.Minute
	}
	go srv.RunSessionLockGC(bgCtx, sessionGCInterval, sessionGCMaxIdle)
	log.Printf("session-lock GC: interval=%s max_idle=%s", sessionGCInterval, sessionGCMaxIdle)

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
	go runResolveProbeLoop(bgCtx, resolver, pr, cfg, probeInterval)
	log.Printf("resolve probe: interval=%s", probeInterval)

	go func() {
		log.Printf("loomcycle listening on %s", cfg.Env.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// gRPC server. Optional; opt-in via LOOMCYCLE_GRPC_ADDR. Reuses
	// the same Store + cancel registry as the HTTP server so both
	// surfaces give identical answers. PR 1 of v0.5.5 implements
	// metadata RPCs only (Run/Continue stub to Unimplemented); PR 2
	// wires the streaming surface.
	var grpcSrv *googlegrpc.Server
	if cfg.Env.GrpcAddr != "" {
		grpcAdapter := loomgrpc.New(loomgrpc.Config{
			Store:       storeIface,
			CancelReg:   srv.CancelRegistry(),
			Runner:      srv, // *http.Server satisfies runner.Runner
			AuthToken:   cfg.Env.AuthToken,
			BuildCommit: buildCommit,
			BuildTime:   buildTime,
		})
		grpcSrv = googlegrpc.NewServer(
			googlegrpc.UnaryInterceptor(grpcAdapter.UnaryAuthInterceptor()),
			googlegrpc.StreamInterceptor(grpcAdapter.StreamAuthInterceptor()),
		)
		loomcyclepb.RegisterLoomcycleServer(grpcSrv, grpcAdapter)
		grpcLis, err := net.Listen("tcp", cfg.Env.GrpcAddr)
		if err != nil {
			log.Fatalf("grpc listen %s: %v", cfg.Env.GrpcAddr, err)
		}
		grpcAdapter.MustLogStartupBanner(cfg.Env.GrpcAddr)
		go func() {
			// GracefulStop returns ErrServerStopped on the
			// normal shutdown path — don't log it as a serve
			// failure (would pollute operator logs and trip alert
			// rules watching for "error" tokens).
			if err := grpcSrv.Serve(grpcLis); err != nil && !errors.Is(err, googlegrpc.ErrServerStopped) {
				log.Printf("grpc serve: %v", err)
			}
		}()
	}

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down…")
	bgCancel() // tear down sweeper + GC goroutines first
	if grpcSrv != nil {
		grpcSrv.GracefulStop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

// openStore resolves the operator's storage choice (sqlite default;
// postgres opt-in via storage.backend or LOOMCYCLE_STORAGE_BACKEND) and
// returns a ready Store + a Close-and-log closer.
//
// Errors propagate up to log.Fatalf in main; they cover both
// missing-config (postgres backend selected without a DSN) and
// dial/migration failures (postgres unreachable, schema not initialised
// when LOOMCYCLE_PG_AUTOMIGRATE=0).
func openStore(cfg *config.Config) (store.Store, func(), error) {
	switch cfg.Storage.Backend {
	case "sqlite", "":
		if err := os.MkdirAll(cfg.Env.DataDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("data dir: %w", err)
		}
		dbPath := filepath.Join(cfg.Env.DataDir, "loomcycle.db")
		st, err := storesqlite.Open(dbPath)
		if err != nil {
			return nil, nil, fmt.Errorf("sqlite open: %w", err)
		}
		log.Printf("store: sqlite at %s", dbPath)
		return st, func() { _ = st.Close() }, nil

	case "postgres":
		if cfg.Storage.PgDSN == "" {
			return nil, nil, fmt.Errorf("postgres backend selected but storage.pg_dsn / LOOMCYCLE_PG_DSN is empty")
		}
		st, err := storepostgres.Open(context.Background(), storepostgres.Config{
			DSN:          cfg.Storage.PgDSN,
			MaxOpenConns: cfg.Storage.PgMaxOpenConns,
			MinIdleConns: cfg.Storage.PgMinIdleConns,
			AutoMigrate:  cfg.Storage.PgAutoMigrate,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("postgres open: %w", err)
		}
		log.Printf("store: postgres (automigrate=%v)", cfg.Storage.PgAutoMigrate)
		return st, func() { _ = st.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("unknown storage.backend %q (want \"sqlite\" or \"postgres\")", cfg.Storage.Backend)
	}
}

// providerResolver constructs Provider instances at startup based on which
// env vars are set. A provider that wasn't configured returns a clear error
// when an agent tries to use it (rather than failing the whole startup).
type providerResolver struct {
	anthropic providers.Provider
	openai    providers.Provider
	// ollama = hosted ollama.com (Bearer auth via OLLAMA_API_KEY).
	// ollamaLocal = local-network endpoint (no auth, default
	// localhost:11434). Both wrap the same internal driver; only the
	// providerID, auth header, and base URL differ. See v0.8.3 split.
	ollama      providers.Provider
	ollamaLocal providers.Provider
	deepseek    providers.Provider
	gemini      providers.Provider
}

func newProviderResolver(cfg *config.Config) *providerResolver {
	pr := &providerResolver{}
	streamOpts := streamhttp.Options{
		HeaderTimeout: cfg.Env.ProviderHeaderTimeout,
		IdleTimeout:   cfg.Env.ProviderIdleTimeout,
	}
	if cfg.Env.AnthropicAPIKey != "" {
		pr.anthropic = anthropic.New(cfg.Env.AnthropicAPIKey, "", streamOpts, nil)
	}
	if cfg.Env.OpenAIAPIKey != "" {
		pr.openai = openai.New(cfg.Env.OpenAIAPIKey, "", streamOpts, nil)
	}
	// Hosted ollama.com — opts in via OLLAMA_API_KEY. Bearer-auth on the
	// same /api/chat wire shape as local Ollama; the driver is shared,
	// only the providerID + auth header + base URL differ.
	// OLLAMA_CLOUD_BASE_URL overrides the public endpoint for staged
	// rollouts or vendor mirrors.
	if cfg.Env.OllamaAPIKey != "" {
		pr.ollama = ollama.New("ollama", cfg.Env.OllamaAPIKey, cfg.Env.OllamaCloudBaseURL, streamOpts, nil)
	}
	// Local-network Ollama — no API key, local trust model. The loader
	// defaults OLLAMA_BASE_URL to http://localhost:11434, so this is
	// effectively always-on. Operators disable it via
	// OLLAMA_BASE_URL=disabled (or an empty string in shell env).
	if cfg.Env.OllamaBaseURL != "" && cfg.Env.OllamaBaseURL != "disabled" {
		pr.ollamaLocal = ollama.New("ollama-local", "", cfg.Env.OllamaBaseURL, streamOpts, nil)
	}
	// DeepSeek opts in via DEEPSEEK_API_KEY. Optional DEEPSEEK_BASE_URL
	// overrides the public endpoint for self-hosted OpenAI-compatible
	// mirrors. Same on/off semantics as Anthropic + OpenAI.
	if cfg.Env.DeepSeekAPIKey != "" {
		pr.deepseek = deepseek.New(cfg.Env.DeepSeekAPIKey, cfg.Env.DeepSeekBaseURL, streamOpts, nil)
	}
	// Gemini opts in via GEMINI_API_KEY. Optional GEMINI_BASE_URL
	// points at a Vertex AI Gemini endpoint instead of the public
	// generativelanguage.googleapis.com surface.
	if cfg.Env.GeminiAPIKey != "" {
		pr.gemini = gemini.New(cfg.Env.GeminiAPIKey, cfg.Env.GeminiBaseURL, streamOpts, nil)
	}
	return pr
}

func (p *providerResolver) Get(id string) (providers.Provider, error) {
	switch id {
	case "anthropic":
		if p.anthropic == nil {
			return nil, fmt.Errorf("anthropic provider not configured (set ANTHROPIC_API_KEY)")
		}
		return p.anthropic, nil
	case "openai":
		if p.openai == nil {
			return nil, fmt.Errorf("openai provider not configured (set OPENAI_API_KEY)")
		}
		return p.openai, nil
	case "ollama":
		if p.ollama == nil {
			return nil, fmt.Errorf("ollama provider not configured (set OLLAMA_API_KEY for hosted ollama.com; use provider id \"ollama-local\" for a local-network Ollama)")
		}
		return p.ollama, nil
	case "ollama-local":
		if p.ollamaLocal == nil {
			return nil, fmt.Errorf("ollama-local provider not configured (set OLLAMA_BASE_URL, or it's been opted out via OLLAMA_BASE_URL=disabled)")
		}
		return p.ollamaLocal, nil
	case "deepseek":
		if p.deepseek == nil {
			return nil, fmt.Errorf("deepseek provider not configured (set DEEPSEEK_API_KEY)")
		}
		return p.deepseek, nil
	case "gemini":
		if p.gemini == nil {
			return nil, fmt.Errorf("gemini provider not configured (set GEMINI_API_KEY)")
		}
		return p.gemini, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", id)
	}
}

// buildResolver constructs the model-resolution matrix
// (resolve.Resolver) and runs the FIRST round of live probes
// synchronously before returning. Per the operator's directive:
// providers without API keys are explicitly marked Excluded so
// Snapshot() distinguishes "operator chose not to enable this
// provider" from "operator enabled but the probe failed".
//
// The first probe completes synchronously so traffic served on the
// HTTP/gRPC port immediately after this returns sees a populated
// matrix (or an explicit Excluded marker for unconfigured providers).
// Subsequent rounds run in a background goroutine started by main —
// see runResolveProbeLoop.
func buildResolver(cfg *config.Config, pr *providerResolver) *resolve.Resolver {
	libraryTiers := convertTiers(cfg.Tiers)
	r := resolve.NewResolver(cfg.ProviderPriority, libraryTiers)

	// First-round probe — synchronous, so the matrix is hot before
	// the listener accepts traffic.
	runResolveProbeOnce(context.Background(), r, pr, cfg)
	return r
}

// runResolveProbeOnce performs one full sweep across all four
// providers: explicitly Excludes those without keys (visible in
// Snapshot), and live-probes the rest. Used by buildResolver for
// the synchronous startup probe and by runResolveProbeLoop for
// every periodic re-probe.
//
// Each provider's probe runs in its own goroutine with a 5-second
// deadline so a slow provider doesn't hold the whole sweep. The
// caller's ctx (typically bgCtx for the periodic loop, or
// context.Background for startup) bounds the total wait.
func runResolveProbeOnce(ctx context.Context, r *resolve.Resolver, pr *providerResolver, cfg *config.Config) {
	type probeJob struct {
		id         string             // provider id
		excluded   bool               // operator opted out
		exclReason string             // why excluded; surfaced in LastError
		provider   providers.Provider // nil when excluded
	}

	jobs := []probeJob{
		{id: "anthropic", excluded: cfg.Env.AnthropicAPIKey == "",
			exclReason: "ANTHROPIC_API_KEY not set"},
		{id: "openai", excluded: cfg.Env.OpenAIAPIKey == "",
			exclReason: "OPENAI_API_KEY not set"},
		{id: "deepseek", excluded: cfg.Env.DeepSeekAPIKey == "",
			exclReason: "DEEPSEEK_API_KEY not set"},
		{id: "gemini", excluded: cfg.Env.GeminiAPIKey == "",
			exclReason: "GEMINI_API_KEY not set"},
		{id: "ollama", excluded: cfg.Env.OllamaAPIKey == "",
			exclReason: "OLLAMA_API_KEY not set"},
		{id: "ollama-local", excluded: cfg.Env.OllamaBaseURL == "" || cfg.Env.OllamaBaseURL == "disabled",
			exclReason: "OLLAMA_BASE_URL not configured"},
	}
	for i := range jobs {
		if jobs[i].excluded {
			continue
		}
		if p, err := pr.Get(jobs[i].id); err == nil {
			jobs[i].provider = p
		} else {
			// Provider configured but resolver-Get failed. Mark
			// Excluded with the resolver error as the reason —
			// rare in practice (newProviderResolver wires every
			// driver whose key is set) but defensive.
			jobs[i].excluded = true
			jobs[i].exclReason = fmt.Sprintf("provider not registered: %v", err)
		}
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		if j.excluded {
			r.SetExcluded(j.id, j.exclReason)
			log.Printf("resolve probe: %s excluded (%s)", j.id, j.exclReason)
			continue
		}
		wg.Add(1)
		go func(j probeJob) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			models, err := j.provider.ListModels(probeCtx)
			if err != nil {
				r.SetReachable(j.id, false, nil, err.Error())
				log.Printf("resolve probe: %s unreachable (%v)", j.id, err)
				return
			}
			r.SetReachable(j.id, true, models, "")
			log.Printf("resolve probe: %s reachable (%d models listed)", j.id, len(models))
		}(j)
	}
	wg.Wait()
}

// runResolveProbeLoop runs runResolveProbeOnce on the configured
// interval until ctx is cancelled. Started as a goroutine from main;
// shares bgCtx with the heartbeat sweeper and session-lock GC so
// SIGTERM tears all three down together.
func runResolveProbeLoop(ctx context.Context, r *resolve.Resolver, pr *providerResolver, cfg *config.Config, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runResolveProbeOnce(ctx, r, pr, cfg)
		}
	}
}

// convertTiers translates the config-package representation of library
// tier definitions into the resolver-package representation. Mirrors
// the per-agent helper in internal/api/http/server.go but operates on
// the top-level cfg.Tiers map.
func convertTiers(in map[string][]config.TierCandidate) map[string][]resolve.Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]resolve.Candidate, len(in))
	for tier, cands := range in {
		conv := make([]resolve.Candidate, 0, len(cands))
		for _, c := range cands {
			conv = append(conv, resolve.Candidate{Provider: c.Provider, Model: c.Model})
		}
		out[tier] = conv
	}
	return out
}

// runMemorySweeper periodically deletes Memory rows whose TTL has
// expired. Read paths in the store filter expired rows out anyway so
// agents never see stale values; the sweeper just keeps the table
// bounded over time.
func runMemorySweeper(ctx context.Context, s store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			swept, err := s.MemorySweep(ctx)
			if err != nil {
				log.Printf("memory sweep: %v", err)
				continue
			}
			if swept > 0 {
				log.Printf("memory sweep: deleted %d expired row(s)", swept)
			}
		}
	}
}

// runChannelsSweeper is the v0.8.4 mirror of runMemorySweeper for
// the channel_messages table. Same shape — read paths filter
// expired rows regardless, so the sweeper is bounded-table cleanup.
func runChannelsSweeper(ctx context.Context, s store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			swept, err := s.ChannelSweepExpired(ctx)
			if err != nil {
				log.Printf("channels sweep: %v", err)
				continue
			}
			if swept > 0 {
				log.Printf("channels sweep: deleted %d expired row(s)", swept)
			}
		}
	}
}

// spawnStdioMCP starts a stdio MCP child for one server entry. Env keys are
// sorted so process listings are deterministic across runs.
func spawnStdioMCP(name string, srv config.MCPServer) (mcp.Caller, error) {
	keys := make([]string, 0, len(srv.Env))
	for k := range srv.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+srv.Env[k])
	}
	return mcpstdio.Spawn(mcpstdio.Config{
		Command: srv.Command,
		Args:    srv.Args,
		Env:     env,
		OnStderr: func(line string) {
			log.Printf("mcp[%s]: %s", name, line)
		},
	})
}

// applyAllowedToolsFilter — thin wrapper over mcp.ApplyAllowedToolsFilter.
// Lives in the mcp package so both the boot-time path here and the
// lazy-retry path (mcp.LazyResolver) share one implementation.
func applyAllowedToolsFilter(descs []mcp.ToolDescriptor, allowed []string) []mcp.ToolDescriptor {
	return mcp.ApplyAllowedToolsFilter(descs, allowed)
}
