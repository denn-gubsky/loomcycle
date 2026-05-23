// Package otel is loomcycle's v0.10.0 distributed-tracing bootstrap.
// It installs an OTLP/HTTP exporter against the global OpenTelemetry
// tracer provider when the operator sets
// LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT; otherwise it leaves the global
// provider as a no-op so every `tracer.Start(ctx, name)` call across the
// codebase costs nothing.
//
// The single Init function is the public entry point. Recorder
// helpers (RecordRunStart, RecordIteration, RecordProviderCall,
// RecordToolCall, RecordMCPCall, RecordRunDone) live in recorder.go
// and centralize the attribute set so every call site stays consistent
// without per-site duplication.
//
// What the spans capture:
//
//   - loomcycle.run          — one span per agent run (top-level or
//     sub-run; sub-runs nest as children of the parent's iteration span
//     via context propagation).
//   - loomcycle.iteration    — one per turn within loop.Run.
//   - loomcycle.provider.call— one per HTTP request to a model provider.
//   - loomcycle.tool.call    — one per Dispatcher.Execute call.
//   - loomcycle.mcp.call     — one per MCP tools/call (nested under
//     the outer tool.call).
//
// What spans do NOT carry: transcript bodies, tool inputs, tool
// results, system prompts, user prompts. Only shape (run/agent/user/
// run ID, provider name, model name, tool name), cost (input/output/
// cache tokens), and latency. Secrets stay out of telemetry by design.
package otel

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the single fixed name every loomcycle span uses. Operators
// filtering Jaeger by "instrumentation library" or Tempo by
// "service.namespace" see one stable identifier.
const TracerName = "github.com/denn-gubsky/loomcycle"

// Config drives the Init bootstrap. Mirrors the cfg.Env subset the
// command package owns; declared here so the package doesn't take a
// hard dependency on internal/config.
type Config struct {
	// Endpoint is the OTLP/HTTP endpoint (no path — otlptracehttp appends
	// /v1/traces). Empty disables the entire subsystem.
	Endpoint string
	// Headers are appended to every OTLP request (collector auth).
	Headers map[string]string
	// ServiceName populates the `service.name` resource attribute.
	// Defaults to "loomcycle" when empty.
	ServiceName string
	// ServiceVersion populates the `service.version` resource attribute.
	// Sourced from the binary's buildVersion at boot.
	ServiceVersion string
	// SamplerRatio is the head-based sampling ratio. 1.0 captures every
	// span; 0.1 captures ~10%. Always respects parent sampling decisions.
	// Clamped to [0,1] by the caller before reaching here.
	SamplerRatio float64
}

var (
	initOnce sync.Once
	// shutdownFn is the per-process shutdown closure returned by Init.
	// Stored package-private so callers can ignore the return value when
	// they don't manage lifecycle (tests, short-lived helpers); the
	// canonical lifecycle owner is cmd/loomcycle/main.go which keeps the
	// return value and defers on SIGTERM.
	shutdownFn func(context.Context) error
)

// Init installs the global OpenTelemetry tracer provider. When
// cfg.Endpoint is empty, this is a no-op + returns a no-op shutdown
// closer (safe to call on every code path). Otherwise constructs an
// OTLP/HTTP exporter pointed at cfg.Endpoint, wires a TracerProvider
// with the configured sampler + service attributes, and registers it
// globally so all `otel.Tracer(...)` calls across the codebase pick it
// up.
//
// Idempotent: subsequent calls after the first are no-ops. The
// loomcycle binary boots once, so this is the right tradeoff; tests
// using the in-memory exporter call SetTracerProviderForTest directly.
//
// Returns the shutdown function. The caller MUST defer it (the OTLP
// exporter batches; in-flight spans are lost if the process exits
// without flushing). A 5-second deadline on shutdown is the right call
// — the exporter respects ctx.
func Init(cfg Config) (func(context.Context) error, error) {
	var (
		fn  func(context.Context) error
		err error
	)
	initOnce.Do(func() {
		fn, err = doInit(cfg)
		shutdownFn = fn
	})
	if fn == nil && err == nil {
		// Repeat-call after a successful first Init: return the previously
		// stored shutdown so the caller still has the lifecycle handle.
		fn = shutdownFn
		if fn == nil {
			fn = noopShutdown
		}
	}
	return fn, err
}

func doInit(cfg Config) (func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		// No-op mode: leave the global tracer provider at its zero-value
		// no-op. Every call site's tracer.Start(ctx, name) returns a
		// no-op span at zero cost. Set propagator anyway so any external
		// trace context coming in via HTTP headers is preserved (cheap +
		// makes future opt-in seamless).
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return noopShutdown, nil
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "loomcycle"
	}

	// Build resource attrs. service.name + service.version are the two
	// fields every backend (Jaeger / Tempo / Honeycomb) groups on.
	resAttrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
	}
	if cfg.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	res, err := sdkresource.New(
		context.Background(),
		sdkresource.WithAttributes(resAttrs...),
	)
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: build resource: %w", err)
	}

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(stripScheme(cfg.Endpoint)),
	}
	if isInsecureEndpoint(cfg.Endpoint) {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	exporter, err := otlptrace.New(
		context.Background(),
		otlptracehttp.NewClient(exporterOpts...),
	)
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: build exporter: %w", err)
	}

	// ParentBased(TraceIDRatioBased(ratio)) is the recommended sampler:
	// respects upstream decisions when a parent span is sampled (so a
	// trace that crosses replicas stays whole), applies the ratio to
	// untraced roots.
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplerRatio))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// Tracer returns the loomcycle-named tracer. Call sites use this rather
// than otel.Tracer(TracerName) directly so future changes to the tracer
// name or any wrapping logic happen in one place.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// SetTracerProviderForTest swaps the global tracer provider for the
// duration of a test. Returns a cleanup function. Tests use this with
// `tracetest.NewInMemoryExporter()` to assert what spans landed.
// Production code should call Init instead.
func SetTracerProviderForTest(tp trace.TracerProvider) func() {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return func() { otel.SetTracerProvider(prev) }
}

func noopShutdown(context.Context) error { return nil }

// isInsecureEndpoint returns true when the endpoint is plain HTTP. The
// otlptracehttp exporter defaults to TLS; for `http://localhost:4318`
// (the standard local-Jaeger setup) we must opt out explicitly.
func isInsecureEndpoint(endpoint string) bool {
	const httpPrefix = "http://"
	if len(endpoint) < len(httpPrefix) {
		return false
	}
	return endpoint[:len(httpPrefix)] == httpPrefix
}

// stripScheme strips the http:// or https:// prefix that operators
// naturally include. The otlptracehttp exporter wants "host:port",
// not a full URL.
func stripScheme(endpoint string) string {
	const (
		httpPrefix  = "http://"
		httpsPrefix = "https://"
	)
	if len(endpoint) >= len(httpsPrefix) && endpoint[:len(httpsPrefix)] == httpsPrefix {
		return endpoint[len(httpsPrefix):]
	}
	if len(endpoint) >= len(httpPrefix) && endpoint[:len(httpPrefix)] == httpPrefix {
		return endpoint[len(httpPrefix):]
	}
	return endpoint
}
