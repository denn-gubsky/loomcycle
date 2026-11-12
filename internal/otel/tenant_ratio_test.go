package otel

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceIDKeeps_AgreesWithTraceIDRatioBased(t *testing.T) {
	ratios := []float64{-1, 0, 1e-9, 0.001, 0.1, 0.25, 1.0 / 3, 0.5, 0.9, 0.999999, 1, 2}
	for i := 0; i < 20000; i++ {
		var id trace.TraceID
		_, _ = rand.Read(id[:])
		r := ratios[i%len(ratios)]
		want := sdktrace.TraceIDRatioBased(r).ShouldSample(sdktrace.SamplingParameters{TraceID: id}).Decision == sdktrace.RecordAndSample
		if got := traceIDKeeps(id, r); got != want {
			t.Fatalf("traceIDKeeps(%s, %v) = %v, TraceIDRatioBased says %v", id, r, got, want)
		}
	}
	// The bound itself: a trace id exactly at ratio × 2^63 is not kept.
	var at trace.TraceID
	at[8] = 0x80 // low 8 bytes 0x80…0 >> 1 == 2^62 == 0.5 × 2^63
	if traceIDKeeps(at, 0.5) || !traceIDKeeps(at, 0.5000001) {
		t.Error("the bound is not the one TraceIDRatioBased uses")
	}
}

// twoDestinations is the exporting provider Init builds with tenant export on:
// the operator's collector behind its own pipeline, and tenant acme's behind
// the router at ratio.
type twoDestinations struct {
	tp       *sdktrace.TracerProvider
	r        *TenantRouter
	op, acme *fakeCollector
	st       *pipelineStats
}

func newTwoDestinations(t *testing.T, sampler string, arg float64, operatorTenantRatios map[string]float64, ratio float64) *twoDestinations {
	t.Helper()
	clearOTELEnv(t)
	d := &twoDestinations{op: newFakeCollector(t), acme: newFakeCollector(t), st: &pipelineStats{}}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", d.op.srv.URL)
	d.r = fastRouter(TenantRouterConfig{QueueSize: 8192})
	dst := dest(d.acme, nil)
	dst.Ratio = ratio
	d.r.SetDestination("acme", dst)
	cfg := Config{
		Enabled: true, Protocol: ProtocolHTTPProtobuf, SamplerName: sampler, SamplerArg: arg,
		TenantSamplerRatios: operatorTenantRatios, TenantRouter: d.r,
		MaxQueueSize: 8192, MaxExportBatchSize: 512, ExportTimeout: 5 * time.Second, ScheduleDelay: 20 * time.Millisecond,
	}
	tp, err := newTracerProvider(context.Background(), cfg, d.st, newErrorHandler(slog.New(slog.DiscardHandler), time.Now))
	if err != nil {
		t.Fatal(err)
	}
	prev := tenantRouting.Load()
	installTenantRouter(tp, d.r)
	d.tp = tp
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		tenantRouting.Store(prev)
	})
	return d
}

// traces starts n traces of tenant, each a root with a child and a grandchild
// (every span carrying the tenant, as the recorder helpers stamp it), and
// returns their trace ids.
func (d *twoDestinations) traces(n int, tenant string) []trace.TraceID {
	tr := d.tp.Tracer("t")
	attrs := trace.WithAttributes(attribute.String(AttrTenant, tenant))
	ids := make([]trace.TraceID, 0, n)
	for i := 0; i < n; i++ {
		ctx, root := tr.Start(context.Background(), "root", attrs)
		cctx, child := tr.Start(ctx, "child", attrs)
		_, grand := tr.Start(cctx, "grandchild", attrs)
		grand.End()
		child.End()
		root.End()
		ids = append(ids, root.SpanContext().TraceID())
	}
	return ids
}

func (d *twoDestinations) flush(t *testing.T) {
	t.Helper()
	if err := d.tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// keptIDs is the hex ids of the traces in ids that ratio keeps.
func keptIDs(ids []trace.TraceID, ratio float64) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		if traceIDKeeps(id, ratio) {
			out[id.String()] = true
		}
	}
	return out
}

// assertTraces checks that c holds exactly the traces want, each whole: perTrace
// spans, one root, every other span's parent among them, and no trace-state
// marker.
func assertTraces(t *testing.T, who string, c *fakeCollector, want map[string]bool, perTrace int) {
	t.Helper()
	by := map[string][]collectedSpan{}
	for _, s := range c.got() {
		by[s.traceID] = append(by[s.traceID], s)
		if strings.Contains(s.traceState, tenantOnlyKey) {
			t.Errorf("%s: span %s carries the trace-state marker %q", who, s.name, s.traceState)
		}
	}
	for id := range by {
		if !want[id] {
			t.Errorf("%s holds trace %s, which it should not", who, id)
		}
	}
	for id := range want {
		spans := by[id]
		if len(spans) != perTrace {
			t.Errorf("%s: trace %s has %d spans, want %d (a fragment)", who, id, len(spans), perTrace)
			continue
		}
		ids := map[string]bool{}
		for _, s := range spans {
			ids[s.spanID] = true
		}
		roots := 0
		for _, s := range spans {
			switch {
			case s.parentID == "":
				roots++
			case !ids[s.parentID]:
				t.Errorf("%s: trace %s span %s has its parent missing", who, id, s.name)
			}
		}
		if roots != 1 {
			t.Errorf("%s: trace %s has %d roots", who, id, roots)
		}
	}
}

func (d *twoDestinations) waitSpans(t *testing.T, opWant, acmeWant int) {
	t.Helper()
	waitFor(t, "both collectors", func() bool {
		return len(d.op.got()) >= opWant && len(d.acme.got()) >= acmeWant
	})
	d.flush(t)
}

// The operator at 0.1, a tenant raising its own to 1: the operator still
// receives exactly the traces its own sampler keeps, and counts no others in
// its pipeline's statistics; the tenant receives every trace.
func TestTenantExport_OperatorExportUnchangedWhenATenantRaisesItsRatio(t *testing.T) {
	d := newTwoDestinations(t, "parentbased_traceidratio", 0.1, nil, 1)
	ids := d.traces(400, "acme")
	opWant, all := keptIDs(ids, 0.1), keptIDs(ids, 1)
	if len(opWant) == 0 || len(opWant) == len(ids) {
		t.Fatalf("the operator's ratio kept %d of %d traces; the test needs some of each", len(opWant), len(ids))
	}
	d.flush(t)
	d.waitSpans(t, 3*len(opWant), 3*len(ids))
	assertTraces(t, "operator", d.op, opWant, 3)
	assertTraces(t, "tenant", d.acme, all, 3)
	n := uint64(3 * len(opWant))
	if ended, exported, dropped := d.st.ended.Load(), d.st.exported.Load(), d.st.dropped.Load(); ended != n || exported != n || dropped != 0 {
		t.Errorf("operator stats ended=%d exported=%d dropped=%d, want %d %d 0: spans kept for the tenant were counted",
			ended, exported, dropped, n, n)
	}
}

// Both destinations keep whole traces even where the operator's sampler is
// not parent-based: with always_off, every span a tenant's ratio keeps is a
// child the operator's sampler alone would drop; with the operator's own
// ratio for the tenant over an always_off base, the operator keeps the roots
// it always kept, and the tenant still receives the whole of each trace.
func TestTenantExport_BothDestinationsHoldWholeTraces(t *testing.T) {
	t.Run("always_off", func(t *testing.T) {
		d := newTwoDestinations(t, "always_off", 0, nil, 0.5)
		ids := d.traces(200, "acme")
		want := keptIDs(ids, 0.5)
		d.flush(t)
		d.waitSpans(t, 0, 3*len(want))
		assertTraces(t, "operator", d.op, map[string]bool{}, 3)
		assertTraces(t, "tenant", d.acme, want, 3)
	})
	t.Run("operator tenant ratio over always_off", func(t *testing.T) {
		d := newTwoDestinations(t, "always_off", 0, map[string]float64{"acme": 0.3}, 0.6)
		ids := d.traces(300, "acme")
		opWant, want := keptIDs(ids, 0.3), keptIDs(ids, 0.6)
		d.flush(t)
		d.waitSpans(t, len(opWant), 3*len(want))
		// The operator's own sampler decides the roots at 0.3 and drops every
		// child (always_off): its copy is unchanged, roots alone.
		assertTraces(t, "operator", d.op, opWant, 1)
		assertTraces(t, "tenant", d.acme, want, 3)
	})
}

// A tenant asking for less than the operator samples receives only its share,
// by trace id, and the operator's copy is untouched.
func TestTenantExport_TenantRatioBelowTheOperatorsThinsOnlyTheTenant(t *testing.T) {
	d := newTwoDestinations(t, "parentbased_always_on", 1, nil, 0.25)
	ids := d.traces(300, "acme")
	want := keptIDs(ids, 0.25)
	d.flush(t)
	d.waitSpans(t, 3*len(ids), 3*len(want))
	assertTraces(t, "operator", d.op, keptIDs(ids, 1), 3)
	assertTraces(t, "tenant", d.acme, want, 3)
	if st := d.r.Status("acme"); st.Dropped != 0 {
		t.Errorf("thinned spans were counted dropped: %+v", st)
	}
}

// A ratio changed at runtime applies to the next traces, without rebuilding
// the tenant's pipeline; a removed destination raises nothing more.
func TestTenantExport_RatioChangeAtRuntimeTakesEffect(t *testing.T) {
	d := newTwoDestinations(t, "always_off", 0, nil, 0.2)
	first := d.traces(200, "acme")
	want1 := keptIDs(first, 0.2)
	d.flush(t)
	d.waitSpans(t, 0, 3*len(want1))
	assertTraces(t, "tenant at 0.2", d.acme, want1, 3)
	d.r.mu.RLock()
	pipe := d.r.pipes["acme"]
	d.r.mu.RUnlock()

	raised := dest(d.acme, nil)
	raised.Ratio = 1
	d.r.SetDestination("acme", raised)
	second := d.traces(100, "acme")
	d.flush(t)
	want2 := keptIDs(first, 0.2)
	for id := range keptIDs(second, 1) {
		want2[id] = true
	}
	d.waitSpans(t, 0, 3*len(want2))
	assertTraces(t, "tenant at 1", d.acme, want2, 3)
	d.r.mu.RLock()
	same := d.r.pipes["acme"] == pipe
	d.r.mu.RUnlock()
	if !same {
		t.Error("a ratio change rebuilt the tenant's pipeline")
	}

	d.r.RemoveDestination("acme")
	_, root := d.tp.Tracer("t").Start(context.Background(), "root", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	root.End()
	if root.SpanContext().IsSampled() {
		t.Error("a tenant with no destination still raised a trace")
	}
}

// A destination exporting metrics only receives no spans, so its ratio raises
// none: the operator's sampler alone decides, and the trace is not routed.
// Adding traces to the same destination raises them from the next span on.
func TestTenantExport_MetricsOnlyDestinationRaisesNoSpans(t *testing.T) {
	d := newTwoDestinations(t, "always_off", 0, nil, 1)
	metricsOnly := dest(d.acme, nil)
	metricsOnly.Ratio, metricsOnly.Signals = 1, []string{SignalMetrics}
	d.r.SetDestination("acme", metricsOnly)
	if r, ok := d.r.exportRatio("acme"); ok {
		t.Errorf("a metrics-only destination published ratio %v", r)
	}
	tr := d.tp.Tracer("t")
	acme := trace.WithAttributes(attribute.String(AttrTenant, "acme"))
	_, root := tr.Start(context.Background(), "root", acme)
	root.End()
	if root.SpanContext().IsSampled() {
		t.Error("a metrics-only destination raised a trace the operator's sampler drops")
	}

	both := metricsOnly
	both.Signals = []string{SignalTraces, SignalMetrics}
	d.r.SetDestination("acme", both)
	if r, ok := d.r.exportRatio("acme"); !ok || r != 1 {
		t.Errorf("a destination with traces published ratio %v, %v; want 1, true", r, ok)
	}
	_, root = tr.Start(context.Background(), "root", acme)
	root.End()
	if !root.SpanContext().IsSampled() {
		t.Error("adding traces to the destination raised no trace")
	}
}

// A caller's trace state cannot carry the marker in and so hide its trace
// from the operator's collector.
func TestTenantExport_ACallersMarkerCannotHideItsTraceFromTheOperator(t *testing.T) {
	d := newTwoDestinations(t, "parentbased_always_on", 1, nil, 1)
	ts, err := trace.ParseTraceState(tenantOnlyKey + "=1,vendor=x")
	if err != nil {
		t.Fatal(err)
	}
	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4}, TraceFlags: trace.FlagsSampled, TraceState: ts, Remote: true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), remote)
	attrs := trace.WithAttributes(attribute.String(AttrTenant, "acme"))
	cctx, run := d.tp.Tracer("t").Start(ctx, "run", attrs)
	_, child := d.tp.Tracer("t").Start(cctx, "child", attrs)
	child.End()
	run.End()
	d.waitSpans(t, 2, 2)
	for _, s := range d.op.got() {
		if strings.Contains(s.traceState, tenantOnlyKey) || !strings.Contains(s.traceState, "vendor=x") {
			t.Errorf("operator span %s trace state %q: want the caller's without the marker", s.name, s.traceState)
		}
	}
	if n := len(d.op.got()); n != 2 {
		t.Errorf("the operator's collector got %d spans of the caller's trace, want 2", n)
	}
}

// A no-trace run is not raised for its tenant (nothing of it is routed there),
// while an ordinary run of the same tenant is.
func TestTenantExport_NoTraceRunIsNotRaised(t *testing.T) {
	d := newTwoDestinations(t, "always_off", 0, nil, 1)
	t.Cleanup(SetTracerProviderForTest(d.tp))
	_, nt := RecordRunStart(context.Background(), RunStartAttrs{RunID: "secret", Tenant: "acme", NoTrace: true})
	nt.End()
	if nt.SpanContext().IsSampled() {
		t.Error("a no-trace run was sampled for its tenant")
	}
	_, ok := RecordRunStart(context.Background(), RunStartAttrs{RunID: "plain", Tenant: "acme"})
	ok.End()
	d.waitSpans(t, 0, 1)
	got := d.acme.got()
	if len(got) != 1 || got[0].attrs[AttrRunID] != "plain" {
		t.Errorf("the tenant's collector got %v, want only the ordinary run", spanNames(got))
	}
}

// A trace kept only for a tenant is none of the operator's: a run records no
// trace id for it, and no traceparent is sent to an MCP server. A trace the
// operator's own sampler keeps (beta's) is linked and propagated as before.
func TestTenantExport_ATenantOnlyTraceIsNeitherLinkedNorPropagated(t *testing.T) {
	d := newTwoDestinations(t, "always_off", 0, map[string]float64{"beta": 1}, 1)
	t.Cleanup(SetTracerProviderForTest(d.tp))
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })
	for _, c := range []struct {
		tenant string
		linked bool
	}{{"beta", true}, {"acme", false}} {
		ctx, run := RecordRunStart(context.Background(), RunStartAttrs{RunID: "r", Tenant: c.tenant})
		if !run.SpanContext().IsSampled() {
			t.Fatalf("%s: the run was not sampled", c.tenant)
		}
		h := http.Header{}
		InjectTraceContext(ctx, h)
		if got := SampledTraceID(ctx) != ""; got != c.linked {
			t.Errorf("%s: SampledTraceID set = %v, want %v", c.tenant, got, c.linked)
		}
		if got := h.Get("traceparent") != ""; got != c.linked {
			t.Errorf("%s: traceparent sent = %v, want %v (%v)", c.tenant, got, c.linked, h)
		}
		run.End()
	}
}

// A span with no tenant inside a trace kept for its tenant (work opened
// without the run's context) is kept too, so the tenant's spans beneath it
// still reach the tenant.
func TestTenantExport_AnUntenantedSpanInsideATenantOnlyTraceKeepsItsSubtree(t *testing.T) {
	d := newTwoDestinations(t, "always_off", 0, nil, 1)
	tr := d.tp.Tracer("t")
	acme := trace.WithAttributes(attribute.String(AttrTenant, "acme"))
	ctx, root := tr.Start(context.Background(), "root", acme)
	mctx, mid := tr.Start(ctx, "untenanted")
	_, leaf := tr.Start(mctx, "leaf", acme)
	if !mid.SpanContext().IsSampled() || !leaf.SpanContext().IsSampled() {
		t.Error("a span below an untenanted one in a tenant-only trace was dropped")
	}
	leaf.End()
	mid.End()
	root.End()
	d.waitSpans(t, 0, 2)
	if got := spanNames(d.acme.got()); len(got) != 2 {
		t.Errorf("the tenant's collector got %v, want root and leaf", got)
	}
}

// Gate off, the operator's sampler and pipeline are what they were: no
// wrapper, and a caller's trace state passes through whatever it holds.
func TestTenantExport_GateOffLeavesSamplerAndOperatorPipelineUnchanged(t *testing.T) {
	op := NewTenantSampler(sdktrace.ParentBased(sdktrace.AlwaysSample()), map[string]float64{"acme": 0.5})
	if withTenantExport(op, nil) != op {
		t.Error("gate off: the operator's sampler is wrapped")
	}
	clearOTELEnv(t)
	c := newFakeCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.srv.URL)
	st := &pipelineStats{}
	tp, err := newTracerProvider(context.Background(), Config{
		Enabled: true, Protocol: ProtocolHTTPProtobuf, SamplerName: "parentbased_always_on", SamplerArg: 1,
		MaxQueueSize: 64, MaxExportBatchSize: 64, ExportTimeout: 5 * time.Second, ScheduleDelay: 20 * time.Millisecond,
	}, st, newErrorHandler(slog.New(slog.DiscardHandler), time.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ts, _ := trace.ParseTraceState(tenantOnlyKey + "=1")
	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}, TraceFlags: trace.FlagsSampled, TraceState: ts, Remote: true,
	})
	_, s := tp.Tracer("t").Start(trace.ContextWithRemoteSpanContext(context.Background(), remote), "run",
		trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := c.got()
	if len(got) != 1 || got[0].traceState != tenantOnlyKey+"=1" {
		t.Fatalf("operator got %+v, want the span with the caller's trace state as sent", got)
	}
	if st.ended.Load() != 1 || st.exported.Load() != 1 {
		t.Errorf("stats ended=%d exported=%d, want 1 1", st.ended.Load(), st.exported.Load())
	}
}
