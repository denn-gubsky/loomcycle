package otel

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// With the SDK's attribute count limit lowered (OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT
// does it), a span keeps only its first attributes. The no-trace marker must
// survive with the tenant, and a span that lost attributes without carrying
// the marker is not routed: whether it was a no-trace run's cannot be told.
func TestTenantRouter_AnAttributeLimitNeverLetsANoTraceSpanThrough(t *testing.T) {
	clearOTELEnv(t)
	for _, limit := range []int{1, 2, 3, 4} {
		c := newFakeCollector(t)
		r := fastRouter(TenantRouterConfig{})
		r.SetDestination("acme", dest(c, nil))
		tp := routedProvider(t, r, sdktrace.WithRawSpanLimits(func() sdktrace.SpanLimits {
			l := sdktrace.NewSpanLimits()
			l.AttributeCountLimit = limit
			return l
		}()))

		ctx, run := RecordRunStart(context.Background(), RunStartAttrs{RunID: "secret", AgentID: "a1", AgentName: "quiet", Tenant: "acme", SessionID: "s1", NoTrace: true})
		_, mc := RecordModelCall(ctx, ModelCallAttrs{Provider: "anthropic", Model: "m", Purpose: "turn"})
		mc.End()
		_, tool := RecordToolCall(ctx, ToolCallAttrs{Name: "Read"})
		tool.End()
		run.End()
		// An ordinary span that lost attributes to the limit and carries no
		// marker: not routed either.
		_, plain := RecordToolCall(WithTenant(context.Background(), "acme"), ToolCallAttrs{Name: "Grep"})
		plain.SetAttributes(attribute.String("x.a", "1"), attribute.String("x.b", "2"), attribute.String("x.c", "3"), attribute.String("x.d", "4"))
		plain.End()

		if err := tp.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = r.ForceFlush(context.Background())
		time.Sleep(50 * time.Millisecond)
		if got := c.got(); len(got) != 0 {
			t.Errorf("limit %d: the tenant's collector got %v", limit, spanNames(got))
		}
		if st := r.Status("acme"); st.Dropped == 0 {
			t.Errorf("limit %d: the unroutable span was not counted as dropped: %+v", limit, st)
		}
	}
}

// A destination changed while spans end: the race detector must find no
// unsynchronised access to what the router keeps per tenant.
func TestTenantRouter_ChangingADestinationWhileSpansEndIsRaceFree(t *testing.T) {
	clearOTELEnv(t)
	c1, c2 := newFakeCollector(t), newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(c1, nil))
	tp := routedProvider(t, r)
	tr := tp.Tracer("t")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				r.SetDestination("acme", dest(c2, nil))
			} else {
				r.SetDestination("acme", dest(c1, nil))
			}
		}
	}()
	for i := 0; i < 500; i++ {
		_, s := tr.Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
		s.End()
	}
	close(stop)
	wg.Wait()
}
