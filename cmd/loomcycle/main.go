// Command loomcycle is the loomcycle sidecar.
//
// Usage:
//
//	loomcycle --config loomcycle.yaml
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
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcpstdio "github.com/denn-gubsky/loomcycle/internal/tools/mcp/stdio"
)

func main() {
	cfgPath := flag.String("config", "loomcycle.yaml", "path to config YAML")
	flag.Parse()

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
	// The Read tool is sandboxed: it refuses every call when Root is empty.
	// Operators must set LOOMCYCLE_READ_ROOT explicitly to enable it (no
	// silent default — the wrong default would leak file contents).
	allTools := []tools.Tool{
		&builtin.Read{Root: cfg.Env.ReadRoot},
	}
	if cfg.Env.ReadRoot == "" {
		log.Printf("note: Read tool is registered but disabled — set LOOMCYCLE_READ_ROOT to enable")
	}

	// MCP: spawn declared stdio servers, discover their tools, register
	// each as `mcp__{server}__{tool}` alongside the built-ins. Failures
	// to spawn or handshake are logged and the server is skipped — the
	// other servers still come up.
	mcpPool := mcp.NewPool(
		func(name string) (mcp.Caller, error) {
			srv, ok := cfg.MCPServers[name]
			if !ok {
				return nil, fmt.Errorf("mcp_servers.%s: not in config", name)
			}
			if srv.Transport != "stdio" {
				return nil, fmt.Errorf("mcp_servers.%s: transport %q not supported in this build (only stdio for now)", name, srv.Transport)
			}
			// Sort env keys so process listings and logs are deterministic
			// across runs — Go map iteration is randomised.
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
		},
		func(c mcp.Caller) {
			if cl, ok := c.(*mcpstdio.Client); ok {
				_ = cl.Close()
			}
		},
	)
	defer mcpPool.Close()

	for name, srv := range cfg.MCPServers {
		if srv.Transport != "stdio" {
			log.Printf("mcp[%s]: skipped (transport %q not yet supported)", name, srv.Transport)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, descs, err := mcpPool.Get(ctx, name)
		cancel()
		if err != nil {
			log.Printf("mcp[%s]: skipped — %v", name, err)
			continue
		}
		log.Printf("mcp[%s]: ready, %d tools registered", name, len(descs))
	}
	allTools = append(allTools, mcpPool.Tools()...)

	// Storage: open SQLite under DataDir. We could no-op when DataDir is
	// unset, but that would silently disable the transcript/continuation
	// endpoints — better to just use a sensible default and persist.
	if err := os.MkdirAll(cfg.Env.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	dbPath := filepath.Join(cfg.Env.DataDir, "loomcycle.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	log.Printf("store: sqlite at %s", dbPath)

	var storeIface store.Store = st
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
