package otel

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// pushedPoint is one data point a fake collector received.
type pushedPoint struct {
	metric string // instrument name
	prom   string // its Prometheus family name
	attrs  map[string]string
}

// pushedRequest is one metric push a fake collector received.
type pushedRequest struct {
	path     string
	headers  http.Header
	resource map[string]string
	points   []pushedPoint
	// exemplars is how many exemplars the push carried.
	exemplars int
}

// metricCollector is an OTLP/HTTP collector that records the metric pushes it
// is sent (and answers span exports, so one collector can take both).
type metricCollector struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []pushedRequest
	// spans counts span exports received.
	spans int
	// handler, when set, answers a metric push instead of accepting it.
	handler func(w http.ResponseWriter, r *http.Request) bool
}

func newMetricCollector(t *testing.T) *metricCollector {
	t.Helper()
	c := &metricCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *metricCollector) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/v1/traces" {
		c.mu.Lock()
		c.spans++
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		return
	}
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h != nil && h(w, r) {
		return
	}
	var req colmetricpb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, decodePush(r.URL.Path, r.Header.Clone(), &req))
	c.mu.Unlock()
	out, _ := proto.Marshal(&colmetricpb.ExportMetricsServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

func decodePush(path string, h http.Header, req *colmetricpb.ExportMetricsServiceRequest) pushedRequest {
	pr := pushedRequest{path: path, headers: h, resource: map[string]string{}}
	for _, rm := range req.GetResourceMetrics() {
		for k, v := range kvMap(rm.GetResource().GetAttributes()) {
			pr.resource[k] = v
		}
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				add := func(typ string, kvs map[string]string) {
					pr.points = append(pr.points, pushedPoint{metric: m.GetName(), prom: PromName(m.GetName(), m.GetUnit(), typ), attrs: kvs})
				}
				switch d := m.GetData().(type) {
				case *metricpb.Metric_Sum:
					typ := "gauge"
					if d.Sum.GetIsMonotonic() {
						typ = "counter"
					}
					for _, p := range d.Sum.GetDataPoints() {
						add(typ, kvMap(p.GetAttributes()))
						pr.exemplars += len(p.GetExemplars())
					}
				case *metricpb.Metric_Gauge:
					for _, p := range d.Gauge.GetDataPoints() {
						add("gauge", kvMap(p.GetAttributes()))
						pr.exemplars += len(p.GetExemplars())
					}
				case *metricpb.Metric_Histogram:
					for _, p := range d.Histogram.GetDataPoints() {
						add("histogram", kvMap(p.GetAttributes()))
						pr.exemplars += len(p.GetExemplars())
					}
				default:
					pr.points = append(pr.points, pushedPoint{metric: m.GetName()})
				}
			}
		}
	}
	return pr
}

func (c *metricCollector) got() []pushedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pushedRequest(nil), c.reqs...)
}

func (c *metricCollector) spanExports() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spans
}

func metricDest(c *metricCollector, headers map[string]string, signals ...string) TenantDestination {
	return TenantDestination{Endpoint: c.srv.URL, Protocol: ProtocolHTTPProtobuf, Headers: headers, Dial: plainDial, Signals: signals}
}

// metricRouter is a router whose interval never fires on its own: a test
// pushes with pushNow.
func metricRouter(t *testing.T, cfg TenantRouterConfig) *TenantRouter {
	t.Helper()
	if cfg.MetricInterval == 0 {
		cfg.MetricInterval = time.Hour
	}
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "v-test"
	}
	r := fastRouter(cfg)
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	return r
}

// pushNow runs one interval at now and waits for its pushes.
func pushNow(r *TenantRouter, now time.Time) {
	r.pushMetrics(now)
	r.metrics.pushes.Wait()
}

// recordSeries records series of acme, of other, of no tenant at all (a
// provider retry, a tool call) and, with export on, the GenAI histograms.
func recordSeries(t *testing.T) {
	t.Helper()
	restore, err := SetMetricsForTest(false, 0, sdkmetric.NewManualReader())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	ctx := context.Background()
	for _, tenant := range []string{"acme", "other"} {
		RecordUsage(ctx, UsageRecord{Tenant: tenant, Provider: "anthropic", Model: "m1", Key: "tenant", Input: 10, Output: 2})
		m := startRunMetric(ctx, tenant, TriggerHTTP, "a")
		m.setStatus("completed")
		m.end(ctx)
	}
	RecordProviderRetry(ctx, "anthropic", "header")
	RecordToolCallMetric(ctx, "Read", ToolOutcomeOK, time.Millisecond)
}

func TestTenantRouter_MetricPushCarriesOnlyTheTenantsOwnSeries(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	acme, other := newMetricCollector(t), newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", metricDest(acme, nil, SignalTraces, SignalMetrics))
	r.SetDestination("other", metricDest(other, nil, SignalMetrics))
	// The tenant's confined scrape, taken before the push counts itself.
	var buf bytes.Buffer
	if err := WritePrometheus(context.Background(), &buf, tenantMetricView("acme")); err != nil {
		t.Fatal(err)
	}
	pushNow(r, time.Now())

	for tenant, c := range map[string]*metricCollector{"acme": acme, "other": other} {
		reqs := c.got()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d pushes, want 1", tenant, len(reqs))
		}
		if reqs[0].path != "/v1/metrics" {
			t.Errorf("%s: pushed to %s", tenant, reqs[0].path)
		}
		if len(reqs[0].points) == 0 {
			t.Fatalf("%s: an empty push", tenant)
		}
		for _, p := range reqs[0].points {
			if got, ok := p.attrs[LabelTenant]; !ok || got != tenant {
				t.Errorf("%s received %s %v: not its own series", tenant, p.metric, p.attrs)
			}
			if strings.HasPrefix(p.metric, "gen_ai.") || p.metric == "loomcycle.provider.retries" || p.metric == "loomcycle.tool.calls" {
				t.Errorf("%s received the operator-wide series %s", tenant, p.metric)
			}
		}
		if reqs[0].exemplars != 0 {
			t.Errorf("%s: %d exemplars pushed", tenant, reqs[0].exemplars)
		}
	}

	// The push is exactly the tenant's confined scrape: the same families.
	var scraped []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			scraped = append(scraped, strings.Fields(name)[0])
		}
	}
	seen := map[string]bool{}
	var pushed []string
	for _, p := range acme.got()[0].points {
		if !seen[p.prom] {
			seen[p.prom] = true
			pushed = append(pushed, p.prom)
		}
	}
	sort.Strings(scraped)
	sort.Strings(pushed)
	if strings.Join(scraped, ",") != strings.Join(pushed, ",") {
		t.Errorf("pushed families %v, the tenant's scrape shows %v", pushed, scraped)
	}
}

func TestTenantRouter_MetricPushResourceIsMinimal(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,host.name=operator-box")
	t.Setenv("OTEL_SERVICE_NAME", "operator-service")
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	pushNow(r, time.Now())
	reqs := c.got()
	if len(reqs) != 1 {
		t.Fatalf("%d pushes, want 1", len(reqs))
	}
	want := map[string]string{"service.name": "loomcycle", "service.version": "v-test", AttrTenant: "acme"}
	if got := reqs[0].resource; len(got) != len(want) {
		t.Errorf("resource = %v, want %v", got, want)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("resource %s = %q, want %q (resource %v)", k, got[k], v, got)
			}
		}
	}
}

func TestSplitTenantMetrics_LeavesOutUserUntenantedAndOtherScopeSeries(t *testing.T) {
	at := func(kvs ...attribute.KeyValue) attribute.Set { return attribute.NewSet(kvs...) }
	ex := []metricdata.Exemplar[int64]{{Value: 1, TraceID: []byte("0123456789abcdef")}}
	rm := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{
		{Scope: instrumentation.Scope{Name: MeterName}, Metrics: []metricdata.Metrics{{
			Name: "loomcycle.runs",
			Data: metricdata.Sum[int64]{IsMonotonic: true, Temporality: metricdata.CumulativeTemporality, DataPoints: []metricdata.DataPoint[int64]{
				{Attributes: at(attribute.String(LabelTenant, "acme")), Value: 1, Exemplars: ex},
				{Attributes: at(attribute.String(LabelTenant, "acme"), attribute.String("user_hash", "u1")), Value: 2},
				{Attributes: at(attribute.String(LabelTenant, "other")), Value: 3},
				{Attributes: at(attribute.String(LabelTenant, "")), Value: 4},
				{Attributes: at(attribute.Bool("otel.metric.overflow", true)), Value: 5},
			}},
		}}},
		{Scope: instrumentation.Scope{Name: genAIMeterName}, Metrics: []metricdata.Metrics{{
			Name: "gen_ai.client.token.usage",
			Data: metricdata.Histogram[int64]{DataPoints: []metricdata.HistogramDataPoint[int64]{
				{Attributes: at(attribute.String(LabelTenant, "acme")), Count: 1},
			}},
		}}},
	}}
	parts := splitTenantMetrics(rm, map[string]bool{"acme": true}, "v")
	if len(parts) != 1 || parts["acme"] == nil {
		t.Fatalf("parts = %v, want acme's only", parts)
	}
	part := parts["acme"]
	if len(part.ScopeMetrics) != 1 || part.ScopeMetrics[0].Scope.Name != MeterName || len(part.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("acme's part = %+v", part)
	}
	sum := part.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 || len(sum.DataPoints[0].Exemplars) != 0 {
		t.Errorf("acme's points = %+v, want its one series without a user and without exemplars", sum.DataPoints)
	}
	if !sum.IsMonotonic || sum.Temporality != metricdata.CumulativeTemporality {
		t.Errorf("the sum lost its kind: %+v", sum)
	}
	// The collected snapshot is shared with every tenant's push: untouched.
	if src := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]); len(src.DataPoints[0].Exemplars) != 1 {
		t.Error("the split modified the collected snapshot")
	}
}

func TestTenantRouter_DestinationWithoutMetricsIsPushedNoMetrics(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{MetricInterval: 20 * time.Millisecond})
	r.SetDestination("acme", metricDest(c, nil)) // no signals: traces only
	r.SetDestination("other", metricDest(c, nil, SignalTraces))
	pushNow(r, time.Now())
	time.Sleep(100 * time.Millisecond) // several intervals of the router's own
	if got := c.got(); len(got) != 0 {
		t.Errorf("a traces-only destination received %d metric pushes", len(got))
	}
}

func TestTenantRouter_MetricsOnlyDestinationRoutesNoSpans(t *testing.T) {
	clearOTELEnv(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	if err := r.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, p := c.spanExports(), r.Pipelines(); n != 0 || p != 0 {
		t.Errorf("a metrics-only destination: %d span exports, %d span pipelines", n, p)
	}
}

func TestTenantRouter_RemovedDestinationIsPushedNoMoreMetrics(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{MetricInterval: 20 * time.Millisecond})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	waitFor(t, "a push", func() bool { return len(c.got()) >= 1 })
	r.RemoveDestination("acme")
	r.metrics.tick.Lock() // an interval that read the destination has ended
	r.metrics.tick.Unlock()
	r.metrics.pushes.Wait()
	n := len(c.got())
	time.Sleep(150 * time.Millisecond)
	if got := len(c.got()); got != n {
		t.Errorf("%d pushes after the destination was removed", got-n)
	}
	r.metrics.mu.Lock()
	open := len(r.metrics.pipes)
	r.metrics.mu.Unlock()
	if open != 0 {
		t.Errorf("%d metric pipes open after removal", open)
	}
}

func TestTenantRouter_RemovingTheDestinationEndsAPushInFlight(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	entered := make(chan struct{}, 1)
	c.handler = func(_ http.ResponseWriter, r *http.Request) bool {
		entered <- struct{}{}
		<-r.Context().Done()
		return true
	}
	r := metricRouter(t, TenantRouterConfig{ExportTimeout: 30 * time.Second})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	r.pushMetrics(time.Now())
	<-entered
	start := time.Now()
	r.RemoveDestination("acme")
	r.metrics.pushes.Wait()
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("the push in flight ran on %v after the destination was removed", el)
	}
}

func TestTenantRouter_BlackHoleMetricCollectorDelaysNoOtherTenant(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	release := make(chan struct{})
	hole, ok := newMetricCollector(t), newMetricCollector(t)
	hole.handler = func(http.ResponseWriter, *http.Request) bool { <-release; return true }
	r := metricRouter(t, TenantRouterConfig{ExportTimeout: 10 * time.Second})
	r.SetDestination("acme", metricDest(hole, nil, SignalMetrics))
	r.SetDestination("other", metricDest(ok, nil, SignalMetrics))
	// Registered after the router's cleanup, so it runs first: the hole
	// answers and the shutdown does not wait out the timeout.
	t.Cleanup(func() { close(release) })
	start := time.Now()
	r.pushMetrics(time.Now())
	if el := time.Since(start); el > time.Second {
		t.Errorf("the interval took %v with a tenant's collector hung", el)
	}
	waitFor(t, "the other tenant's push", func() bool { return len(ok.got()) == 1 })
	// The next interval skips the hung tenant rather than queue behind it.
	r.pushMetrics(time.Now())
	r.metrics.mu.Lock()
	busy := r.metrics.pipes["acme"].busy.Load()
	r.metrics.mu.Unlock()
	if !busy {
		t.Error("the hung push is not in flight")
	}
}

func TestTenantRouter_MetricPushStopsWhileIdleAndResumesOnChange(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{IdleTimeout: time.Hour})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	t0 := time.Now()
	pushNow(r, t0)
	pushNow(r, t0.Add(30*time.Minute)) // unchanged, not idle yet: pushed again
	if got := len(c.got()); got != 2 {
		t.Fatalf("%d pushes before going idle, want 2", got)
	}
	pushNow(r, t0.Add(61*time.Minute)) // unchanged for over an hour
	if got := len(c.got()); got != 2 {
		t.Errorf("an idle tenant was pushed: %d pushes", got)
	}
	RecordUsage(context.Background(), UsageRecord{Tenant: "acme", Provider: "anthropic", Model: "m1", Key: "tenant", Input: 1})
	pushNow(r, t0.Add(62*time.Minute))
	if got := len(c.got()); got != 3 {
		t.Errorf("a changed series was not pushed: %d pushes, want 3", got)
	}
}

func TestTenantRouter_MetricPipesAreBoundedLeastRecentlyChangedFirst(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{MaxPipelines: 1})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	r.SetDestination("other", metricDest(c, nil, SignalMetrics))
	pushNow(r, time.Now())
	r.metrics.mu.Lock()
	open := len(r.metrics.pipes)
	r.metrics.mu.Unlock()
	if open != 1 {
		t.Errorf("%d metric pipes open, want at most 1", open)
	}
}

func TestTenantRouter_MetricPushFailureIsReportedAndClearedByAMetricPushOnly(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	var refuse atomic.Bool
	refuse.Store(true)
	c.handler = func(w http.ResponseWriter, _ *http.Request) bool {
		if refuse.Load() {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	}
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", metricDest(c, nil, SignalTraces, SignalMetrics))
	pushNow(r, time.Now())
	st := r.Status("acme")
	if st.LastError != "metrics: http_404" || st.ExportFailures != 1 {
		t.Fatalf("status = %+v, want the metric push failure", st)
	}
	// A span export that succeeds leaves the metric failure standing.
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	waitFor(t, "the span export", func() bool { return r.Status("acme").Exported == 1 })
	if st := r.Status("acme"); st.LastError != "metrics: http_404" {
		t.Errorf("a span export cleared the metric failure: %+v", st)
	}
	var buf bytes.Buffer
	if err := WritePrometheus(context.Background(), &buf, PromOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := `loomcycle_tenant_telemetry_metric_pushes_total{outcome="failed",tenant="acme"} 1`; !strings.Contains(buf.String(), want) {
		t.Errorf("/metrics lacks %q", want)
	}
	refuse.Store(false)
	pushNow(r, time.Now())
	if st := r.Status("acme"); st.LastError != "" {
		t.Errorf("a metric push that succeeded left %q", st.LastError)
	}
	buf.Reset()
	if err := WritePrometheus(context.Background(), &buf, tenantMetricView("acme")); err != nil {
		t.Fatal(err)
	}
	if want := `loomcycle_tenant_telemetry_metric_pushes_total{outcome="ok",tenant="acme"} 1`; !strings.Contains(buf.String(), want) {
		t.Errorf("acme's confined scrape lacks %q", want)
	}
}

func TestTenantRouter_MetricPushNeverCarriesTheOperatorsHeaders(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=operator-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "x-operator=metrics-secret")
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", metricDest(c, map[string]string{"x-scope-orgid": "acme"}, SignalMetrics))
	r.SetDestination("bare", metricDest(c, nil, SignalMetrics))
	RecordUsage(context.Background(), UsageRecord{Tenant: "bare", Provider: "p", Model: "m", Key: "tenant", Input: 1})
	pushNow(r, time.Now())
	reqs := c.got()
	if len(reqs) != 2 {
		t.Fatalf("%d pushes, want 2", len(reqs))
	}
	for _, req := range reqs {
		if req.headers.Get("Authorization") != "" || req.headers.Get("X-Operator") != "" {
			t.Errorf("the operator's header reached a tenant's collector: %v", req.headers)
		}
		if req.resource[AttrTenant] == "acme" && req.headers.Get("X-Scope-Orgid") != "acme" {
			t.Errorf("acme's push lacks its own header: %v", req.headers)
		}
	}
}

func TestTenantRouter_MetricsArePushedEveryInterval(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{MetricInterval: 20 * time.Millisecond})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	waitFor(t, "two pushes", func() bool { return len(c.got()) >= 2 })
}

func TestTenantRouter_ShutdownMakesTheLastMetricPush(t *testing.T) {
	clearOTELEnv(t)
	recordSeries(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", metricDest(c, nil, SignalMetrics))
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(c.got()); got != 1 {
		t.Errorf("%d pushes at shutdown, want the last one", got)
	}
	pushNow(r, time.Now())
	if got := len(c.got()); got != 1 {
		t.Errorf("a push after shutdown: %d", got)
	}
}

// grpcMetricCollector is an OTLP/gRPC metric collector that records the
// metadata each push came with.
type grpcMetricCollector struct {
	colmetricpb.UnimplementedMetricsServiceServer
	mu sync.Mutex
	md []metadata.MD
}

func (g *grpcMetricCollector) Export(ctx context.Context, _ *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.md = append(g.md, md)
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func TestTenantRouter_GRPCMetricPushGoesThroughTheGivenDialerWithTheTenantsHeaders(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=operator-secret")
	recordSeries(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &grpcMetricCollector{}
	srv := grpc.NewServer()
	colmetricpb.RegisterMetricsServiceServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	var (
		dmu    sync.Mutex
		dialed []string
	)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dmu.Lock()
		dialed = append(dialed, addr)
		dmu.Unlock()
		return plainDial(ctx, network, addr)
	}
	r := metricRouter(t, TenantRouterConfig{})
	r.SetDestination("acme", TenantDestination{Endpoint: "http://" + lis.Addr().String(), Protocol: ProtocolGRPC,
		Headers: map[string]string{"x-scope-orgid": "acme"}, Dial: dial, Signals: []string{SignalMetrics}})
	pushNow(r, time.Now())
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.md) != 1 {
		t.Fatalf("%d gRPC pushes, want 1 (status %+v)", len(g.md), r.Status("acme"))
	}
	if got := g.md[0].Get("x-scope-orgid"); len(got) != 1 || got[0] != "acme" {
		t.Errorf("metadata = %v, want the tenant's header", g.md[0])
	}
	if got := g.md[0].Get("authorization"); len(got) != 0 {
		t.Errorf("the operator's header reached the tenant's gRPC collector: %v", got)
	}
	dmu.Lock()
	defer dmu.Unlock()
	if len(dialed) == 0 || dialed[0] != lis.Addr().String() {
		t.Errorf("dialed %v, want the destination through the given dialer", dialed)
	}
}

func TestTenantRouterTest_SendsATestMetricOnlyToAMetricsDestination(t *testing.T) {
	clearOTELEnv(t)
	c := newMetricCollector(t)
	r := metricRouter(t, TenantRouterConfig{})
	if err := r.Test(context.Background(), "acme", metricDest(c, nil, SignalMetrics), time.Second); err != nil {
		t.Fatalf("test of a metrics-only destination: %v", err)
	}
	reqs := c.got()
	if len(reqs) != 1 || len(reqs[0].points) != 1 || reqs[0].points[0].metric != SpanTelemetryTest || reqs[0].resource[AttrTenant] != "acme" {
		t.Fatalf("pushed %+v, want one test metric", reqs)
	}
	if n := c.spanExports(); n != 0 {
		t.Errorf("a metrics-only test sent %d span exports", n)
	}
	if err := r.Test(context.Background(), "acme", metricDest(c, nil, SignalTraces, SignalMetrics), time.Second); err != nil {
		t.Fatalf("test of both signals: %v", err)
	}
	if n, m := c.spanExports(), len(c.got()); n != 1 || m != 2 {
		t.Errorf("a test of both signals sent %d span exports and %d metric pushes in all, want 1 and 2", n, m)
	}
	c.handler = func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(http.StatusNotFound); return true }
	if err := r.Test(context.Background(), "acme", metricDest(c, nil, SignalMetrics), time.Second); err == nil || err.Error() != "metrics: http_404" {
		t.Errorf("a refused test metric: %v, want metrics: http_404", err)
	}
}
