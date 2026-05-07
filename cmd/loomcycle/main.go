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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
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

	// Storage: SQLite (default, compact installs) or Postgres
	// (production, hundreds of concurrent agents). Both adapters
	// implement the same store.Store interface; they're tested against
	// a shared contract suite in CI so they can't drift silently.
	storeIface, storeCloser, err := openStore(cfg)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer storeCloser()
	srv := lchttp.New(cfg, pr, allTools, sem, storeIface)
	httpServer := &http.Server{
		Addr:              cfg.Env.ListenAddr,
		Handler:           srv.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Env.AuthToken == "" {
		log.Printf("WARNING: LOOMCYCLE_AUTH_TOKEN is not set; /v1 routes are unauthenticated (dev mode)")
	}

	go func() {
		log.Printf("loomcycle listening on %s", cfg.Env.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down…")
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
	ollama    providers.Provider
}

func newProviderResolver(cfg *config.Config) *providerResolver {
	pr := &providerResolver{}
	if cfg.Env.AnthropicAPIKey != "" {
		pr.anthropic = anthropic.New(cfg.Env.AnthropicAPIKey, "", nil)
	}
	if cfg.Env.OpenAIAPIKey != "" {
		pr.openai = openai.New(cfg.Env.OpenAIAPIKey, "", nil)
	}
	// Ollama has no API key — wire it up if a base URL is configured (the
	// loader defaults this to http://localhost:11434 so it's effectively
	// always-on; users disable it by setting OLLAMA_BASE_URL=disabled).
	if cfg.Env.OllamaBaseURL != "" && cfg.Env.OllamaBaseURL != "disabled" {
		pr.ollama = ollama.New(cfg.Env.OllamaBaseURL, nil)
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
			return nil, fmt.Errorf("ollama provider not configured (set OLLAMA_BASE_URL or unset OLLAMA_BASE_URL=disabled)")
		}
		return p.ollama, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", id)
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

// applyAllowedToolsFilter narrows a server's discovered tool descriptors
// to the operator-permitted subset. Empty allowed = pass-through (default
// behaviour: expose every tool the server advertises).
func applyAllowedToolsFilter(descs []mcp.ToolDescriptor, allowed []string) []mcp.ToolDescriptor {
	if len(allowed) == 0 {
		return descs
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowSet[name] = struct{}{}
	}
	out := make([]mcp.ToolDescriptor, 0, len(descs))
	for _, d := range descs {
		if _, ok := allowSet[d.Name]; ok {
			out = append(out, d)
		}
	}
	return out
}
