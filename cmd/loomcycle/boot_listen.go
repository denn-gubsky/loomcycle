package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	loomgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	googlegrpc "google.golang.org/grpc"
)

// startListeners is boot phase 10: serve HTTP, and gRPC when
// LOOMCYCLE_GRPC_ADDR is set.
func (b *boot) startListeners() {
	cfg := b.cfg
	storeIface := b.storeIface
	srv := b.srv
	a2aServer := b.a2aServer
	httpServer := b.httpServer

	go func() {
		// The text is quoted in the deployment guides as the sign of a
		// healthy boot; keep it.
		slog.Info(fmt.Sprintf("loomcycle listening on %s", cfg.Env.ListenAddr), "addr", cfg.Env.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("listen: serving HTTP failed", "addr", cfg.Env.ListenAddr, "err", err)
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
			Store:     storeIface,
			CancelReg: srv.CancelRegistry(),
			Connector: srv, // v0.8.15: *http.Server satisfies connector.Connector
			Runner:    srv, // *http.Server satisfies runner.Runner (streaming)
			// RFC AW: share the HTTP server's token-budget tracker so the gRPC
			// TokenLimit RPC reads the identical live counters + reloads the same
			// cache the /v1/_limits handler uses.
			Limits:    srv.LimitsTracker(),
			AuthToken: cfg.Env.AuthToken,
			// RFC EY: the gRPC gate evaluates under the same mode as HTTP.
			ACLMode: srv.ACLResolver().Mode(),
			// RFC L: reuse the HTTP server's token resolution so gRPC
			// stamps the same authoritative principal (gRPC runs flow
			// authoritatively through RunOnce) and shares the open-mode
			// decision.
			PrincipalResolver: srv.ResolvePrincipal,
			AuthConfigured:    srv.AuthConfigured,
			BuildCommit:       buildCommit,
			BuildTime:         buildTime,
			// GetRun's `live` must see a team walk, which only the HTTP
			// server's walk table knows about.
			RunLive: srv.RunLive,
			// RFC EI P3: where a queued run waits, from this replica's queue.
			QueuePosition: srv.QueuePosition,
		})
		grpcSrv = googlegrpc.NewServer(
			googlegrpc.UnaryInterceptor(grpcAdapter.UnaryAuthInterceptor()),
			googlegrpc.StreamInterceptor(grpcAdapter.StreamAuthInterceptor()),
		)
		loomcyclepb.RegisterLoomcycleServer(grpcSrv, grpcAdapter)
		// v1.x RFC G — register the A2A gRPC binding on the same server.
		// gRPC is not a path-mounted http.Handler (it needs the HTTP/2
		// server), so the /a2a/grpc binding rides loomcycle's existing
		// grpc.Server; the AgentCard advertises the gRPC port.
		//
		// Call-site nil-guard mirrors the other two a2aServer uses above
		// (SetExtraMux at ~1426, PathTenantWrapper at ~1497): a2aServer is
		// nil whenever A2AServerEnabled is off. RegisterGRPC is itself
		// nil-receiver-safe, so this is defensive consistency rather than a
		// live-crash fix — it stops the gRPC path depending on the callee's
		// internal guard (exp7 C3).
		if a2aServer != nil {
			a2aServer.RegisterGRPC(grpcSrv)
		}
		grpcLis, err := net.Listen("tcp", cfg.Env.GrpcAddr)
		if err != nil {
			fatal("grpc listen: opening the listener failed", "addr", cfg.Env.GrpcAddr, "err", err)
		}
		grpcAdapter.MustLogStartupBanner(cfg.Env.GrpcAddr)
		go func() {
			// GracefulStop returns ErrServerStopped on the
			// normal shutdown path — don't log it as a serve
			// failure (would pollute operator logs and trip alert
			// rules watching for "error" tokens).
			if err := grpcSrv.Serve(grpcLis); err != nil && !errors.Is(err, googlegrpc.ErrServerStopped) {
				slog.Error("grpc serve: failed", "err", err)
			}
		}()
	}

	b.grpcSrv = grpcSrv
}

// waitAndShutdown is boot phase 11: block until SIGINT/SIGTERM, then pause the
// runs in flight while the listeners drain, then stop the background work and
// the flushers, in that order. The store, SQL Memory, MCP pool and backplane
// close after it returns, from main's cleanups.
func (b *boot) waitAndShutdown() {
	otelShutdown := b.otelShutdown
	pr := b.pr
	accessFlusher := b.accessFlusher
	heartbeatRunner := b.heartbeatRunner
	webhookReceiver := b.webhookReceiver
	httpServer := b.httpServer
	bgCancel := b.bgCancel
	countFlusherDone := b.countFlusherDone
	grpcSrv := b.grpcSrv
	pauseMgr := b.pauseMgrRef
	pauseTimeout := time.Duration(b.cfg.Env.ShutdownPauseTimeoutMs) * time.Millisecond
	if pauseMgr == nil {
		pauseTimeout = 0
	}

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down…")

	// RFC ES F1: park the runs in flight so the next boot resumes them, while
	// the listeners drain. Concurrently, not one after the other, so the whole
	// shutdown fits a container's stop grace (Docker's default is 10 s): the
	// pause is bounded by its own knob, each drain by 5 s. The pause comes
	// before bgCancel — that deletes this replica's heartbeat row and stops the
	// sweepers while runs are still finishing their turns.
	var drain sync.WaitGroup
	if pauseTimeout > 0 {
		drain.Add(1)
		go func() {
			defer drain.Done()
			logShutdownPause(pauseMgr.PauseForShutdown(pauseTimeout))
		}()
	}
	if grpcSrv != nil {
		drain.Add(1)
		go func() {
			defer drain.Done()
			stopGRPC(grpcSrv, grpcStopTimeout, pauseTimeout > 0)
		}()
	}
	// The ctx's cancel runs when waitAndShutdown returns, before main's
	// cleanups — the order it ran in when it was main's last defer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drain.Add(1)
	go func() {
		defer drain.Done()
		_ = httpServer.Shutdown(ctx)
	}()
	drain.Wait()

	bgCancel() // tear down sweeper + GC goroutines
	if heartbeatRunner != nil {
		heartbeatRunner.Stop()
	}
	// v0.11.9 — stop the OAuth-dev refresher's background goroutine
	// before the process exits so an in-flight refresh HTTP call
	// either completes + persists OR cleanly aborts. Without this,
	// the OS hard-kills the goroutine and a partially-rotated token
	// could be lost (the on-disk token is atomic write, but the
	// fresh access token might never make it to the persist step).
	if pr.anthropicOAuthRefresher != nil {
		pr.anthropicOAuthRefresher.Stop()
	}
	// RFC BL: stop the access-count flusher and flush the final window AFTER the
	// HTTP+gRPC servers have finished draining. In-flight Search requests
	// Record() access bumps right up to the end of that drain window, so the
	// flusher's final flush must run last to capture them — stopping it earlier
	// would leave those late bumps with no goroutine left to flush them. The
	// store closer is a cleanup (runs after main returns), so the store is still
	// open for this final write.
	accessFlusher.Stop()
	// The webhook delivery counters, for the same reason: the deliveries
	// answered during the drain are counted too. The store is still open.
	// bgCancel above stopped the periodic flusher; a flush it had in flight
	// finishes first (at most its own 10s timeout).
	<-countFlusherDone
	if webhookReceiver != nil {
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := webhookReceiver.FlushCounts(fctx); err != nil {
			slog.Error("webhooks: storing the last delivery counts failed", "err", err)
		}
		fcancel()
	}
	// Flush OTEL spans before exit. The OTLP exporter batches; without this,
	// in-flight spans never reach the collector. Give it its OWN 5s budget —
	// sharing the HTTP shutdown's ctx meant a slow in-flight request drain
	// could exhaust the deadline first, silently dropping the final spans.
	if otelShutdown != nil {
		otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer otelCancel()
		if err := otelShutdown(otelCtx); err != nil {
			slog.Error("otel: shutdown failed", "err", err)
		}
	}
}

// grpcStopTimeout bounds the gRPC server's graceful stop, which otherwise waits
// for every open stream to end — and a stream whose run is parked for the
// shutdown never does.
const grpcStopTimeout = 5 * time.Second

// stopGRPC stops srv gracefully within timeout. Past it, when runs were paused
// for the shutdown, the remaining RPCs are left to the process exit: Stop
// would cancel them, and a run started on a gRPC stream runs under that
// stream's context, so a run parked for the shutdown would end cancelled
// instead of being resumed. With no shutdown pause, Stop cancels them, which
// is what stops their tool calls.
func stopGRPC(srv *googlegrpc.Server, timeout time.Duration, runsPaused bool) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(timeout):
	}
	if runsPaused {
		slog.Warn("grpc: graceful stop expired; the open streams end with the process", "timeout", timeout)
		return
	}
	slog.Warn("grpc: graceful stop expired; stopping the open streams", "timeout", timeout)
	srv.Stop()
	<-done
}

// logShutdownPause reports what the shutdown pause did with the runs in flight.
func logShutdownPause(res pause.ShutdownResult) {
	slog.Info("shutdown: runs paused for the next boot to resume",
		"paused", res.ParkedRuns, "elapsed", res.Duration.Round(time.Millisecond), "interrupted", len(res.InterruptedRuns))
	if len(res.UnparkedRuns) > 0 {
		slog.Warn("shutdown: runs did not park in time and are not resumed (raise LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS)",
			"runs", len(res.UnparkedRuns), "run_ids", strings.Join(res.UnparkedRuns, ", "))
	}
	if res.UnpausableRuns > 0 {
		slog.Warn("shutdown: no-trace runs in flight are lost: they have nothing stored to resume from", "runs", res.UnpausableRuns)
	}
}
