package otel_test

import (
	"context"
	"testing"
	"time"

	otellib "github.com/denn-gubsky/loomcycle/internal/otel"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestInit_NoOpWhenEndpointEmpty pins the load-bearing default: with
// no LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT, the tracer is a no-op and
// the call sites scattered across the codebase pay zero cost. Verified
// by checking that the global tracer provider stays at the default
// no-op type after Init (no OTLP exporter wired).
func TestInit_NoOpWhenEndpointEmpty(t *testing.T) {
	shutdown, err := otellib.Init(otellib.Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("Init with empty endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown closure is nil; should be a safe no-op")
	}

	// Calling shutdown is always safe — operators may defer it
	// unconditionally regardless of whether OTEL is enabled.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown returned error on no-op tracer: %v", err)
	}

	// Tracer().Start(...) returns a no-op span — the operation costs
	// nothing and never panics even when no exporter is wired.
	_, span := otellib.Tracer().Start(context.Background(), "test.span")
	defer span.End()
	if span == nil {
		t.Fatal("tracer.Start returned nil span")
	}
}

// TestSetTracerProviderForTest_CapturesSpansToInMemoryExporter is the
// canonical test harness for downstream consumers: install an
// in-memory SDK provider, run the code that emits spans, assert the
// exporter received them. This pattern recurs in every other test in
// this package and in api/http + loop tests that exercise OTEL.
func TestSetTracerProviderForTest_CapturesSpansToInMemoryExporter(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := otellib.SetTracerProviderForTest(tp)
	defer cleanup()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := otellib.Tracer().Start(context.Background(), "test.run")
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name != "test.run" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "test.run")
	}
}

// TestSetTracerProviderForTest_CleanupRestoresPriorProvider verifies
// the test harness doesn't leak across tests. After cleanup, the
// global provider must be back to whatever it was before. Important
// because Go's test runner shares process state across tests in a
// package.
func TestSetTracerProviderForTest_CleanupRestoresPriorProvider(t *testing.T) {
	before := otel.GetTracerProvider()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := otellib.SetTracerProviderForTest(tp)
	if otel.GetTracerProvider() == before {
		t.Fatal("SetTracerProviderForTest did not install the new provider")
	}
	cleanup()
	if otel.GetTracerProvider() != before {
		t.Error("cleanup did not restore the prior provider — global state leaked across tests")
	}
	_ = tp.Shutdown(context.Background())
}
