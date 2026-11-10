package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// startMetricsListener opens the LOOMCYCLE_METRICS_LISTEN listener and serves
// h on it. An empty addr means the listener is off: it returns a nil server.
// A bind failure is returned, never logged and skipped: an operator who set
// the variable expects the scrape to be there.
//
// The listener serves no authenticated route and carries no request body
// worth reading, so it gets tight timeouts, unlike the main listener, which
// keeps read and write deadlines off for its SSE streams.
func startMetricsListener(addr string, h http.Handler, log *slog.Logger) (*http.Server, net.Addr, error) {
	if addr == "" {
		return nil, nil, nil
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics listener: %w", err)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	announceMetricsListener(log, addr)
	// Raw on purpose, like the main listener: a panic in Serve ends the
	// process instead of leaving it up with no metrics listener.
	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Error("metrics listener: serving failed", "addr", addr, "err", err)
		}
	}()
	return srv, lis.Addr(), nil
}

// announceMetricsListener logs the listener's address, and warns when it
// binds beyond loopback: anyone who can reach the port reads the metrics.
func announceMetricsListener(log *slog.Logger, addr string) {
	log.Info("metrics listener: serving unauthenticated GET /metrics", "addr", addr)
	if !config.IsLoopbackListenAddr(addr) {
		log.Warn("metrics listener: bound beyond loopback with no authentication; "+
			"keep the port on a network only the scraper reaches (a compose network, a firewall rule)",
			"addr", addr)
	}
}
