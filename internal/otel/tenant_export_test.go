package otel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// collectedSpan is one span a fake collector received, flattened.
type collectedSpan struct {
	name     string
	resource map[string]string
	attrs    map[string]string
	events   []map[string]string
	status   string // description
}

// fakeCollector is an OTLP/HTTP collector that records what it is sent.
type fakeCollector struct {
	srv *httptest.Server

	mu      sync.Mutex
	spans   []collectedSpan
	headers []http.Header
	// handler, when set, answers instead of accepting.
	handler func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	c := &fakeCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func kvMap(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		v := kv.GetValue()
		switch {
		case v.GetStringValue() != "":
			out[kv.GetKey()] = v.GetStringValue()
		default:
			out[kv.GetKey()] = strings.TrimSpace(v.String())
		}
	}
	return out
}

func (c *fakeCollector) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	h := c.handler
	c.headers = append(c.headers, r.Header.Clone())
	c.mu.Unlock()
	if h != nil && h(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	for _, rs := range req.GetResourceSpans() {
		res := kvMap(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				cs := collectedSpan{name: s.GetName(), resource: res, attrs: kvMap(s.GetAttributes()), status: s.GetStatus().GetMessage()}
				for _, e := range s.GetEvents() {
					cs.events = append(cs.events, kvMap(e.GetAttributes()))
				}
				c.spans = append(c.spans, cs)
			}
		}
	}
	c.mu.Unlock()
	out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

func (c *fakeCollector) got() []collectedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collectedSpan(nil), c.spans...)
}

func (c *fakeCollector) gotHeaders() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]http.Header(nil), c.headers...)
}

func plainDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func dest(c *fakeCollector, headers map[string]string) TenantDestination {
	return TenantDestination{Endpoint: c.srv.URL, Protocol: ProtocolHTTPProtobuf, Headers: headers, Dial: plainDial}
}

// routedProvider is a tracer provider whose only processor is the router,
// with tenant routing marked on for the test's duration.
func routedProvider(t *testing.T, r *TenantRouter, extra ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	t.Helper()
	prev := tenantRouting.Load()
	tenantRouting.Store(true)
	tp := sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(r)}, extra...)...)
	t.Cleanup(SetTracerProviderForTest(tp))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		tenantRouting.Store(prev)
	})
	return tp
}

func fastRouter(cfg TenantRouterConfig) *TenantRouter {
	if cfg.ScheduleDelay == 0 {
		cfg.ScheduleDelay = 20 * time.Millisecond
	}
	if cfg.ExportTimeout == 0 {
		cfg.ExportTimeout = 2 * time.Second
	}
	return NewTenantRouter(cfg)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func spanNames(spans []collectedSpan) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.name+"/"+s.attrs[AttrTenant])
	}
	return out
}

func TestTenantRouter_EachTenantReceivesOnlyItsOwnSpans(t *testing.T) {
	clearOTELEnv(t)
	ca, cb := newFakeCollector(t), newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(ca, nil))
	r.SetDestination("beta", dest(cb, nil))
	tp := routedProvider(t, r)

	actx, arun := RecordRunStart(context.Background(), RunStartAttrs{RunID: "ra", Tenant: "acme"})
	_, atool := RecordToolCall(actx, ToolCallAttrs{Name: "Read"})
	atool.End()
	arun.End()
	_, brun := RecordRunStart(context.Background(), RunStartAttrs{RunID: "rb", Tenant: "beta"})
	brun.End()
	// Untenanted work: substrate spans and a run with no tenant.
	_, none := RecordRunStart(context.Background(), RunStartAttrs{RunID: "rn"})
	none.End()
	RecordHelpReconcile(context.Background(), 1, 0, 0, 0, false)
	// A tenant with no destination.
	_, g := RecordRunStart(context.Background(), RunStartAttrs{RunID: "rg", Tenant: "gamma"})
	g.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "acme's two spans", func() bool { return len(ca.got()) == 2 })
	waitFor(t, "beta's span", func() bool { return len(cb.got()) == 1 })
	_ = r.ForceFlush(context.Background())
	for _, s := range ca.got() {
		if s.attrs[AttrTenant] != "acme" || s.resource[AttrTenant] != "acme" {
			t.Errorf("acme's collector got %v", spanNames(ca.got()))
		}
	}
	for _, s := range cb.got() {
		if s.attrs[AttrTenant] != "beta" || s.resource[AttrTenant] != "beta" {
			t.Errorf("beta's collector got %v", spanNames(cb.got()))
		}
	}
	if n := len(ca.got()) + len(cb.got()); n != 3 {
		t.Errorf("collectors got %d spans in all, want 3: %v %v", n, spanNames(ca.got()), spanNames(cb.got()))
	}
	if st := r.Status("acme"); !st.Configured || st.Exported != 2 || st.Dropped != 0 || st.LastSuccessAt.IsZero() || st.LastError != "" {
		t.Errorf("acme status = %+v", st)
	}
	if st := r.Status("gamma"); st.Configured {
		t.Errorf("gamma has no destination but status says configured: %+v", st)
	}
}

func TestTenantRouter_NoTraceRunSpansAreNeverRouted(t *testing.T) {
	clearOTELEnv(t)
	c := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(c, nil))
	tp := routedProvider(t, r)

	ctx, run := RecordRunStart(context.Background(), RunStartAttrs{RunID: "secret", Tenant: "acme", NoTrace: true})
	ictx, it := RecordIteration(ctx, 1)
	_, mc := RecordModelCall(ictx, ModelCallAttrs{Provider: "anthropic", Model: "m"})
	mc.End()
	tctx, tool := RecordToolCall(ictx, ToolCallAttrs{Name: "Agent"})
	// A sub-run started under the no-trace run names no mode of its own.
	_, sub := RecordRunStart(tctx, RunStartAttrs{RunID: "child", Tenant: "acme"})
	sub.End()
	tool.End()
	it.End()
	run.End()
	// A no-trace model call inside an ordinary run.
	octx, ok := RecordRunStart(context.Background(), RunStartAttrs{RunID: "plain", Tenant: "acme"})
	_, ntc := RecordModelCall(octx, ModelCallAttrs{Provider: "anthropic", Model: "m", NoTrace: true})
	ntc.End()
	ok.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ordinary run's span", func() bool { return len(c.got()) >= 1 })
	_ = r.ForceFlush(context.Background())
	got := c.got()
	if len(got) != 1 || got[0].attrs[AttrRunID] != "plain" {
		t.Fatalf("the tenant's collector got %v, want only the ordinary run's span", spanNames(got))
	}
	if _, marked := got[0].attrs[AttrNoTrace]; marked {
		t.Error("the routed span carries the no-trace marker")
	}
}

func TestTenantRouter_TenantCopyHasMinimalResourceAndAllowedAttributesOnly(t *testing.T) {
	clearOTELEnv(t)
	c := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{ServiceVersion: "1.2.3"})
	r.SetDestination("acme", dest(c, nil))
	opRes := sdkresource.NewSchemaless(attribute.String("host.name", "op-host-7"),
		attribute.String("service.instance.id", "replica-3"), attribute.String("deployment.environment.name", "prod"))
	tp := routedProvider(t, r, sdktrace.WithResource(opRes))

	ctx, run := RecordRunStart(context.Background(), RunStartAttrs{RunID: "r1", Tenant: "acme", AgentName: "a"})
	_, prov := RecordProviderCall(ctx, ProviderCallAttrs{Provider: "code-js", Kind: "synthetic-code", CodeHash: "deadbeef"})
	prov.End()
	RecordQueueWait(run, 5*time.Millisecond)
	run.SetAttributes(attribute.String("loomcycle.some_future_attribute", "x"), attribute.String(AttrUserID, "u-raw"))
	SetSpanError(run, errors.New("dial tcp 10.0.3.4:11434: connection refused"))
	run.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two spans", func() bool { return len(c.got()) == 2 })
	for _, s := range c.got() {
		want := map[string]string{"service.name": "loomcycle", "service.version": "1.2.3", AttrTenant: "acme"}
		if len(s.resource) != len(want) {
			t.Errorf("%s: resource = %v, want exactly %v", s.name, s.resource, want)
		}
		for k, v := range want {
			if s.resource[k] != v {
				t.Errorf("%s: resource %s = %q, want %q", s.name, k, s.resource[k], v)
			}
		}
		for k := range s.attrs {
			if !tenantAttrPolicy[k] {
				t.Errorf("%s: carries %s, which is not the tenant's to see", s.name, k)
			}
		}
		for _, e := range s.events {
			if _, ok := e["exception.message"]; ok {
				t.Errorf("%s: an event carries the error text", s.name)
			}
		}
		if s.status != "" {
			t.Errorf("%s: status description %q reached the tenant", s.name, s.status)
		}
		if s.name == SpanRun && s.attrs[AttrRunID] != "r1" {
			t.Errorf("run span lost its run id: %v", s.attrs)
		}
	}
}

func TestTenantRouter_BlackHoleCollectorBlocksNeitherSpanEndNorOtherTenants(t *testing.T) {
	clearOTELEnv(t)
	release := make(chan struct{})
	hole := newFakeCollector(t)
	hole.handler = func(http.ResponseWriter, *http.Request) bool { <-release; return true }
	ok := newFakeCollector(t)
	// An export into the hole hangs for the whole timeout: a span end that
	// waited on it would take that long, far over the bound asserted below,
	// which leaves room for a loaded machine.
	r := fastRouter(TenantRouterConfig{QueueSize: 64, ExportTimeout: 10 * time.Second})
	r.SetDestination("hole", dest(hole, nil))
	r.SetDestination("ok", dest(ok, nil))
	tp := routedProvider(t, r)
	// Registered after the provider, so it runs before its shutdown: the
	// hole answers and the shutdown does not wait out the timeout.
	t.Cleanup(func() { close(release) })
	tr := tp.Tracer("t")

	// Fill the hole tenant's queue and keep going: every span end returns at
	// once, the overflow dropped and counted.
	const n = 2000
	start := time.Now()
	for i := 0; i < n; i++ {
		_, s := tr.Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "hole")))
		s.End()
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("%d span ends took %v with the tenant's collector hung", n, el)
	}
	// The other tenant's span is exported while the hole still hangs.
	_, s := tr.Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "ok")))
	s.End()
	// The collector holds the span before the exporter has its answer: wait
	// for the status too.
	waitFor(t, "the other tenant's span", func() bool { return len(ok.got()) == 1 && r.Status("ok").Exported == 1 })
	if st := r.Status("hole"); st.Dropped < n-64-1 {
		t.Errorf("hole status = %+v, want at least %d dropped from a full queue", st, n-64-1)
	}
	if st := r.Status("ok"); st.Exported != 1 || st.Dropped != 0 {
		t.Errorf("ok status = %+v", st)
	}
}

func TestTenantRouter_ExportErrorIsRecordedScrubbedAndNeverReachesTheOperatorHandler(t *testing.T) {
	clearOTELEnv(t)
	const secret = "Bearer s3cr3t-tenant-token-value"
	c := newFakeCollector(t)
	c.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		// A collector that echoes the credential back, and talks a lot.
		_, _ = io.WriteString(w, "bad token "+r.Header.Get("Authorization")+" "+strings.Repeat("x", 1000))
		return true
	}
	var handled []error
	var hmu sync.Mutex
	prev := otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { hmu.Lock(); handled = append(handled, err); hmu.Unlock() }))
	t.Cleanup(func() { otel.SetErrorHandler(prev) })

	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(c, map[string]string{"authorization": secret}))
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	waitFor(t, "the failure", func() bool { return r.Status("acme").ExportFailures == 1 })
	st := r.Status("acme")
	if st.LastError == "" || st.LastErrorAt.IsZero() || st.Dropped != 1 {
		t.Fatalf("status = %+v", st)
	}
	if strings.Contains(st.LastError, "s3cr3t") {
		t.Errorf("last_error carries the header value: %q", st.LastError)
	}
	if len([]rune(st.LastError)) > tenantErrorMaxRunes+1 {
		t.Errorf("last_error is %d runes long", len([]rune(st.LastError)))
	}
	if !strings.Contains(st.LastError, "401") {
		t.Errorf("last_error lost the status: %q", st.LastError)
	}
	hmu.Lock()
	defer hmu.Unlock()
	if len(handled) != 0 {
		t.Errorf("the tenant's failure reached the process error handler: %v", handled)
	}
}

func TestTenantRouter_OperatorEnvironmentNeverReachesATenantCollector(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=operator-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "x-operator=operator-secret")
	op := newFakeCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", op.srv.URL)
	for _, headers := range []map[string]string{nil, {"x-scope-orgid": "acme"}} {
		c := newFakeCollector(t)
		r := fastRouter(TenantRouterConfig{})
		r.SetDestination("acme", dest(c, headers))
		tp := routedProvider(t, r)
		_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
		s.End()
		waitFor(t, "the export", func() bool { return len(c.got()) == 1 })
		for _, h := range c.gotHeaders() {
			if h.Get("Authorization") != "" || h.Get("X-Operator") != "" {
				t.Errorf("headers %v: the tenant's collector got the operator's headers: %v", headers, h)
			}
			if headers != nil && h.Get("X-Scope-Orgid") != "acme" {
				t.Errorf("the tenant's own header is missing: %v", h)
			}
		}
	}
	if n := len(op.gotHeaders()); n != 0 {
		t.Errorf("the operator's endpoint got %d tenant exports", n)
	}
}

func TestTenantRouter_RedirectIsNotFollowed(t *testing.T) {
	clearOTELEnv(t)
	target := newFakeCollector(t)
	c := newFakeCollector(t)
	c.handler = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, target.srv.URL+"/v1/traces", http.StatusTemporaryRedirect)
		return true
	}
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(c, map[string]string{"authorization": "Bearer abcdefgh"}))
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	waitFor(t, "the failure", func() bool { return r.Status("acme").ExportFailures == 1 })
	if n := len(target.gotHeaders()); n != 0 {
		t.Errorf("the redirect target was reached %d times", n)
	}
}

func TestTenantRouter_DestinationChangeRebuildsAndRemovalStops(t *testing.T) {
	clearOTELEnv(t)
	c1, c2 := newFakeCollector(t), newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{})
	tp := routedProvider(t, r)
	end := func() {
		_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
		s.End()
	}
	r.SetDestination("acme", dest(c1, nil))
	end()
	waitFor(t, "c1", func() bool { return len(c1.got()) == 1 })
	r.SetDestination("acme", dest(c2, nil))
	end()
	waitFor(t, "c2", func() bool { return len(c2.got()) == 1 })
	if r.Pipelines() != 1 {
		t.Errorf("pipelines = %d after a change, want 1", r.Pipelines())
	}
	r.RemoveDestination("acme")
	end()
	_ = r.ForceFlush(context.Background())
	time.Sleep(100 * time.Millisecond)
	if len(c1.got()) != 1 || len(c2.got()) != 1 || r.Pipelines() != 0 {
		t.Errorf("after removal: c1=%d c2=%d pipelines=%d", len(c1.got()), len(c2.got()), r.Pipelines())
	}
}

func TestTenantRouter_PipelinesAreBoundedLeastRecentlyUsedFirst(t *testing.T) {
	clearOTELEnv(t)
	c := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{MaxPipelines: 2})
	tp := routedProvider(t, r)
	for _, tenant := range []string{"a", "b", "c"} {
		r.SetDestination(tenant, dest(c, nil))
	}
	for _, tenant := range []string{"a", "b", "a", "c"} {
		_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, tenant)))
		s.End()
		time.Sleep(2 * time.Millisecond)
	}
	r.mu.RLock()
	_, hasA := r.pipes["a"]
	_, hasB := r.pipes["b"]
	n := len(r.pipes)
	r.mu.RUnlock()
	if n != 2 || !hasA || hasB {
		t.Errorf("pipelines: %d open, a=%v b=%v; want a and c, b evicted", n, hasA, hasB)
	}
	waitFor(t, "every span", func() bool { return len(c.got()) == 4 })
}

func TestTenantRouter_IdlePipelineIsClosed(t *testing.T) {
	clearOTELEnv(t)
	c := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{IdleTimeout: time.Minute})
	r.SetDestination("acme", dest(c, nil))
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	if r.Pipelines() != 1 {
		t.Fatalf("pipelines = %d", r.Pipelines())
	}
	r.reapIdle(time.Now().Add(2 * time.Minute))
	if r.Pipelines() != 0 {
		t.Fatalf("an idle pipeline stayed open")
	}
	waitFor(t, "the flush on close", func() bool { return len(c.got()) == 1 })
}

func TestTestTenantExport_ReportsSuccessAndAScrubbedFailure(t *testing.T) {
	clearOTELEnv(t)
	c := newFakeCollector(t)
	if err := TestTenantExport(context.Background(), "acme", dest(c, nil), "1.0", 2*time.Second); err != nil {
		t.Fatalf("test export to a working collector: %v", err)
	}
	got := c.got()
	if len(got) != 1 || got[0].name != SpanTelemetryTest || got[0].resource[AttrTenant] != "acme" {
		t.Fatalf("collector got %+v", got)
	}

	const secret = "tok-0123456789abcdef"
	c.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "denied: "+r.Header.Get("X-Api-Key"))
		return true
	}
	d := dest(c, map[string]string{"x-api-key": secret})
	d.Endpoint = strings.Replace(c.srv.URL, "http://", "http://user:pw@", 1) + "/?k=" + secret
	err := TestTenantExport(context.Background(), "acme", d, "1.0", 2*time.Second)
	if err == nil {
		t.Fatal("a refusing collector passed the test")
	}
	for _, leak := range []string{secret, "user:pw", "pw@"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("test error carries %q: %v", leak, err)
		}
	}
}

func TestScrubError_CutsTheBodyAndMasksHeaderValues(t *testing.T) {
	long := strings.Repeat("y", 500)
	err := errors.New("failed to send to https://u:p@h.example/v1/traces?token=abc: 400 Bad Request (body: secret-header-value " + long + ")")
	got := scrubError(err, map[string]string{"authorization": "Basic secret-header-value"})
	if strings.Contains(got, "secret-header-value") || strings.Contains(got, "u:p@") || strings.Contains(got, "token=abc") {
		t.Errorf("scrubbed = %q", got)
	}
	if i := strings.Index(got, "body: "); i < 0 || len([]rune(got[i+6:])) > tenantErrorMaxBodyRunes+1 {
		t.Errorf("body not cut: %q", got)
	}
}

func TestInit_TenantRouterIsInstalledOnlyWhenGiven(t *testing.T) {
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

	shutdown, err := doInit(Config{Propagators: []string{"tracecontext"}, SamplerName: "parentbased_always_on", SamplerArg: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = shutdown(context.Background())
	if TenantRoutingInstalled() {
		t.Error("gate off: a tenant router is installed")
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Error("gate off with tracing off: the global provider is no longer the no-op")
	}
	// Spans opened with the gate off carry no marker, whatever the run.
	exp := withInMemorySyncer(t)
	_, span := RecordRunStart(context.Background(), RunStartAttrs{RunID: "r", Tenant: "acme", NoTrace: true})
	span.End()
	for _, kv := range exp.GetSpans()[0].Attributes {
		if string(kv.Key) == AttrNoTrace {
			t.Error("gate off: a span carries the no-trace marker")
		}
	}

	r := fastRouter(TenantRouterConfig{})
	shutdown, err = doInit(Config{Propagators: []string{"tracecontext"}, SamplerName: "parentbased_always_on", SamplerArg: 1, TenantRouter: r})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if !TenantRoutingInstalled() {
		t.Error("gate on: no tenant router installed")
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); !isSDK {
		t.Error("gate on with operator tracing off: no recording provider for the router to see spans on")
	}
	if Stats().Enabled {
		t.Error("gate on with operator tracing off: the operator's pipeline reports itself enabled")
	}
}

func withInMemorySyncer(t *testing.T) *tracetestExporter {
	t.Helper()
	exp := &tracetestExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := SetTracerProviderForTest(tp)
	t.Cleanup(func() { restore(); _ = tp.Shutdown(context.Background()) })
	return exp
}

// tracetestExporter keeps exported spans' attributes (tracetest is in the
// external test package's imports; this one is internal).
type tracetestExporter struct {
	mu    sync.Mutex
	spans []struct{ Attributes []attribute.KeyValue }
}

func (e *tracetestExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range spans {
		e.spans = append(e.spans, struct{ Attributes []attribute.KeyValue }{s.Attributes()})
	}
	return nil
}

func (e *tracetestExporter) Shutdown(context.Context) error { return nil }

func (e *tracetestExporter) GetSpans() []struct{ Attributes []attribute.KeyValue } {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spans
}

func TestTenantRouter_SelfMetricsCountPerTenant(t *testing.T) {
	clearOTELEnv(t)
	restore, err := SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	c, bad := newFakeCollector(t), newFakeCollector(t)
	bad.handler = func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(http.StatusBadRequest); return true }
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(c, nil))
	r.SetDestination("bad", dest(bad, nil))
	tp := routedProvider(t, r)
	for _, tenant := range []string{"acme", "bad"} {
		_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, tenant)))
		s.End()
	}
	waitFor(t, "the export", func() bool { return r.Status("acme").Exported == 1 && r.Status("bad").ExportFailures == 1 })
	var buf bytes.Buffer
	if err := WritePrometheus(context.Background(), &buf, PromOptions{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`loomcycle_tenant_telemetry_exported_total{tenant="acme"} 1`,
		`loomcycle_tenant_telemetry_dropped_total{tenant="bad"} 1`,
		`loomcycle_tenant_telemetry_export_failures_total{tenant="bad"} 1`,
		`loomcycle_tenant_telemetry_pipelines 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	// A tenant's confined scrape shows it its own counters only.
	buf.Reset()
	if err := WritePrometheus(context.Background(), &buf, PromOptions{Tenant: "acme"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `tenant_telemetry_exported_total{tenant="acme"} 1`) || strings.Contains(out, `tenant="bad"`) {
		t.Errorf("acme's confined view:\n%s", out)
	}
}

// grpcCollector is an OTLP/gRPC collector that records span names and the
// metadata they came with.
type grpcCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	mu    sync.Mutex
	names []string
	md    []metadata.MD
}

func (g *grpcCollector) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.md = append(g.md, md)
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				g.names = append(g.names, s.GetName())
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func TestTenantRouter_GRPCDestinationReceivesSpansWithTheTenantsHeaders(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=operator-secret")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &grpcCollector{}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	var dialed []string
	var dmu sync.Mutex
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dmu.Lock()
		dialed = append(dialed, addr)
		dmu.Unlock()
		return plainDial(ctx, network, addr)
	}
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", TenantDestination{Endpoint: "http://" + lis.Addr().String(), Protocol: ProtocolGRPC,
		Headers: map[string]string{"x-scope-orgid": "acme"}, Dial: dial})
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	waitFor(t, "the gRPC export", func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.names) == 1 })
	g.mu.Lock()
	md := g.md[0]
	g.mu.Unlock()
	if got := md.Get("x-scope-orgid"); len(got) != 1 || got[0] != "acme" {
		t.Errorf("metadata = %v, want the tenant's header", md)
	}
	if got := md.Get("authorization"); len(got) != 0 {
		t.Errorf("the operator's header reached the tenant's gRPC collector: %v", got)
	}
	dmu.Lock()
	defer dmu.Unlock()
	if len(dialed) == 0 || dialed[0] != lis.Addr().String() {
		t.Errorf("dialed %v, want the destination through the given dialer", dialed)
	}
}
