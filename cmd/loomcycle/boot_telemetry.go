package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/tenanttelemetry"
)

// tenantTelemetryRouter reads the operator's tenant-export settings and, when
// LOOMCYCLE_TENANT_TELEMETRY is on, builds the router initTelemetry hands to
// the tracer provider. Off, it returns nil and tracing is exactly as without
// the feature.
func (b *boot) tenantTelemetryRouter() *lcotel.TenantRouter {
	cfg, err := tenanttelemetry.ConfigFromEnv(os.Getenv)
	if err != nil {
		fatal("telemetry: invalid tenant telemetry configuration", "err", err)
	}
	b.tenantTelemetryCfg = cfg
	if !cfg.Enabled {
		return nil
	}
	b.tenantRouter = lcotel.NewTenantRouter(lcotel.TenantRouterConfig{
		QueueSize: cfg.QueueSize, MaxPipelines: cfg.MaxPipelines, ServiceVersion: buildVersion,
	})
	slog.Info("telemetry: tenant trace export enabled", "allow_http", cfg.AllowHTTP,
		"allowed_hosts", len(cfg.AllowedHosts), "max_ratio", cfg.MaxRatio,
		"queue", cfg.QueueSize, "max_pipelines", cfg.MaxPipelines)
	return b.tenantRouter
}

// wireTenantTelemetry builds the tenant policy manager over the store and the
// credential engine, applies the stored policies and turns the surfaces on.
// The address guard is the HTTP tool's: no private, loopback or metadata
// address unless the operator's private-host allowlist vouches for it.
func (b *boot) wireTenantTelemetry() {
	if b.tenantRouter == nil {
		return
	}
	deps := tenanttelemetry.Deps{
		Config: b.tenantTelemetryCfg, Store: b.storeIface, Router: b.tenantRouter,
		Dial:           netguard.GuardedDialContext(false, b.cfg.Env.HTTPPrivateHostAllowlist),
		ServiceVersion: buildVersion,
		RegisterSecret: func(tenant, value string) {
			b.logRedactor.RegisterCredential(redact.Holder{Tenant: tenant}, value)
			b.secrets.registerCredential(context.Background(), tenant, value)
		},
	}
	// Without a secret key no credential resolves, and a policy may then
	// name none: say so rather than fail each resolution.
	if b.credEngine != nil && b.credEngine.CanResolve() {
		deps.Credentials = b.credEngine
	}
	m, err := tenanttelemetry.New(deps)
	if err != nil {
		fatal("telemetry: tenant telemetry setup failed", "err", err)
	}
	if err := m.Refresh(context.Background()); err != nil {
		// The policies are re-read every minute; a boot that could not read
		// them yet exports nothing until then.
		slog.Warn("telemetry: could not load the tenant telemetry policies at boot", "err", err)
	}
	b.tenantTelemetry = m
	b.srv.SetTenantTelemetry(m)
}

// startTenantTelemetry keeps the policies applied: a refresh every minute
// (credential rotation and expiry, a missed invalidation) under bgCtx.
func (b *boot) startTenantTelemetry() {
	if b.tenantTelemetry == nil {
		return
	}
	goSupervised(b.bgCtx, "tenanttelemetry.refresh", b.tenantTelemetry.Run)
}
