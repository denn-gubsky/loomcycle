package otel

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"math"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Tenant metric push: a tenant whose destination names the metrics signal
// receives, every MetricInterval, its own series of the in-process
// instruments — exactly the series its confined /metrics scrape shows
// (tenantMetricView), under the same minimal resource as its spans.
//
// No instrument is registered for it. Each interval the router collects the
// manual reader /metrics renders once, splits the snapshot by the tenant label
// and hands each tenant with a destination its own part. A series with no
// tenant label (a database pool, a provider attempt, the overflow series) is
// operator-wide and goes to no tenant; nor does a series naming a user.
//
// Each push runs in its own goroutine, inside the export slots every tenant
// shares and bounded by the export timeout, so a tenant whose collector is
// slow or down delays nobody else's push and never the interval itself.
//
// A tenant's push stops once its series have not changed for IdleTimeout
// (the values it last pushed stand) and starts again when they change; at
// most MaxPipelines pushers are open, the least recently changed closed
// first. Collection and each push read memory only: no database is queried.

// DefaultTenantMetricInterval is how often a tenant's series are pushed: the
// floor the operator sets, not a tenant's choice.
const DefaultTenantMetricInterval = time.Minute

// The signals a destination may export.
const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
)

// MetricsURL is the URL an http/protobuf metric push to endpoint is posted
// to: the OTLP base endpoint with the metrics path. A header credential is
// checked against it too, as a URL its value is sent to.
func MetricsURL(endpoint string, protocol Protocol) string {
	if protocol == ProtocolGRPC {
		return endpoint
	}
	return strings.TrimRight(endpoint, "/") + "/v1/metrics"
}

// exports reports whether d exports signal. No signal named is traces only,
// as a stored policy's.
func (d TenantDestination) exports(signal string) bool {
	if len(d.Signals) == 0 {
		return signal == SignalTraces
	}
	for _, s := range d.Signals {
		if s == signal {
			return true
		}
	}
	return false
}

// tenantMetricView is what of the instruments a tenant may see: the options
// of its confined /metrics scrape (the HTTP server's promViewForPrincipal for
// a tenant_metrics token). The push and the scrape share shows, so a series
// left out of one is left out of the other.
func tenantMetricView(tenant string) PromOptions {
	return PromOptions{Confined: true, Tenant: tenant, OmitUserSeries: true}
}

// tenantMetrics is the router's metric side.
type tenantMetrics struct {
	// collect is the snapshot pushed: the /metrics reader's (collect), or a
	// test's.
	collect func(context.Context) (*metricdata.ResourceMetrics, error)

	// tick serialises the interval's passes with the final one at shutdown.
	tick sync.Mutex

	mu     sync.Mutex
	pipes  map[string]*tenantMetricPipe
	marks  map[string]metricMark
	closed bool
	// pushes tracks the pushes in flight, which Shutdown waits for.
	pushes sync.WaitGroup
}

// metricMark is a tenant's series as last seen: a digest of their values and
// when it last changed.
type metricMark struct {
	digest  uint64
	changed time.Time
}

// tenantMetricPipe is one tenant's metric exporter.
type tenantMetricPipe struct {
	fp      string
	exp     sdkmetric.Exporter
	release func()
	headers map[string]string
	stats   *tenantStats
	// busy: a push is in flight; the next interval skips the tenant rather
	// than queue a second behind it.
	busy atomic.Bool
	// ctx is cancelled when the pipe is closed, ending a push in flight.
	ctx    context.Context
	cancel context.CancelFunc
}

func newTenantMetrics() *tenantMetrics {
	return &tenantMetrics{collect: collect, pipes: map[string]*tenantMetricPipe{}, marks: map[string]metricMark{}}
}

// newTenantMetricExporter builds the OTLP metric exporter for d, from the
// same endpoint check, dialer, TLS and header copy as its span exporter
// (newTenantExporter), so no operator OTEL_EXPORTER_OTLP_* setting reaches a
// tenant's collector. One attempt per push, bounded by timeout.
func newTenantMetricExporter(ctx context.Context, d TenantDestination, timeout time.Duration) (sdkmetric.Exporter, func(), error) {
	u, headers, err := tenantEndpoint(d)
	if err != nil {
		return nil, nil, err
	}
	if d.Protocol == ProtocolGRPC {
		return newTenantGRPCMetricExporter(ctx, u, d, headers, timeout)
	}
	client, tr := tenantHTTPClient(d, timeout)
	exp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(MetricsURL(d.Endpoint, ProtocolHTTPProtobuf)),
		otlpmetrichttp.WithHTTPClient(client),
		otlpmetrichttp.WithHeaders(headers),
		otlpmetrichttp.WithTimeout(timeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, nil, err
	}
	return exp, tr.CloseIdleConnections, nil
}

func newTenantGRPCMetricExporter(ctx context.Context, u *url.URL, d TenantDestination, headers map[string]string, timeout time.Duration) (sdkmetric.Exporter, func(), error) {
	conn, err := tenantGRPCConn(u, d)
	if err != nil {
		return nil, nil, err
	}
	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(conn),
		otlpmetricgrpc.WithHeaders(headers),
		otlpmetricgrpc.WithTimeout(timeout),
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}),
	)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return exp, func() { _ = conn.Close() }, nil
}

// TestTenantMetricExport pushes one synthetic gauge (loomcycle.telemetry.test
// = 1) to d under the tenant's resource, built exactly as a tenant's metric
// push builds its exporter, and returns nil or a *TenantExportError reading
// "metrics: <category>". It answers within timeout.
func TestTenantMetricExport(ctx context.Context, tenant string, d TenantDestination, serviceVersion string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTenantExportTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, code := withStatusHolder(ctx)
	exp, release, err := newTenantMetricExporter(ctx, d, timeout)
	if err != nil {
		return &TenantExportError{msg: "metrics: " + CategoryFailed}
	}
	defer func() {
		_ = exp.Shutdown(context.Background())
		if release != nil {
			release()
		}
	}()
	rm := &metricdata.ResourceMetrics{
		Resource: tenantResource(tenant, serviceVersion),
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Scope: instrumentation.Scope{Name: MeterName},
			Metrics: []metricdata.Metrics{{
				Name: SpanTelemetryTest, Unit: "1", Description: "A tenant telemetry test: one value, sent on request.",
				Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{{
					Attributes: attribute.NewSet(attribute.String(LabelTenant, tenant)), Time: time.Now(), Value: 1,
				}}},
			}},
		}},
	}
	if err := exp.Export(ctx, rm); err != nil {
		return &TenantExportError{msg: "metrics: " + exportCategory(err, code.code.Load())}
	}
	return nil
}

// startMetrics runs the interval's push until Shutdown.
func (r *TenantRouter) startMetrics() {
	r.closing.Add(1)
	go func() {
		defer r.closing.Done()
		t := time.NewTicker(r.cfg.MetricInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case now := <-t.C:
				r.pushMetrics(now)
			}
		}
	}()
}

// metricTarget is a tenant with a destination that exports metrics.
type metricTarget struct {
	dest  TenantDestination
	fp    string
	stats *tenantStats
}

// pushMetrics is one interval: collect once, split by tenant, push each
// tenant's part in the background. It returns without waiting on a push.
func (r *TenantRouter) pushMetrics(now time.Time) {
	m := r.metrics
	m.tick.Lock()
	defer m.tick.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}
	defer func() {
		if v := recover(); v != nil {
			r.cfg.Log.Error("otel: tenant metric push panicked", "panic", v)
		}
	}()
	targets := r.metricTargets()
	var parts map[string]*metricdata.ResourceMetrics
	if len(targets) > 0 {
		rm, err := m.collect(context.Background())
		if errors.Is(err, sdkmetric.ErrReaderShutdown) {
			// The meter provider stops beside the tracer at shutdown: the
			// last push has nothing left to read.
			return
		}
		if err != nil {
			r.cfg.Log.Warn("otel: tenant metric push could not collect the instruments", "err", err)
			return
		}
		if rm != nil {
			wanted := make(map[string]bool, len(targets))
			for t := range targets {
				wanted[t] = true
			}
			parts = splitTenantMetrics(rm, wanted, r.cfg.ServiceVersion)
		}
	}
	// The plan is made under the router's lock: a destination removed or
	// changed after metricTargets read it is not pushed to.
	r.mu.RLock()
	defer r.mu.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for t, p := range m.pipes {
		if e := r.entries[t]; e == nil || e.fp != p.fp || !e.dest.exports(SignalMetrics) {
			r.closeMetricPipeLocked(t)
		}
	}
	for t, tg := range targets {
		if e := r.entries[t]; e == nil || e.fp != tg.fp {
			continue
		}
		part := parts[t]
		if part == nil {
			// No series yet: nothing to push, and nothing kept open for it.
			r.closeMetricPipeLocked(t)
			continue
		}
		d := metricDigest(part)
		mark, seen := m.marks[t]
		if !seen || mark.digest != d {
			mark = metricMark{digest: d, changed: now}
			m.marks[t] = mark
		}
		if now.Sub(mark.changed) >= r.cfg.IdleTimeout {
			r.closeMetricPipeLocked(t)
			continue
		}
		p := m.pipes[t]
		if p == nil {
			if p = r.openMetricPipeLocked(t, tg); p == nil {
				continue
			}
		}
		if !p.busy.CompareAndSwap(false, true) {
			continue
		}
		m.pushes.Add(1)
		go r.pushTenantMetrics(t, p, part)
	}
}

// metricTargets are the tenants whose destination exports metrics.
func (r *TenantRouter) metricTargets() map[string]metricTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]metricTarget{}
	for t, e := range r.entries {
		if e.dest.exports(SignalMetrics) {
			out[t] = metricTarget{dest: e.dest, fp: e.fp, stats: e.stats}
		}
	}
	return out
}

// openMetricPipeLocked builds tenant's metric exporter (no network I/O: it
// connects on its first push), closing the least recently changed pipe when
// MaxPipelines are open. Called with r.mu and m.mu held.
func (r *TenantRouter) openMetricPipeLocked(tenant string, tg metricTarget) *tenantMetricPipe {
	m := r.metrics
	if len(m.pipes) >= r.cfg.MaxPipelines {
		var (
			oldest string
			at     time.Time
		)
		for t := range m.pipes {
			if c := m.marks[t].changed; oldest == "" || c.Before(at) {
				oldest, at = t, c
			}
		}
		r.closeMetricPipeLocked(oldest)
	}
	exp, release, err := newTenantMetricExporter(context.Background(), tg.dest, r.cfg.ExportTimeout)
	if err != nil {
		tg.stats.metricFailed(time.Now(), "metrics: build exporter: "+scrubError(err, tg.dest.Headers))
		r.cfg.Log.Warn("otel: tenant metric exporter could not be built", "tenant", tenant, "err", scrubError(err, tg.dest.Headers))
		return nil
	}
	p := &tenantMetricPipe{fp: tg.fp, exp: exp, release: release, headers: tg.dest.Headers, stats: tg.stats}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	m.pipes[tenant] = p
	return p
}

// closeMetricPipeLocked ends tenant's push in flight, if any, and closes its
// exporter in the background. Called with m.mu held.
func (r *TenantRouter) closeMetricPipeLocked(tenant string) {
	m := r.metrics
	p := m.pipes[tenant]
	if p == nil {
		return
	}
	delete(m.pipes, tenant)
	p.cancel()
	r.closing.Add(1)
	go func() {
		defer r.closing.Done()
		defer func() {
			if v := recover(); v != nil {
				r.cfg.Log.Error("otel: tenant metric exporter close panicked", "tenant", tenant, "panic", v)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), r.cfg.ExportTimeout)
		defer cancel()
		_ = p.exp.Shutdown(ctx)
		if p.release != nil {
			p.release()
		}
	}()
}

// forgetMetrics stops tenant's push and forgets its series' mark, so a new
// destination is pushed the current values at once. Called with r.mu held.
func (r *TenantRouter) forgetMetrics(tenant string) {
	m := r.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	r.closeMetricPipeLocked(tenant)
	delete(m.marks, tenant)
}

// pushTenantMetrics sends part through p inside the shared export slots and
// records the outcome. A push of a pipe closed meanwhile is neither a success
// nor a failure: the tenant changed or removed its destination.
func (r *TenantRouter) pushTenantMetrics(tenant string, p *tenantMetricPipe, part *metricdata.ResourceMetrics) {
	defer r.metrics.pushes.Done()
	defer p.busy.Store(false)
	defer func() {
		if v := recover(); v != nil {
			r.cfg.Log.Error("otel: tenant metric push panicked", "tenant", tenant, "panic", v)
		}
	}()
	release, ok := r.slots.acquire(p.ctx, tenant)
	if !ok {
		if p.ctx.Err() == nil {
			r.metricPushFailed(tenant, p, CategoryBusy)
		}
		return
	}
	defer release()
	ctx, cancelExport := context.WithTimeout(p.ctx, r.cfg.ExportTimeout)
	defer cancelExport()
	ctx, code := withStatusHolder(ctx)
	err := p.exp.Export(ctx, part)
	switch {
	case p.ctx.Err() != nil:
	case err == nil:
		p.stats.metricSucceeded(time.Now())
		tenantMetricPushed(tenant, metricPushOK)
	default:
		r.metricPushFailed(tenant, p, exportCategory(err, code.code.Load()))
		r.cfg.Log.Debug("otel: tenant metric push failed", "tenant", tenant, "err", scrubError(err, p.headers))
	}
}

func (r *TenantRouter) metricPushFailed(tenant string, p *tenantMetricPipe, category string) {
	p.stats.failures.Add(1)
	p.stats.metricFailed(time.Now(), "metrics: "+category)
	tenantMetricPushed(tenant, metricPushFailed)
}

// tenantMetricPushesName is the instrument counting the pushes.
const tenantMetricPushesName = "loomcycle.tenant_telemetry.metric_pushes"

// The outcome labels of loomcycle.tenant_telemetry.metric_pushes.
const (
	metricPushOK     = "ok"
	metricPushFailed = "failed"
)

// shutdownMetrics makes the last push of every open pipe, waits for it while
// ctx allows, and closes every pipe. The router's lock is not held.
func (r *TenantRouter) shutdownMetrics(ctx context.Context) {
	m := r.metrics
	r.pushMetrics(time.Now())
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.pushes.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	m.mu.Lock()
	for t := range m.pipes {
		r.closeMetricPipeLocked(t)
	}
	m.mu.Unlock()
}

// metricSucceeded records a successful push. It clears a metric failure only:
// a span export failing meanwhile is still the tenant's to see.
func (st *tenantStats) metricSucceeded(at time.Time) {
	st.mu.Lock()
	st.lastSuccess, st.metricErr, st.metricErrAt = at, "", time.Time{}
	st.mu.Unlock()
}

func (st *tenantStats) metricFailed(at time.Time, msg string) {
	st.mu.Lock()
	st.metricErr, st.metricErrAt = msg, at
	st.mu.Unlock()
}

// splitTenantMetrics is each wanted tenant's part of rm: the series of the
// scope /metrics renders that its confined view shows, without exemplars (a
// trace id the tenant's own traces may not hold), under the tenant's
// resource. A tenant with no series has no part.
func splitTenantMetrics(rm *metricdata.ResourceMetrics, wanted map[string]bool, version string) map[string]*metricdata.ResourceMetrics {
	owner := func(attrs attribute.Set) string {
		v, ok := attrs.Value(LabelTenant)
		if !ok {
			return ""
		}
		t := v.Emit()
		if !wanted[t] || !tenantMetricView(t).shows(attrs) {
			return ""
		}
		return t
	}
	out := map[string]*metricdata.ResourceMetrics{}
	for _, sm := range rm.ScopeMetrics {
		if !rendersScope(sm.Scope.Name) {
			continue
		}
		for _, mt := range sm.Metrics {
			for t, data := range splitAggregation(mt.Data, owner) {
				part := out[t]
				if part == nil {
					part = &metricdata.ResourceMetrics{Resource: tenantResource(t, version)}
					out[t] = part
				}
				if n := len(part.ScopeMetrics); n == 0 || part.ScopeMetrics[n-1].Scope != sm.Scope {
					part.ScopeMetrics = append(part.ScopeMetrics, metricdata.ScopeMetrics{Scope: sm.Scope})
				}
				last := &part.ScopeMetrics[len(part.ScopeMetrics)-1]
				last.Metrics = append(last.Metrics, metricdata.Metrics{Name: mt.Name, Description: mt.Description, Unit: mt.Unit, Data: data})
			}
		}
	}
	return out
}

// splitAggregation splits data's points by owner, dropping the points owner
// gives to no one (""). Only the kinds /metrics renders are pushed.
func splitAggregation(data metricdata.Aggregation, owner func(attribute.Set) string) map[string]metricdata.Aggregation {
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		return splitPoints(d.DataPoints, owner, func(p []metricdata.DataPoint[int64]) metricdata.Aggregation {
			return metricdata.Sum[int64]{DataPoints: p, Temporality: d.Temporality, IsMonotonic: d.IsMonotonic}
		})
	case metricdata.Sum[float64]:
		return splitPoints(d.DataPoints, owner, func(p []metricdata.DataPoint[float64]) metricdata.Aggregation {
			return metricdata.Sum[float64]{DataPoints: p, Temporality: d.Temporality, IsMonotonic: d.IsMonotonic}
		})
	case metricdata.Gauge[int64]:
		return splitPoints(d.DataPoints, owner, func(p []metricdata.DataPoint[int64]) metricdata.Aggregation {
			return metricdata.Gauge[int64]{DataPoints: p}
		})
	case metricdata.Gauge[float64]:
		return splitPoints(d.DataPoints, owner, func(p []metricdata.DataPoint[float64]) metricdata.Aggregation {
			return metricdata.Gauge[float64]{DataPoints: p}
		})
	case metricdata.Histogram[int64]:
		return splitPoints(d.DataPoints, owner, func(p []metricdata.HistogramDataPoint[int64]) metricdata.Aggregation {
			return metricdata.Histogram[int64]{DataPoints: p, Temporality: d.Temporality}
		})
	case metricdata.Histogram[float64]:
		return splitPoints(d.DataPoints, owner, func(p []metricdata.HistogramDataPoint[float64]) metricdata.Aggregation {
			return metricdata.Histogram[float64]{DataPoints: p, Temporality: d.Temporality}
		})
	}
	return nil
}

// metricPoint is a data point kind the split handles.
type metricPoint interface {
	metricdata.DataPoint[int64] | metricdata.DataPoint[float64] |
		metricdata.HistogramDataPoint[int64] | metricdata.HistogramDataPoint[float64]
}

func splitPoints[P metricPoint](points []P, owner func(attribute.Set) string, wrap func([]P) metricdata.Aggregation) map[string]metricdata.Aggregation {
	var by map[string][]P
	for _, p := range points {
		t := owner(pointAttrs(p))
		if t == "" {
			continue
		}
		if by == nil {
			by = map[string][]P{}
		}
		by[t] = append(by[t], withoutExemplars(p))
	}
	out := make(map[string]metricdata.Aggregation, len(by))
	for t, ps := range by {
		out[t] = wrap(ps)
	}
	return out
}

func pointAttrs[P metricPoint](p P) attribute.Set {
	switch v := any(p).(type) {
	case metricdata.DataPoint[int64]:
		return v.Attributes
	case metricdata.DataPoint[float64]:
		return v.Attributes
	case metricdata.HistogramDataPoint[int64]:
		return v.Attributes
	case metricdata.HistogramDataPoint[float64]:
		return v.Attributes
	}
	return attribute.Set{}
}

func withoutExemplars[P metricPoint](p P) P {
	switch v := any(&p).(type) {
	case *metricdata.DataPoint[int64]:
		v.Exemplars = nil
	case *metricdata.DataPoint[float64]:
		v.Exemplars = nil
	case *metricdata.HistogramDataPoint[int64]:
		v.Exemplars = nil
	case *metricdata.HistogramDataPoint[float64]:
		v.Exemplars = nil
	}
	return p
}

// metricDigest identifies the values of part's series, whatever order the
// reader lists them in: a sum of each point's hash. Timestamps are left out,
// so a series that did not move hashes the same at the next collection; so is
// the count of the pushes themselves, which every push moves and which would
// keep an idle tenant pushed forever.
func metricDigest(part *metricdata.ResourceMetrics) uint64 {
	var sum uint64
	enc := attribute.DefaultEncoder()
	for _, sm := range part.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name == tenantMetricPushesName {
				continue
			}
			point := func(attrs attribute.Set, values ...float64) {
				h := fnv.New64a()
				_, _ = h.Write([]byte(mt.Name))
				_, _ = h.Write([]byte{0})
				_, _ = h.Write([]byte(attrs.Encoded(enc)))
				var b [8]byte
				for _, v := range values {
					binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
					_, _ = h.Write(b[:])
				}
				sum += h.Sum64()
			}
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					point(p.Attributes, float64(p.Value))
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					point(p.Attributes, p.Value)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					point(p.Attributes, float64(p.Value))
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					point(p.Attributes, p.Value)
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					point(p.Attributes, float64(p.Count), float64(p.Sum))
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					point(p.Attributes, float64(p.Count), p.Sum)
				}
			}
		}
	}
	return sum
}
