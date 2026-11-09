// Package otel is loomcycle's distributed-tracing bootstrap. It installs an
// OTLP exporter (HTTP/protobuf or gRPC) against the global OpenTelemetry
// tracer provider when the operator sets OTEL_EXPORTER_OTLP_ENDPOINT (or its
// _TRACES_ twin); otherwise it leaves the global provider as a no-op so every
// `tracer.Start(ctx, name)` call across the codebase costs nothing.
//
// Configuration is the standard OTEL_* environment, which the OTLP exporter
// reads itself. loomcycle decides only whether tracing is on, the protocol,
// the sampler, the resource and the batching bounds (ResolveConfig), and adds
// LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE, LOOMCYCLE_OTEL_INBOUND_CONTEXT
// and LOOMCYCLE_ENVIRONMENT.
//
// Init is the public entry point. Recorder
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
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
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

var (
	initOnce sync.Once
	// shutdownFn is the per-process shutdown closure returned by Init.
	// Stored package-private so callers can ignore the return value when
	// they don't manage lifecycle (tests, short-lived helpers); the
	// canonical lifecycle owner is cmd/loomcycle/main.go which keeps the
	// return value and defers on SIGTERM.
	shutdownFn func(context.Context) error
)

// Init installs the global propagator, the inbound trace-context mode and,
// when cfg.Enabled, an OTLP exporter behind a bounded batch processor as the
// global tracer provider. Disabled, the provider stays the no-op default and
// the returned shutdown does nothing (safe to defer on every path).
//
// Idempotent: subsequent calls after the first are no-ops. The
// loomcycle binary boots once, so this is the right tradeoff; tests
// using the in-memory exporter call SetTracerProviderForTest directly.
//
// Returns the shutdown function. The caller MUST defer it (the batch
// processor holds spans; they are lost if the process exits without
// flushing). The exporter respects ctx, so a short deadline bounds it.
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
	// Set whether or not tracing exports: a caller's trace context is read
	// (after authentication) and carried even when this process records
	// nothing, so turning tracing on later changes nothing else.
	otel.SetTextMapPropagator(newPropagator(cfg.Propagators))
	setInboundMode(cfg.Inbound)
	if !cfg.Enabled {
		return noopShutdown, nil
	}
	errs := newErrorHandler(slog.Default(), time.Now)
	tp, err := newTracerProvider(context.Background(), cfg, &stats, errs)
	if err != nil {
		return noopShutdown, err
	}
	otel.SetErrorHandler(errs)
	otel.SetTracerProvider(tp)
	stats.enabled.Store(true)
	return tp.Shutdown, nil
}

// newTracerProvider builds the exporting provider without installing it.
func newTracerProvider(ctx context.Context, cfg Config, st *pipelineStats, errs *errorHandler) (*sdktrace.TracerProvider, error) {
	sampler, err := Sampler(cfg.SamplerName, cfg.SamplerArg)
	if err != nil {
		return nil, err
	}
	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	if cfg.HeadersFile != "" {
		if headers, err = ReadHeadersFile(cfg.HeadersFile); err != nil {
			return nil, err
		}
	}
	exp, err := newExporter(ctx, cfg.Protocol, headers)
	if err != nil {
		return nil, err
	}
	bsp := sdktrace.NewBatchSpanProcessor(&countingExporter{next: exp, st: st, errs: errs},
		sdktrace.WithMaxQueueSize(cfg.MaxQueueSize),
		sdktrace.WithMaxExportBatchSize(cfg.MaxExportBatchSize),
		sdktrace.WithExportTimeout(cfg.ExportTimeout),
		sdktrace.WithBatchTimeout(cfg.ScheduleDelay),
	)
	return sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(&gateProcessor{next: bsp, st: st, maxInFlight: int64(cfg.MaxQueueSize)}),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	), nil
}

// newExporter builds the OTLP exporter for protocol. No endpoint, TLS or
// timeout option is passed: each would override the OTEL_EXPORTER_OTLP_*
// variables the exporter reads itself. headers (from the headers file) are
// the one exception, and ResolveConfig refuses them beside the env headers.
func newExporter(ctx context.Context, protocol Protocol, headers map[string]string) (sdktrace.SpanExporter, error) {
	var (
		exp sdktrace.SpanExporter
		err error
	)
	switch protocol {
	case ProtocolGRPC:
		var opts []otlptracegrpc.Option
		if len(headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(headers))
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	default:
		var opts []otlptracehttp.Option
		if len(headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
		exp, err = otlptracehttp.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("otel: build %s exporter: %w", protocol, err)
	}
	return exp, nil
}

// newResource is loomcycle's defaults overridden by OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES: each later source wins over the earlier, so the
// environment always has the last word.
func newResource(ctx context.Context, cfg Config) (*sdkresource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName("loomcycle")}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(cfg.InstanceID))
	}
	if cfg.Environment != "" {
		// The current semconv name; the pinned v1.26 package predates it.
		attrs = append(attrs, attribute.String("deployment.environment.name", cfg.Environment))
	}
	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(attrs...),
		sdkresource.WithFromEnv(),
		sdkresource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}
	return res, nil
}

func newPropagator(names []string) propagation.TextMapPropagator {
	var ps []propagation.TextMapPropagator
	for _, n := range names {
		switch n {
		case "tracecontext":
			ps = append(ps, propagation.TraceContext{})
		case "baggage":
			ps = append(ps, propagation.Baggage{})
		}
	}
	return propagation.NewCompositeTextMapPropagator(ps...)
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
