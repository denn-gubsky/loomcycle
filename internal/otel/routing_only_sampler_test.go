package otel

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// restoreGlobalTracing puts back the globals doInit sets.
func restoreGlobalTracing(t *testing.T) {
	t.Helper()
	clearOTELEnv(t)
	prevTP, prevRouting, prevOnly := otel.GetTracerProvider(), tenantRouting.Load(), routingOnly.Load()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		tenantRouting.Store(prevRouting)
		routingOnly.Store(prevOnly)
		stats.enabled.Store(false)
	})
	tenantRouting.Store(false)
	otel.SetTracerProvider(trace.NewNoopTracerProvider())
}

func resolveFrom(t *testing.T, env map[string]string) Config {
	t.Helper()
	cfg, err := ResolveConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// With tenant export on and no operator endpoint, the routing-only provider
// records every run the operator's sampler keeps. It must be the operator's
// sampler, not the default that keeps every run.
func TestInit_RoutingOnlyProviderSamplesWithTheOperatorSampler(t *testing.T) {
	restoreGlobalTracing(t)
	cfg := resolveFrom(t, map[string]string{"OTEL_TRACES_SAMPLER": "always_off"})
	if cfg.Enabled {
		t.Fatal("tracing on with no endpoint")
	}
	cfg.TenantRouter = fastRouter(TenantRouterConfig{})
	shutdown, err := doInit(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	_, span := Tracer().Start(context.Background(), "run")
	defer span.End()
	if span.IsRecording() {
		t.Error("routing-only provider recorded a span the operator's always_off sampler drops")
	}
}

// Tracing off, the sampler variables may belong to other programs, so a bad one
// does not fail ResolveConfig; the routing-only provider that would sample
// with it refuses it.
func TestInit_RoutingOnlyProviderRefusesAnInvalidSampler(t *testing.T) {
	restoreGlobalTracing(t)
	for _, env := range []map[string]string{
		{"OTEL_TRACES_SAMPLER": "bogus"},
		{"OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "7"},
	} {
		cfg := resolveFrom(t, env)
		cfg.TenantRouter = fastRouter(TenantRouterConfig{})
		shutdown, err := doInit(cfg)
		_ = shutdown(context.Background())
		if err == nil || !strings.Contains(err.Error(), "OTEL_TRACES_SAMPLER") {
			t.Errorf("env %v: doInit err = %v, want the sampler refused", env, err)
		}
	}
}

// OTEL_SDK_DISABLED turns off every span this process would record, the ones
// a tenant's destination would get included.
func TestInit_SDKDisabledInstallsNoTenantRouter(t *testing.T) {
	restoreGlobalTracing(t)
	cfg := resolveFrom(t, map[string]string{"OTEL_SDK_DISABLED": "true"})
	cfg.TenantRouter = fastRouter(TenantRouterConfig{})
	shutdown, err := doInit(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if TenantRoutingInstalled() {
		t.Error("a tenant router is installed with OTEL_SDK_DISABLED=true")
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Error("OTEL_SDK_DISABLED=true left a recording provider installed")
	}
}
