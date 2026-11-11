package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/safego"
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
	secrets, err := loadOTELHeadersFile(&otelCfg, b.logRedactor)
	if err != nil {
		fatal("otel: reading the collector headers failed", "err", err)
	}
	b.otelHeaderSecrets = secrets
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
	// Values ignored in place of an error: unsupported names beside supported
	// ones while tracing is on; anything at all while it is off, where the
	// OTEL_* environment may belong to other programs.
	if len(otelCfg.Warnings) > 0 {
		if otelCfg.Enabled {
			for _, w := range otelCfg.Warnings {
				slog.Warn("otel: ignoring a setting", "detail", w)
			}
		} else {
			slog.Info("otel: tracing is off; unsupported OTEL_* values ignored", "detail", strings.Join(otelCfg.Warnings, "; "))
		}
	}
	otelCfg.TenantRouter = b.tenantTelemetryRouter()
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
	for _, w := range metricsCfg.Warnings {
		if metricsCfg.Export {
			slog.Warn("otel: ignoring a setting", "detail", w)
		} else {
			slog.Info("otel: metric export is off; unsupported OTEL_METRICS_EXPORTER value ignored", "detail", w)
		}
	}
	if metricsCfg.Export && metricsCfg.HeadersFile != "" {
		// One file, read once: the tracer's read already registered its
		// values with the redactors when tracing is on.
		if otelCfg.Headers != nil {
			metricsCfg.Headers = otelCfg.Headers
		} else {
			fileCfg := lcotel.Config{Enabled: true, HeadersFile: metricsCfg.HeadersFile}
			secrets, err := loadOTELHeadersFile(&fileCfg, b.logRedactor)
			if err != nil {
				fatal("otel: reading the collector headers failed", "err", err)
			}
			metricsCfg.Headers = fileCfg.Headers
			b.otelHeaderSecrets = append(b.otelHeaderSecrets, secrets...)
		}
	}
	metricsShutdown, err := lcotel.InitMetrics(metricsCfg)
	if err != nil {
		fatal("otel: metrics setup failed", "err", err)
	}
	if metricsCfg.Export {
		slog.Info("otel: metric export enabled", "protocol", metricsCfg.Protocol, "headers_file", metricsCfg.HeadersFile != "")
	}
	b.otelShutdown = concurrentShutdown(otelShutdown, metricsShutdown)
}

// errShutdownPanicked is a flush's answer when it panicked.
var errShutdownPanicked = errors.New("otel: a shutdown flush panicked")

// concurrentShutdown runs every shutdown at once under the one deadline and
// joins their errors: a trace flush stalled on a dead collector must not
// spend the deadline the metric flush needs, as running them in turn did.
func concurrentShutdown(fns ...func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		errs := make(chan error, len(fns))
		for _, fn := range fns {
			safego.Go(ctx, "otel.shutdown", func() {
				// Deferred, so a flush that panics still answers and the
				// collector below is not left waiting for it.
				err := errShutdownPanicked
				defer func() { errs <- err }()
				err = fn(ctx)
			})
		}
		var all []error
		for range fns {
			all = append(all, <-errs)
		}
		return errors.Join(all...)
	}
}

// loadOTELHeadersFile reads the collector headers file when tracing is on and
// one is named, puts the headers in cfg, and masks their values in log lines
// from now on: collector auth read from a file is in no environment the
// redactors were built from. It returns the values for the server's at-rest
// redactor, which does not exist yet.
func loadOTELHeadersFile(cfg *lcotel.Config, logRedactor *redact.Redactor) ([]string, error) {
	if !cfg.Enabled || cfg.HeadersFile == "" {
		return nil, nil
	}
	h, err := lcotel.ReadHeadersFile(cfg.HeadersFile)
	if err != nil {
		return nil, err
	}
	cfg.Headers = h
	values := make([]string, 0, len(h))
	for _, v := range h {
		values = append(values, v)
		logRedactor.RegisterOperator(v)
	}
	return values, nil
}
