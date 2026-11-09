package otel_test

import (
	"context"
	"sync"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// attrOf returns the value of key on span, and whether the span carries it.
func attrOf(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func spanNamed(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %s span among %d", name, len(spans))
	return tracetest.SpanStub{}
}

func TestRecorders_StampTenantOnEverySpanOfARun(t *testing.T) {
	exp := withInMemoryExporter(t)
	ctx, run := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1", Tenant: "acme"})
	ictx, iter := lcotel.RecordIteration(ctx, 1)
	_, prov := lcotel.RecordProviderCall(ictx, lcotel.ProviderCallAttrs{Provider: "anthropic"})
	tctx, tool := lcotel.RecordToolCall(ictx, "mcp__srv__x")
	_, mcp := lcotel.RecordMCPCall(tctx, "srv", "x")
	_, mem := lcotel.RecordMemorySearch(tctx, "inprocess")
	// A sub-run started under the parent's span names no tenant of its own.
	_, sub := lcotel.RecordRunStart(tctx, lcotel.RunStartAttrs{RunID: "r2"})
	for _, s := range []trace.Span{sub, mem, mcp, tool, prov, iter, run} {
		s.End()
	}
	_, cons := lcotel.RecordMemoryConsolidate(context.Background(), lcotel.MemoryConsolidateAttrs{Tenant: "beta", Scope: "user"})
	cons.End()

	spans := exp.GetSpans()
	if len(spans) != 8 {
		t.Fatalf("got %d spans, want 8", len(spans))
	}
	for _, s := range spans {
		want := "acme"
		if s.Name == lcotel.SpanMemoryConsolidate {
			want = "beta"
		}
		v, ok := attrOf(s, lcotel.AttrTenant)
		if !ok || v.AsString() != want {
			t.Errorf("%s: %s = %q (present %v), want %q", s.Name, lcotel.AttrTenant, v.AsString(), ok, want)
		}
	}
}

func TestRecorders_OmitTenantOutsideATenantsWork(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, run := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1"})
	run.End()
	_, tool := lcotel.RecordToolCall(context.Background(), "Read")
	tool.End()
	for _, s := range exp.GetSpans() {
		if v, ok := attrOf(s, lcotel.AttrTenant); ok {
			t.Errorf("%s carries %s=%q with no tenant anywhere", s.Name, lcotel.AttrTenant, v.AsString())
		}
	}
}

// recordingSampler keeps the start attributes it was asked about.
type recordingSampler struct {
	mu    sync.Mutex
	attrs map[string][]attribute.KeyValue
}

func (r *recordingSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	r.mu.Lock()
	r.attrs[p.Name] = p.Attributes
	r.mu.Unlock()
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (r *recordingSampler) Description() string { return "recording" }

func TestRecordRunStart_TenantIsAStartAttribute(t *testing.T) {
	rs := &recordingSampler{attrs: map[string][]attribute.KeyValue{}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(rs))
	t.Cleanup(lcotel.SetTracerProviderForTest(tp))
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1", Tenant: "acme"})
	span.End()
	for _, kv := range rs.attrs[lcotel.SpanRun] {
		if string(kv.Key) == lcotel.AttrTenant && kv.Value.AsString() == "acme" {
			return
		}
	}
	t.Fatalf("the sampler saw %v for the run span, without %s=acme", rs.attrs[lcotel.SpanRun], lcotel.AttrTenant)
}
