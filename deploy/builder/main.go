// Command loomcycle-builder is the reference sandbox sidecar for loomcycle.
//
// It runs alongside a distroless loomcycle on the app network and exposes
// container-backed code execution as MCP-over-HTTP tools (sandbox_open/exec/
// write/read/close/list), driving rootless podman for per-session sandbox
// containers. loomcycle stays distroless and never runs a container engine;
// all podman + isolation complexity lives here.
//
// Security posture: the sidecar is the one privileged component. Run it under a
// nested-container-capable runtime (Sysbox recommended) or as a dedicated build
// node. Every MCP request is bearer-authenticated; each session container is
// launched --network none, --read-only, --cap-drop=ALL, non-root, with cpu/mem/
// pids caps and an in-memory tmpfs workspace.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.AllowAnon {
		log.Printf("WARNING: SANDBOX_ALLOW_ANON=1 — running UNAUTHENTICATED; use only for local dev")
	}

	store := NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL)
	eng := NewEngine(cfg, execRunner{})
	disp := NewDispatcher(cfg, eng, store)

	// Boot reconciliation: reap any sandbox containers a prior crash left behind
	// before we start accepting new sessions.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := eng.ReconcileBoot(ctx); err != nil {
			log.Printf("boot reconcile: %v (continuing)", err)
		}
		cancel()
	}

	// TTL garbage collector — reaps idle/aged sessions so a forgotten close (or
	// a dead loomcycle) can't leak containers indefinitely.
	go gcLoop(context.Background(), cfg, store, eng)

	mux := http.NewServeMux()
	mux.Handle("/mcp", NewMCPHandler(cfg, disp, version))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("loomcycle-builder %s listening on %s (image=%s runtime=%q egress=%v)",
		version, cfg.ListenAddr, cfg.Image, cfg.Runtime, cfg.AllowEgress)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("server: %v", err)
		os.Exit(1)
	}
}

func gcLoop(ctx context.Context, cfg *Config, store *Store, eng *Engine) {
	t := time.NewTicker(cfg.GCInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, sess := range store.Expired(now) {
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := eng.Close(rctx, sess.Name); err != nil {
					log.Printf("gc: close %s: %v", sess.Name, err)
				} else {
					log.Printf("gc: reaped expired session %s", sess.ID)
				}
				cancel()
			}
		}
	}
}
