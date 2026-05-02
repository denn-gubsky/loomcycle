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
	"syscall"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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
	builtins := []tools.Tool{
		&builtin.Read{},
	}

	srv := lchttp.New(cfg, pr, builtins, sem)
	httpServer := &http.Server{
		Addr:              cfg.Env.ListenAddr,
		Handler:           srv.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
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

// providerResolver constructs and caches Provider instances on demand.
// Anthropic is the only one wired at v0.1; the others return a clear error.
type providerResolver struct {
	cfg       *config.Config
	anthropic providers.Provider
}

func newProviderResolver(cfg *config.Config) *providerResolver {
	pr := &providerResolver{cfg: cfg}
	if cfg.Env.AnthropicAPIKey != "" {
		pr.anthropic = anthropic.New(cfg.Env.AnthropicAPIKey, "", nil)
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
		return nil, fmt.Errorf("openai provider: not implemented in v0.1 thin slice")
	case "ollama":
		return nil, fmt.Errorf("ollama provider: not implemented in v0.1 thin slice")
	default:
		return nil, fmt.Errorf("unknown provider %q", id)
	}
}
