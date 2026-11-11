package otel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Tenant export: a tenant's own spans to an OTLP destination of its own,
// beside the operator's export (which this never changes).
//
// The router is a span processor on the process's tracer provider. At each
// span's end it reads the span's tenant attribute: a span with none is never
// routed; a span of a tenant with no destination is skipped with one map
// read. A tenant with a destination has its own pipeline — a bounded queue
// and a batch exporter of its own — so a tenant whose collector is slow or
// down fills and drops its own queue and nobody else's, and a span's end never
// waits on any export.
//
// The router never decides who may export or where to: the caller sets each
// tenant's destination (SetDestination) after its own checks, with the dialer
// that guards it.

// TenantDestination is where one tenant's spans are exported.
type TenantDestination struct {
	// Endpoint is the OTLP base URL (https://collector.example:4318). For
	// http/protobuf, spans are posted to Endpoint + "/v1/traces".
	Endpoint string
	Protocol Protocol
	// Headers are sent with every export. Never logged, and scrubbed from
	// every error the status reports.
	Headers map[string]string
	// Dial connects to the collector; the caller's guard (which addresses may
	// be reached) lives here. Required.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSConfig is the client TLS configuration for an https endpoint; nil
	// for the system roots. Tests set it to trust their own collector.
	TLSConfig *tls.Config
}

// fingerprint identifies the destination's settings: a pipeline built for an
// older fingerprint is replaced. Header values enter only hashed.
func (d TenantDestination) fingerprint() string {
	h := sha256.New()
	write := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	write(d.Endpoint)
	write(string(d.Protocol))
	keys := make([]string, 0, len(d.Headers))
	for k := range d.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k)
		write(d.Headers[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TenantRouterConfig bounds the router. Zero fields take the defaults below.
type TenantRouterConfig struct {
	// QueueSize is each tenant pipeline's queue (spans waiting for export).
	QueueSize int
	// MaxPipelines is how many tenant pipelines may exist at once; past it the
	// least recently used is flushed and closed to make room.
	MaxPipelines int
	// IdleTimeout closes a pipeline no span has reached for this long.
	IdleTimeout time.Duration
	// MaxConcurrentExports caps export calls in flight across all tenants.
	MaxConcurrentExports int
	// ExportTimeout bounds one export call.
	ExportTimeout time.Duration
	// ScheduleDelay is how long a pipeline waits to fill a batch.
	ScheduleDelay time.Duration
	// ServiceVersion is service.version on every tenant resource.
	ServiceVersion string
	// Log receives the router's own warnings (never a header value).
	Log *slog.Logger
}

// The defaults of TenantRouterConfig.
const (
	DefaultTenantQueueSize            = 512
	DefaultTenantMaxPipelines         = 256
	DefaultTenantIdleTimeout          = 10 * time.Minute
	DefaultTenantMaxConcurrentExports = 16
	DefaultTenantExportTimeout        = 10 * time.Second
	defaultTenantScheduleDelay        = 5 * time.Second
	tenantReapInterval                = time.Minute
)

func (c TenantRouterConfig) withDefaults() TenantRouterConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultTenantQueueSize
	}
	if c.MaxPipelines <= 0 {
		c.MaxPipelines = DefaultTenantMaxPipelines
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultTenantIdleTimeout
	}
	if c.MaxConcurrentExports <= 0 {
		c.MaxConcurrentExports = DefaultTenantMaxConcurrentExports
	}
	if c.ExportTimeout <= 0 {
		c.ExportTimeout = DefaultTenantExportTimeout
	}
	if c.ScheduleDelay <= 0 {
		c.ScheduleDelay = defaultTenantScheduleDelay
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return c
}

// TenantExportStatus is one tenant's export on this replica since the process
// started.
type TenantExportStatus struct {
	// Configured: a destination is set for the tenant on this replica.
	Configured bool
	// Exported: spans the tenant's collector accepted.
	Exported uint64
	// Dropped: spans that will never reach it (queue full, carried by a failed
	// export, or rejected by the collector).
	Dropped uint64
	// ExportFailures: export calls that failed.
	ExportFailures uint64
	LastSuccessAt  time.Time
	// LastError is the last failure, scrubbed of header values, URL
	// credentials and query values, and cut short; "" when none since the
	// last success.
	LastError   string
	LastErrorAt time.Time
}

// tenantStats outlives the tenant's pipelines: a pipeline closed when idle or
// replaced keeps counting where the last one stopped.
type tenantStats struct {
	exported, dropped, failures atomic.Uint64

	mu          sync.Mutex
	lastSuccess time.Time
	lastErr     string
	lastErrAt   time.Time
}

func (st *tenantStats) succeeded(at time.Time) {
	st.mu.Lock()
	st.lastSuccess, st.lastErr, st.lastErrAt = at, "", time.Time{}
	st.mu.Unlock()
}

func (st *tenantStats) failed(at time.Time, msg string) {
	st.mu.Lock()
	st.lastErr, st.lastErrAt = msg, at
	st.mu.Unlock()
}

type tenantEntry struct {
	dest  TenantDestination
	fp    string
	stats *tenantStats
}

// TenantRouter routes ended spans to their tenant's pipeline. Create it with
// NewTenantRouter and hand it to Init (Config.TenantRouter); it is then a
// processor of the global tracer provider until that shuts down.
type TenantRouter struct {
	cfg TenantRouterConfig
	// slots caps exports in flight across every tenant.
	slots chan struct{}

	mu      sync.RWMutex
	entries map[string]*tenantEntry
	pipes   map[string]*tenantPipeline
	closed  bool
	// flushing names the tenants with a replaced pipeline still flushing.
	// One flush per tenant at a time: a flush waits on the tenant's
	// collector inside a shared export slot, and a tenant flapping its
	// destination between collectors that never answer would otherwise hold
	// one slot per flap.
	flushing map[string]bool

	stop     chan struct{}
	stopOnce sync.Once
	// closing tracks pipelines being flushed and closed in the background.
	closing sync.WaitGroup
}

// NewTenantRouter returns a router with no destinations and starts its idle
// reaper, which Shutdown stops.
func NewTenantRouter(cfg TenantRouterConfig) *TenantRouter {
	cfg = cfg.withDefaults()
	r := &TenantRouter{
		cfg:      cfg,
		slots:    make(chan struct{}, cfg.MaxConcurrentExports),
		entries:  map[string]*tenantEntry{},
		pipes:    map[string]*tenantPipeline{},
		flushing: map[string]bool{},
		stop:     make(chan struct{}),
	}
	r.closing.Add(1)
	go r.reapLoop()
	return r
}

// SetDestination sets (or replaces) tenant's destination. A pipeline built
// for different settings is closed; the next span builds one for these.
func (r *TenantRouter) SetDestination(tenant string, d TenantDestination) {
	if tenant == "" || d.Dial == nil {
		return
	}
	fp := d.fingerprint()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	e := r.entries[tenant]
	if e == nil {
		e = &tenantEntry{stats: &tenantStats{}}
		r.entries[tenant] = e
	}
	e.dest, e.fp = d, fp
	if p := r.pipes[tenant]; p != nil && p.fp != fp {
		r.dropPipelineLocked(tenant, p)
	}
}

// RemoveDestination stops exporting tenant's spans: its pipeline is flushed
// and closed and its status forgotten.
func (r *TenantRouter) RemoveDestination(tenant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, tenant)
	if p := r.pipes[tenant]; p != nil {
		r.dropPipelineLocked(tenant, p)
	}
}

// Tenants returns the tenants with a destination, sorted.
func (r *TenantRouter) Tenants() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for t := range r.entries {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Status reports tenant's export on this replica.
func (r *TenantRouter) Status(tenant string) TenantExportStatus {
	r.mu.RLock()
	e := r.entries[tenant]
	r.mu.RUnlock()
	if e == nil {
		return TenantExportStatus{}
	}
	st := e.stats
	st.mu.Lock()
	defer st.mu.Unlock()
	return TenantExportStatus{
		Configured:     true,
		Exported:       st.exported.Load(),
		Dropped:        st.dropped.Load(),
		ExportFailures: st.failures.Load(),
		LastSuccessAt:  st.lastSuccess,
		LastError:      st.lastErr,
		LastErrorAt:    st.lastErrAt,
	}
}

// Pipelines reports how many tenant pipelines are open.
func (r *TenantRouter) Pipelines() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pipes)
}

// OnStart implements sdktrace.SpanProcessor; routing is decided at the end.
func (r *TenantRouter) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd routes a sampled span of a tenant with a destination to that tenant's
// pipeline. It never blocks on an export: a full queue drops the span.
func (r *TenantRouter) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	tenant, noTrace := spanRoute(s)
	if tenant == "" || noTrace {
		return
	}
	r.mu.RLock()
	e := r.entries[tenant]
	p := r.pipes[tenant]
	var fp string
	if e != nil {
		fp = e.fp
	}
	r.mu.RUnlock()
	if e == nil {
		return
	}
	// A span that lost attributes to the SDK's count limit and carries no
	// marker may be a no-trace run's whose marker was the one lost: fail
	// closed, and count it.
	if s.DroppedAttributes() > 0 {
		e.stats.dropped.Add(1)
		tenantSpansDropped(tenant, 1)
		return
	}
	if p == nil || p.fp != fp {
		if p = r.pipeline(tenant); p == nil {
			return
		}
	}
	p.enqueue(s)
}

// pipeline returns tenant's open pipeline, building it when there is none
// (or the one there is was built for older settings). Building does no
// network I/O: the exporter connects on its first export.
func (r *TenantRouter) pipeline(tenant string) *tenantPipeline {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[tenant]
	if r.closed || e == nil {
		return nil
	}
	if p := r.pipes[tenant]; p != nil {
		if p.fp == e.fp {
			return p
		}
		r.dropPipelineLocked(tenant, p)
	}
	if len(r.pipes) >= r.cfg.MaxPipelines {
		r.evictLRULocked()
	}
	p, err := newTenantPipeline(tenant, e, r.cfg, r.slots)
	if err != nil {
		// A destination that cannot even build an exporter (a malformed
		// endpoint the caller let through) is a failure of its own, not a
		// reason to retry the build on every span.
		e.stats.failed(time.Now(), "build exporter: "+scrubError(err, e.dest.Headers))
		r.cfg.Log.Warn("otel: tenant export pipeline could not be built", "tenant", tenant, "err", scrubError(err, e.dest.Headers))
		return nil
	}
	r.pipes[tenant] = p
	tenantPipelinesChanged(1)
	return p
}

// evictLRULocked closes the pipeline no span reached for longest. A linear
// scan: it runs only when a new pipeline needs room, and MaxPipelines bounds
// it.
func (r *TenantRouter) evictLRULocked() {
	var (
		oldest string
		at     int64
	)
	for t, p := range r.pipes {
		if u := p.lastUsed.Load(); oldest == "" || u < at {
			oldest, at = t, u
		}
	}
	if oldest != "" {
		r.dropPipelineLocked(oldest, r.pipes[oldest])
	}
}

// dropPipelineLocked takes p out of routing and flushes and closes it in the
// background: a flush waits on the tenant's collector, which must hold up
// neither the span that ended nor the caller changing a destination.
func (r *TenantRouter) dropPipelineLocked(tenant string, p *tenantPipeline) {
	delete(r.pipes, tenant)
	tenantPipelinesChanged(-1)
	// A second replaced pipeline while one still flushes is discarded: its
	// spans are counted dropped and its exporter takes no slot.
	flushes := !r.flushing[tenant] || r.closed
	if flushes {
		r.flushing[tenant] = true
	} else {
		p.discard.Store(true)
		// An export it already has in flight ends now, freeing its slot.
		p.cancel()
	}
	r.closing.Add(1)
	go func() {
		defer r.closing.Done()
		if flushes {
			defer func() {
				r.mu.Lock()
				delete(r.flushing, tenant)
				r.mu.Unlock()
			}()
		}
		defer func() {
			// A panic in one tenant's exporter must not end the process.
			if v := recover(); v != nil {
				r.cfg.Log.Error("otel: tenant export pipeline close panicked", "tenant", tenant, "panic", v)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), r.cfg.ExportTimeout)
		defer cancel()
		p.close(ctx)
	}()
}

// reapLoop closes pipelines left idle for IdleTimeout.
func (r *TenantRouter) reapLoop() {
	defer r.closing.Done()
	t := time.NewTicker(tenantReapInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			r.reapIdle(now)
		}
	}
}

func (r *TenantRouter) reapIdle(now time.Time) {
	defer func() {
		if v := recover(); v != nil {
			r.cfg.Log.Error("otel: tenant export idle reaper panicked", "panic", v)
		}
	}()
	cutoff := now.Add(-r.cfg.IdleTimeout).UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	for t, p := range r.pipes {
		if p.lastUsed.Load() < cutoff {
			r.dropPipelineLocked(t, p)
		}
	}
}

// Test sends one test span to d, as TestTenantExport does, inside the cap on
// exports in flight every tenant shares: with every slot held until timeout it
// answers busy rather than add an export beyond the cap. It touches no
// tenant's status.
func (r *TenantRouter) Test(ctx context.Context, tenant string, d TenantDestination, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = r.cfg.ExportTimeout
	}
	start := time.Now()
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
	case <-wait.Done():
		return &TenantExportError{msg: CategoryBusy}
	}
	defer func() { <-r.slots }()
	left := timeout - time.Since(start)
	if left <= 0 {
		return &TenantExportError{msg: CategoryBusy}
	}
	return TestTenantExport(ctx, tenant, d, r.cfg.ServiceVersion, left)
}

// ForceFlush flushes every open pipeline.
func (r *TenantRouter) ForceFlush(ctx context.Context) error {
	r.mu.RLock()
	pipes := make([]*tenantPipeline, 0, len(r.pipes))
	for _, p := range r.pipes {
		pipes = append(pipes, p)
	}
	r.mu.RUnlock()
	var errs []error
	for _, p := range pipes {
		errs = append(errs, p.bsp.ForceFlush(ctx))
	}
	return errors.Join(errs...)
}

// Shutdown flushes and closes every pipeline and stops the reaper. The router
// routes nothing afterwards.
func (r *TenantRouter) Shutdown(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	r.closed = true
	for t, p := range r.pipes {
		r.dropPipelineLocked(t, p)
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.closing.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tenantPipeline is one tenant's bounded queue and batch exporter.
type tenantPipeline struct {
	fp       string
	bsp      sdktrace.SpanProcessor
	stats    *tenantStats
	queue    int64
	inFlight atomic.Int64
	lastUsed atomic.Int64 // unix nanos
	tenant   string
	release  func() // closes what the exporter does not (a gRPC connection)

	// mu orders enqueue against close: a span handed over after the batch
	// processor shut down would be dropped by it uncounted.
	mu     sync.RWMutex
	closed bool
	// discard: the pipeline was replaced while an earlier one of its tenant
	// still flushed; what it holds is dropped, not exported.
	discard atomic.Bool
	// ctx is cancelled when the pipeline is discarded, ending its exports.
	ctx    context.Context
	cancel context.CancelFunc
}

func newTenantPipeline(tenant string, e *tenantEntry, cfg TenantRouterConfig, slots chan struct{}) (*tenantPipeline, error) {
	exp, release, err := newTenantExporter(context.Background(), e.dest, cfg.ExportTimeout)
	if err != nil {
		return nil, err
	}
	p := &tenantPipeline{fp: e.fp, stats: e.stats, queue: int64(cfg.QueueSize), tenant: tenant, release: release}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.lastUsed.Store(time.Now().UnixNano())
	te := &tenantSpanExporter{
		tenant: tenant, next: exp, res: tenantResource(tenant, cfg.ServiceVersion),
		slots: slots, p: p, headers: e.dest.Headers, log: cfg.Log,
	}
	p.bsp = sdktrace.NewBatchSpanProcessor(te,
		sdktrace.WithMaxQueueSize(cfg.QueueSize),
		sdktrace.WithMaxExportBatchSize(cfg.QueueSize),
		sdktrace.WithBatchTimeout(cfg.ScheduleDelay),
		sdktrace.WithExportTimeout(cfg.ExportTimeout),
	)
	return p, nil
}

// enqueue hands s to the batch processor, or drops and counts it when the
// queue is full. The count of spans held is kept here, ahead of the batch
// processor, because the one it keeps itself is not readable.
func (p *tenantPipeline) enqueue(s sdktrace.ReadOnlySpan) {
	p.lastUsed.Store(time.Now().UnixNano())
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed || p.inFlight.Add(1) > p.queue {
		if !p.closed {
			p.inFlight.Add(-1)
		}
		p.stats.dropped.Add(1)
		tenantSpansDropped(p.tenant, 1)
		return
	}
	p.bsp.OnEnd(s)
}

func (p *tenantPipeline) close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	defer p.cancel()
	_ = p.bsp.Shutdown(ctx)
	if p.release != nil {
		p.release()
	}
}

// tenantSpanExporter exports a pipeline's batch as the tenant's copy, inside
// the router's cap on exports in flight, and records the outcome. It returns
// nil whatever happened: an error would reach the process's OTEL error handler,
// which is the operator's collector's.
type tenantSpanExporter struct {
	tenant  string
	next    sdktrace.SpanExporter
	res     *sdkresource.Resource
	slots   chan struct{}
	p       *tenantPipeline
	headers map[string]string
	log     *slog.Logger
}

func (e *tenantSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	n := len(spans)
	defer e.p.inFlight.Add(-int64(n))
	if e.p.discard.Load() {
		e.discarded(n)
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(e.p.ctx, cancel)()
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		if e.p.discard.Load() {
			e.discarded(n)
			return nil
		}
		e.fail(uint64(n), CategoryBusy)
		return nil
	}
	defer func() { <-e.slots }()
	copies := make([]sdktrace.ReadOnlySpan, n)
	for i, s := range spans {
		copies[i] = newTenantSpan(s, e.res)
	}
	ctx, code := withStatusHolder(ctx)
	err := e.next.ExportSpans(ctx, copies)
	if err == nil {
		e.p.stats.exported.Add(uint64(n))
		tenantSpansExported(e.tenant, int64(n))
		e.p.stats.succeeded(time.Now())
		return nil
	}
	if rejected, partial := partialRejected(err); partial {
		r := min(uint64(max(rejected, 0)), uint64(n))
		e.p.stats.exported.Add(uint64(n) - r)
		e.p.stats.dropped.Add(r)
		tenantSpansExported(e.tenant, int64(uint64(n)-r))
		tenantSpansDropped(e.tenant, int64(r))
		e.p.stats.succeeded(time.Now())
		return nil
	}
	if e.p.discard.Load() {
		e.discarded(n)
		return nil
	}
	e.fail(uint64(n), exportCategory(err, code.code.Load()))
	// The operator's log has the error itself, scrubbed of header values,
	// URL credentials and query values.
	e.log.Debug("otel: tenant span export failed", "tenant", e.tenant, "spans", n, "err", scrubError(err, e.headers))
	return nil
}

// discarded counts n spans of a discarded pipeline as dropped. Not a failure:
// the collector was not at fault, the tenant replaced it.
func (e *tenantSpanExporter) discarded(n int) {
	e.p.stats.dropped.Add(uint64(n))
	tenantSpansDropped(e.tenant, int64(n))
}

func (e *tenantSpanExporter) fail(n uint64, msg string) {
	e.p.stats.failures.Add(1)
	e.p.stats.dropped.Add(n)
	tenantExportFailed(e.tenant, int64(n))
	e.p.stats.failed(time.Now(), msg)
}

func (e *tenantSpanExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }
