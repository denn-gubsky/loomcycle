package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// loadConfig is boot phase 1: assemble the layered config stack and load it.
func (b *boot) loadConfig() {
	// Assemble the ordered config-layer stack (embedded presets → CONFIG_DIR →
	// CONFIG_FILES/--config + loomcycle.*.yaml section siblings). Factored into a
	// shared, error-returning helper (autodiscover.go) so the RFC CK runtime
	// reload can RE-run the same assembly — re-globbing siblings and re-reading
	// CONFIG_DIR — and thus pick up added/removed config files without a restart.
	layers, err := assembleConfigLayers(b.presetFlags, b.cfgPaths, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loomcycle: %s\n", err)
		os.Exit(1)
	}
	cfg, err := config.LoadLayers(layers...)
	if err != nil {
		fatal("config: load failed", "err", err)
	}
	// Non-fatal config advisories (e.g. an agent with Memory in tools
	// but no memory_scopes — the tool would default-deny every call, F21).
	// Surfaced once at boot so a silent-ish partial misconfig is visible.
	for _, warn := range cfg.Warnings {
		slog.Warn("config: warning", "detail", warn)
	}
	b.cfg = cfg
}

// initTelemetry starts the OpenTelemetry tracer (a no-op unless configured).
func (b *boot) initTelemetry() {
	cfg := b.cfg
	// v0.10.0 OpenTelemetry tracer bootstrap. No-op when
	// LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT is unset, so zero runtime
	// cost on deployments that haven't opted in. When set, every
	// otel.Tracer(...).Start(ctx, ...) call across the codebase emits
	// to the configured OTLP/HTTP collector.
	otelShutdown, otelErr := lcotel.Init(lcotel.Config{
		Endpoint:       cfg.Env.OTELExporterEndpoint,
		Headers:        cfg.Env.OTELExporterHeaders,
		ServiceName:    cfg.Env.OTELServiceName,
		ServiceVersion: buildVersion,
		SamplerRatio:   cfg.Env.OTELTracesSamplerRatio,
	})
	if otelErr != nil {
		fatal("otel: tracer setup failed", "err", otelErr)
	}
	if cfg.Env.OTELExporterEndpoint != "" {
		slog.Info("otel: tracer enabled", "url", cfg.Env.OTELExporterEndpoint, "service", cfg.Env.OTELServiceName,
			"sampler", fmt.Sprintf("traceidratio@%g", cfg.Env.OTELTracesSamplerRatio))
	} else {
		slog.Info("otel: disabled (set LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT to enable)")
	}
	b.otelShutdown = otelShutdown
}
