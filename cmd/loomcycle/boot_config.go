package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
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
// The standard OTEL_* environment configures it; a value loomcycle cannot
// honour stops the boot here rather than tracing differently than asked.
func (b *boot) initTelemetry() {
	otelCfg, err := lcotel.ResolveConfig(os.Getenv)
	if err != nil {
		fatal("otel: invalid tracing configuration", "err", err)
	}
	otelCfg.ServiceVersion = buildVersion
	// The replica id /metrics labels its series with, so a trace and a
	// scrape from one instance carry the same name.
	otelCfg.InstanceID = b.cfg.Env.ReplicaID
	otelCfg.TenantSamplerRatios = lcotel.MergeTenantSamplerRatios(
		b.cfg.Observability.Tracing.TenantSamplerRatios, otelCfg.TenantSamplerRatios)
	otelCfg.UserHashKey, err = credential.DeriveOTelUserKey(b.cfg.Env.SecretKey)
	if err != nil {
		fatal("otel: LOOMCYCLE_SECRET_KEY cannot key the span user hash", "err", err)
	}
	otelShutdown, err := lcotel.Init(otelCfg)
	if err != nil {
		fatal("otel: tracer setup failed", "err", err)
	}
	if otelCfg.Enabled {
		slog.Info("otel: tracer enabled", "protocol", otelCfg.Protocol,
			"sampler", otelCfg.SamplerName, "sampler_arg", otelCfg.SamplerArg,
			"inbound_context", otelCfg.Inbound, "headers_file", otelCfg.HeadersFile != "",
			"tenant_sampler_ratios", len(otelCfg.TenantSamplerRatios))
		// Only when spans leave the process: with tracing off nothing is
		// exported, and the warning would be noise.
		if otelCfg.UserHashKey == nil {
			slog.Warn("otel: LOOMCYCLE_SECRET_KEY is not set, so spans carry the raw loomcycle.user_id instead of loomcycle.user_hash")
		}
	} else {
		slog.Info("otel: disabled (set OTEL_EXPORTER_OTLP_ENDPOINT to enable)")
	}

	// The metric instruments are always on: /metrics renders them. OTLP export
	// of the same instruments is the operator's opt-in.
	metricsCfg, err := lcotel.ResolveMetricsConfig(os.Getenv)
	if err != nil {
		fatal("otel: invalid metrics configuration", "err", err)
	}
	metricsCfg.ServiceVersion, metricsCfg.InstanceID = buildVersion, b.cfg.Env.ReplicaID
	metricsCfg.Environment = otelCfg.Environment
	metricsShutdown, err := lcotel.InitMetrics(metricsCfg)
	if err != nil {
		fatal("otel: metrics setup failed", "err", err)
	}
	if metricsCfg.Export {
		slog.Info("otel: metric export enabled", "protocol", metricsCfg.Protocol, "headers_file", metricsCfg.HeadersFile != "")
	}
	b.otelShutdown = func(ctx context.Context) error {
		return errors.Join(otelShutdown(ctx), metricsShutdown(ctx))
	}
}
