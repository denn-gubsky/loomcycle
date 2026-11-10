package otel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
	"go.opentelemetry.io/otel/trace"
)

// The in-process metrics. One instrument set, declared with the OTel metric
// API and recorded where the work happens (the run's start and end, the token
// ledger write, the provider attempt, the model call, the tool dispatch), has
// two exits: a manual reader whose snapshot GET /metrics renders as
// Prometheus text (WritePrometheus), and, when OTEL_METRICS_EXPORTER=otlp, a
// periodic OTLP export of the same instruments.
//
// They are counted in-process, so a sampled-out run is counted like any
// other: a rate derived downstream from exported spans sees only the sampled
// fraction of runs. Collecting them reads memory only — no instrument, and no
// callback behind an observable one, queries a database at scrape time.
//
// Labels are bounded by construction: tenant, provider, model, status,
// trigger, the collapsed tool name. A run, user, session or (unless the
// operator opts in) agent is never a label. Every instrument also has the
// SDK's cardinality limit, past which new label sets fold into one series
// marked otel_metric_overflow="true".

// MeterName is the instrumentation scope of the loomcycle_* instruments.
// /metrics renders this scope and no other.
const MeterName = TracerName

// genAIMeterName is the scope of the GenAI semantic-convention histograms.
// They duplicate loomcycle_* series under the convention's names for OTLP
// backends that key dashboards on them, so /metrics leaves them out.
const genAIMeterName = TracerName + "/genai"

// Label keys of the instruments. Plain names, not loomcycle.* attribute keys:
// they are Prometheus labels first, and a dashboard writes them by hand.
const (
	LabelTenant   = "tenant"
	LabelStatus   = "status"
	LabelTrigger  = "trigger"
	LabelAgent    = "agent"
	LabelProvider = "provider"
	LabelModel    = "model"
	LabelType     = "type"
	LabelKey      = "key"
	LabelOutcome  = "outcome"
	LabelReason   = "reason"
	LabelFrom     = "from"
	LabelTo       = "to"
	LabelTool     = "tool"
	LabelDB       = "db"
	LabelBackend  = "backend"
	LabelPeriod   = "period"
	LabelPurpose  = "purpose"
)

// Run triggers: the surface a run was started from.
const (
	TriggerHTTP     = "http"
	TriggerGRPC     = "grpc"
	TriggerMCP      = "mcp"
	TriggerSchedule = "schedule"
	TriggerWebhook  = "webhook"
	TriggerA2A      = "a2a"
	TriggerSubagent = "subagent"
	TriggerResume   = "resume"
)

// RunStatusUnknown is the status of a run whose span ended with no terminal
// status recorded on it: an internal error path that returned before the
// run's end was written.
const RunStatusUnknown = "unknown"

// Tool-call outcomes.
const (
	ToolOutcomeOK      = "ok"
	ToolOutcomeError   = "error"    // the tool returned a Go error
	ToolOutcomeIsError = "is_error" // the tool answered with an error result
)

// ProviderOutcomeOK is the outcome of a provider attempt that got a non-error
// response; every other outcome is one of the ErrorType* values, so a span's
// error.type and the metric's outcome label are the same string.
const ProviderOutcomeOK = "ok"

// The histogram buckets, in seconds. Each covers the range its operation
// actually takes: a provider answers its headers in well under a second to a
// minute, a model call streams for seconds to minutes, a run can take an hour.
var (
	runDurationBuckets     = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	providerRequestBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	modelCallBuckets       = []float64{0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 160, 320, 600}
	ttftBuckets            = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64}
	toolCallBuckets        = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}
	genAIDurationBuckets   = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	genAITokenUsageBuckets = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
)

// ConsolidationBackendInProcess is the backend label of the native memory
// layer's consolidation.
const ConsolidationBackendInProcess = "inprocess"

// instruments is the synchronous instrument set.
type instruments struct {
	runs                    metric.Int64Counter
	runDuration             metric.Float64Histogram
	runsActive              metric.Int64UpDownCounter
	tokens                  metric.Int64Counter
	costUSD                 metric.Float64Counter
	providerRequests        metric.Int64Counter
	providerRequestDuration metric.Float64Histogram
	providerRetries         metric.Int64Counter
	providerFallbacks       metric.Int64Counter
	modelCallDuration       metric.Float64Histogram
	modelTTFT               metric.Float64Histogram
	toolCalls               metric.Int64Counter
	toolCallDuration        metric.Float64Histogram

	genAITokenUsage        genaiconv.ClientTokenUsage
	genAIOperationDuration genaiconv.ClientOperationDuration
}

// meterState is the process's meter provider and the reader /metrics renders.
type meterState struct {
	provider   *sdkmetric.MeterProvider
	reader     *sdkmetric.ManualReader
	inst       instruments
	agentLabel bool
}

// meters is set once by InitMetrics, beside the tracer provider, and read by
// every recording site: the run, ledger, provider, model and tool sites have
// no handle threaded to them, as for spans. Nil before InitMetrics (and in
// tests that install none), which makes every recording a no-op.
var meters atomic.Pointer[meterState]

var initMetricsOnce sync.Once

// InitMetrics builds the process's meter provider: always a manual reader for
// /metrics, plus a periodic OTLP exporter when cfg.Export. Idempotent like
// Init. The returned shutdown flushes the exporter; the caller defers it with
// the trace shutdown.
func InitMetrics(cfg MetricsConfig) (func(context.Context) error, error) {
	var (
		fn  func(context.Context) error
		err error
	)
	ran := false
	initMetricsOnce.Do(func() {
		ran = true
		var st *meterState
		st, err = newMeterState(context.Background(), cfg, nil)
		if err != nil {
			return
		}
		meters.Store(st)
		fn = st.provider.Shutdown
	})
	if !ran {
		return noopShutdown, nil
	}
	if fn == nil {
		fn = noopShutdown
	}
	return fn, err
}

// newMeterState builds the provider and its instruments. extra readers are for
// tests (an in-memory stand-in for the OTLP reader).
func newMeterState(ctx context.Context, cfg MetricsConfig, extra []sdkmetric.Reader) (*meterState, error) {
	limit := cfg.CardinalityLimit
	if limit <= 0 {
		limit = defaultCardinalityLimit
	}
	reader := sdkmetric.NewManualReader()
	opts := []sdkmetric.Option{sdkmetric.WithReader(reader), sdkmetric.WithCardinalityLimit(limit)}
	export := cfg.Export || len(extra) > 0
	if !export {
		// Exemplars ride the OTLP path only; /metrics never renders them, so
		// without export collecting them is wasted work. With export the
		// SDK's default (trace-based, or OTEL_METRICS_EXEMPLAR_FILTER)
		// applies.
		opts = append(opts, sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter))
	}
	if cfg.Export {
		res, err := newResource(ctx, Config{ServiceVersion: cfg.ServiceVersion, InstanceID: cfg.InstanceID, Environment: cfg.Environment})
		if err != nil {
			return nil, err
		}
		exp, err := newMetricExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}
		// No interval or timeout option: OTEL_METRIC_EXPORT_INTERVAL and
		// _TIMEOUT are read by the reader itself, and an option would
		// override them.
		opts = append(opts, sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}
	for _, r := range extra {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	inst, err := newInstruments(mp.Meter(MeterName), mp.Meter(genAIMeterName, metric.WithSchemaURL("https://opentelemetry.io/schemas/"+GenAISemconvVersion)), export)
	if err != nil {
		return nil, err
	}
	st := &meterState{provider: mp, reader: reader, inst: inst, agentLabel: cfg.AgentLabel}
	if err := registerObservables(mp.Meter(MeterName)); err != nil {
		return nil, err
	}
	return st, nil
}

func newInstruments(m, genAI metric.Meter, export bool) (instruments, error) {
	var (
		in   instruments
		errs []error
	)
	add := func(err error) { errs = append(errs, err) }
	hist := func(name, unit, desc string, buckets []float64) metric.Float64Histogram {
		h, err := m.Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(desc), metric.WithExplicitBucketBoundaries(buckets...))
		add(err)
		return h
	}
	counter := func(name, unit, desc string) metric.Int64Counter {
		c, err := m.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		add(err)
		return c
	}
	var err error
	in.runs = counter("loomcycle.runs", "{run}", "Runs that ended, by tenant, terminal status and the surface that started them.")
	in.runDuration = hist("loomcycle.run.duration", "s", "Wall time of a run from its start to its end.", runDurationBuckets)
	in.runsActive, err = m.Int64UpDownCounter("loomcycle.runs.active", metric.WithUnit("{run}"), metric.WithDescription("Runs started and not yet ended on this replica."))
	add(err)
	in.tokens = counter("loomcycle.tokens", "{token}", "Tokens written to the usage ledger, by type and by who paid (key).")
	in.costUSD, err = m.Float64Counter("loomcycle.cost_usd", metric.WithUnit("{USD}"), metric.WithDescription("Cost in US dollars written to the usage ledger, for calls priced in US dollars."))
	add(err)
	in.providerRequests = counter("loomcycle.provider.requests", "{request}", "HTTP requests (attempts) to model providers, by outcome.")
	in.providerRequestDuration = hist("loomcycle.provider.request.duration", "s", "Time from sending a provider request to its response headers (or its failure).", providerRequestBuckets)
	in.providerRetries = counter("loomcycle.provider.retries", "{retry}", "Provider requests retried after a rate limit, by where the wait came from (header or schedule).")
	in.providerFallbacks = counter("loomcycle.provider.fallbacks", "{fallback}", "Runs moved from one provider/model to another mid-run.")
	in.modelCallDuration = hist("loomcycle.model.call.duration", "s", "Time of a model call from the request to the end of its stream, by purpose (turn, compaction, recap, emit_state, summary, gateway).", modelCallBuckets)
	in.modelTTFT = hist("loomcycle.model.ttft", "s", "Time from a model call's lane admission (else its start) to its first streamed event, by purpose.", ttftBuckets)
	in.toolCalls = counter("loomcycle.tool.calls", "{call}", "Tool calls by tool (MCP tools by server, client tools as client) and outcome.")
	in.toolCallDuration = hist("loomcycle.tool.call.duration", "s", "Time a tool call took.", toolCallBuckets)

	// The GenAI convention histograms go out over OTLP only; with export off
	// they are no-ops, so nothing aggregates what no reader exports.
	if !export {
		genAI = noop.NewMeterProvider().Meter(genAIMeterName)
	}
	in.genAITokenUsage, err = genaiconv.NewClientTokenUsage(genAI, metric.WithExplicitBucketBoundaries(genAITokenUsageBuckets...))
	add(err)
	in.genAIOperationDuration, err = genaiconv.NewClientOperationDuration(genAI, metric.WithExplicitBucketBoundaries(genAIDurationBuckets...))
	add(err)
	if err := errors.Join(errs...); err != nil {
		return instruments{}, fmt.Errorf("otel: build metric instruments: %w", err)
	}
	return in, nil
}

// newMetricExporter builds the OTLP metric exporter for cfg.Protocol, wrapped
// so its failures are logged by loomcycle (throttled, value-free) instead of
// by the SDK's default handler.
func newMetricExporter(ctx context.Context, cfg MetricsConfig) (sdkmetric.Exporter, error) {
	headers := cfg.Headers
	if headers == nil && cfg.HeadersFile != "" {
		var err error
		if headers, err = ReadHeadersFile(cfg.HeadersFile); err != nil {
			return nil, err
		}
	}
	var (
		exp sdkmetric.Exporter
		err error
	)
	switch cfg.Protocol {
	case ProtocolGRPC:
		var opts []otlpmetricgrpc.Option
		if len(headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(headers))
		}
		exp, err = otlpmetricgrpc.New(ctx, opts...)
	default:
		var opts []otlpmetrichttp.Option
		if len(headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(headers))
		}
		exp, err = otlpmetrichttp.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("otel: build %s metric exporter: %w", cfg.Protocol, err)
	}
	return &loggingMetricExporter{Exporter: exp, log: newMetricErrorLog(time.Now)}, nil
}

// SetMetricsForTest installs a fresh meter state for the duration of a test
// and returns the restore function. agentLabel and limit configure it as
// LOOMCYCLE_METRICS_AGENT_LABEL and LOOMCYCLE_METRICS_CARDINALITY_LIMIT do;
// extra readers (an in-memory OTLP stand-in) also turn the GenAI histograms on.
func SetMetricsForTest(agentLabel bool, limit int, extra ...sdkmetric.Reader) (func(), error) {
	st, err := newMeterState(context.Background(), MetricsConfig{AgentLabel: agentLabel, CardinalityLimit: limit}, extra)
	if err != nil {
		return nil, err
	}
	prev := meters.Swap(st)
	return func() {
		meters.Store(prev)
		_ = st.provider.Shutdown(context.Background())
	}, nil
}

// collect is the current snapshot of every instrument, nil when metrics are
// not initialised.
func collect(ctx context.Context) (*metricdata.ResourceMetrics, error) {
	st := meters.Load()
	if st == nil {
		return nil, nil
	}
	var rm metricdata.ResourceMetrics
	if err := st.reader.Collect(ctx, &rm); err != nil {
		return nil, err
	}
	return &rm, nil
}

// --- Runs ---

// runMetric is a run's share of the run instruments: it counts the run active
// from its start and, once, ended with its status at its end.
type runMetric struct {
	st      *meterState
	tenant  string
	trigger string
	agent   string
	start   time.Time
	status  atomic.Pointer[string]
	ended   atomic.Bool
}

func startRunMetric(ctx context.Context, tenant, trigger, agent string) *runMetric {
	st := meters.Load()
	if st == nil {
		return nil
	}
	if trigger == "" {
		trigger = TriggerHTTP
	}
	m := &runMetric{st: st, tenant: tenant, trigger: trigger, agent: agent, start: time.Now()}
	st.inst.runsActive.Add(ctx, 1, metric.WithAttributes(attribute.String(LabelTenant, tenant)))
	return m
}

func (m *runMetric) setStatus(status string) {
	if m != nil && status != "" {
		m.status.Store(&status)
	}
}

// end records the run once, with the status SetRunDone left on it.
func (m *runMetric) end(ctx context.Context) {
	if m == nil || !m.ended.CompareAndSwap(false, true) {
		return
	}
	status := RunStatusUnknown
	if s := m.status.Load(); s != nil {
		status = *s
	}
	tenant := attribute.String(LabelTenant, m.tenant)
	m.st.inst.runsActive.Add(ctx, -1, metric.WithAttributes(tenant))
	kvs := []attribute.KeyValue{tenant, attribute.String(LabelStatus, status), attribute.String(LabelTrigger, m.trigger)}
	if m.st.agentLabel {
		kvs = append(kvs, attribute.String(LabelAgent, m.agent))
	}
	m.st.inst.runs.Add(ctx, 1, metric.WithAttributes(kvs...))
	m.st.inst.runDuration.Record(ctx, time.Since(m.start).Seconds(),
		metric.WithAttributes(tenant, attribute.String(LabelStatus, status)))
}

// meteredRunSpan is the loomcycle.run span with its run metric: ending the span ends
// the run's count. Tying the two together is what keeps runs_active honest —
// every run-creation site already ends its span on every path, including a
// run handed off to a background goroutine, and the metric rides that.
type meteredRunSpan struct {
	trace.Span
	m *runMetric
}

func (s *meteredRunSpan) End(opts ...trace.SpanEndOption) {
	s.m.end(trace.ContextWithSpan(context.Background(), s.Span))
	s.Span.End(opts...)
}

type runTriggerKey struct{}

// WithRunTrigger returns ctx naming the surface the runs started under it
// come from. Each transport sets it once at its entry point; a later call
// overrides an earlier one, so the MCP surface served over HTTP is "mcp".
func WithRunTrigger(ctx context.Context, trigger string) context.Context {
	if trigger == "" {
		return ctx
	}
	return context.WithValue(ctx, runTriggerKey{}, trigger)
}

// RunTriggerFromContext returns the trigger WithRunTrigger put on ctx, "" when
// none.
func RunTriggerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(runTriggerKey{}).(string)
	return v
}

// --- The token ledger ---

// UsageRecord is one token-ledger row as the metrics count it.
type UsageRecord struct {
	Tenant   string
	Provider string
	Model    string
	// Agent is the run's agent name; labelled only with
	// LOOMCYCLE_METRICS_AGENT_LABEL.
	Agent string
	// Key is who paid, as the ledger records it: operator, tenant or user.
	Key        string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	// CostUSD is counted when CostKnown: the row is priced in US dollars.
	CostUSD   float64
	CostKnown bool
	// Chat marks a model turn (chat, consolidation, gateway). Only those feed
	// the GenAI token-usage histogram, whose operation is chat.
	Chat bool
}

type agentNameKey struct{}

// WithAgentName returns ctx naming the agent whose usage is recorded under it;
// RecordUsage takes it when the record names none.
func WithAgentName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, agentNameKey{}, name)
}

// RecordUsage counts one ledger row. Called where the row is written, so the
// counters move with the ledger and with nothing else.
func RecordUsage(ctx context.Context, u UsageRecord) {
	st := meters.Load()
	if st == nil {
		return
	}
	if u.Agent == "" {
		u.Agent, _ = ctx.Value(agentNameKey{}).(string)
	}
	u.Model = metricModel(u.Model)
	base := []attribute.KeyValue{
		attribute.String(LabelTenant, u.Tenant),
		attribute.String(LabelProvider, u.Provider),
		attribute.String(LabelModel, u.Model),
		attribute.String(LabelKey, u.Key),
	}
	if st.agentLabel {
		base = append(base, attribute.String(LabelAgent, u.Agent))
	}
	for _, t := range []struct {
		typ string
		n   int64
	}{{"input", u.Input}, {"output", u.Output}, {"cache_read", u.CacheRead}, {"cache_write", u.CacheWrite}} {
		if t.n <= 0 {
			continue
		}
		kvs := append(base[:len(base):len(base)], attribute.String(LabelType, t.typ))
		st.inst.tokens.Add(ctx, t.n, metric.WithAttributes(kvs...))
	}
	if u.CostKnown && u.CostUSD > 0 {
		st.inst.costUSD.Add(ctx, u.CostUSD, metric.WithAttributes(
			attribute.String(LabelTenant, u.Tenant),
			attribute.String(LabelProvider, u.Provider),
			attribute.String(LabelModel, u.Model)))
	}
	if u.Chat {
		provider := genaiconv.ProviderNameAttr(GenAIProviderName(u.Provider))
		model := attribute.String("gen_ai.request.model", u.Model)
		if u.Input > 0 {
			st.inst.genAITokenUsage.Record(ctx, u.Input, genaiconv.OperationNameChat, provider, genaiconv.TokenTypeInput, model)
		}
		if u.Output > 0 {
			st.inst.genAITokenUsage.Record(ctx, u.Output, genaiconv.OperationNameChat, provider, genaiconv.TokenTypeOutput, model)
		}
	}
}

// --- Provider attempts and model calls ---

// providerSpan is a loomcycle.provider.call span with what its attempt
// metrics need. SetProviderResponse records them, whatever the span's
// sampling, so the metric and the span's error.type come from one
// classification.
type providerSpan struct {
	trace.Span
	st              *meterState
	provider, model string
	start           time.Time
}

// recordAttempt counts one provider attempt that ended with outcome.
func (p *providerSpan) recordAttempt(outcome string) {
	ctx := trace.ContextWithSpan(context.Background(), p.Span)
	pm := []attribute.KeyValue{attribute.String(LabelProvider, p.provider), attribute.String(LabelModel, p.model)}
	p.st.inst.providerRequests.Add(ctx, 1, metric.WithAttributes(append(pm[:2:2], attribute.String(LabelOutcome, outcome))...))
	p.st.inst.providerRequestDuration.Record(ctx, time.Since(p.start).Seconds(), metric.WithAttributes(pm...))
}

// modelSpan is a loomcycle.model.call span with what its call metrics need.
type modelSpan struct {
	trace.Span
	st              *meterState
	provider, model string
	// purpose is the call's loomcycle.model.purpose (turn, compaction,
	// recap, emit_state, summary, gateway): a set the runtime names, so a
	// label, and the gateway's calls show apart from the runs'.
	purpose  string
	start    time.Time
	recorded atomic.Bool
}

func (m *modelSpan) recordResult(r ModelCallResult) {
	if !m.recorded.CompareAndSwap(false, true) {
		return
	}
	ctx := trace.ContextWithSpan(context.Background(), m.Span)
	pm := metric.WithAttributes(attribute.String(LabelProvider, m.provider), attribute.String(LabelModel, m.model),
		attribute.String(LabelPurpose, m.purpose))
	elapsed := time.Since(m.start).Seconds()
	m.st.inst.modelCallDuration.Record(ctx, elapsed, pm)
	if r.TTFT > 0 {
		m.st.inst.modelTTFT.Record(ctx, r.TTFT.Seconds(), pm)
	}
	extra := []attribute.KeyValue{attribute.String("gen_ai.request.model", m.model)}
	switch {
	case r.Cancelled:
		extra = append(extra, attribute.String("error.type", ErrorTypeCancelled))
	case r.Err != nil || r.ErrMessage != "":
		// The convention's fallback class: the call failed and no finer
		// class is known at the end of a stream.
		extra = append(extra, attribute.String("error.type", "_OTHER"))
	}
	m.st.inst.genAIOperationDuration.Record(ctx, elapsed, genaiconv.OperationNameChat,
		genaiconv.ProviderNameAttr(GenAIProviderName(m.provider)), extra...)
}

// RecordProviderRetry counts a provider request retried after a rate limit.
// reason is where the wait came from: header or schedule.
func RecordProviderRetry(ctx context.Context, provider, reason string) {
	st := meters.Load()
	if st == nil {
		return
	}
	st.inst.providerRetries.Add(ctx, 1, metric.WithAttributes(
		attribute.String(LabelProvider, provider), attribute.String(LabelReason, reason)))
}

// RecordProviderFallback counts a run moving from one provider and model to
// another, labelled from="provider/model" and to="provider/model".
func RecordProviderFallback(ctx context.Context, fromProvider, fromModel, toProvider, toModel string) {
	st := meters.Load()
	if st == nil {
		return
	}
	st.inst.providerFallbacks.Add(ctx, 1, metric.WithAttributes(
		attribute.String(LabelFrom, fromProvider+"/"+metricModel(fromModel)),
		attribute.String(LabelTo, toProvider+"/"+metricModel(toModel))))
}

// MetricUserHash is the user_hash label naming user in tenant: the
// loomcycle.user_hash value its spans carry, so a series and a trace name a
// user alike. ok is false with no secret key: a hash keyed by nothing could be
// reversed by trying ids, and a raw id is never a label.
func MetricUserHash(tenant, user string) (string, bool) {
	p := userPolicy.Load()
	if p == nil || len(p.key) == 0 || user == "" {
		return "", false
	}
	return UserHash(p.key, tenant, user), true
}

// --- Tools ---

// ToolMetricName is the tool label for a dispatched tool name. An MCP tool
// collapses to its server (mcp:<server>) and a client-hosted tool to
// "client": their tool sets are chosen by the server's or the client's owner,
// not bounded by the runtime. known is whether the name is one the run was
// given; a name the model made up is "unknown", never a label of its own.
func ToolMetricName(name string, known bool) string {
	if !known {
		return "unknown"
	}
	if server, _ := ParseMCPToolName(name); server != "" {
		return "mcp:" + server
	}
	if len(name) > len(clientToolPrefix) && name[:len(clientToolPrefix)] == clientToolPrefix {
		return "client"
	}
	return name
}

// clientToolPrefix mirrors internal/clienttools.ToolPrefix, which this leaf
// module cannot import.
const clientToolPrefix = "client__"

// RecordToolCallMetric counts one tool call. tool is ToolMetricName's label;
// outcome is one of the ToolOutcome* values.
func RecordToolCallMetric(ctx context.Context, tool, outcome string, took time.Duration) {
	st := meters.Load()
	if st == nil {
		return
	}
	t := attribute.String(LabelTool, tool)
	st.inst.toolCalls.Add(ctx, 1, metric.WithAttributes(t, attribute.String(LabelOutcome, outcome)))
	st.inst.toolCallDuration.Record(ctx, took.Seconds(), metric.WithAttributes(t))
}

// --- Observable gauges: database pools, consolidation, token budgets ---

// DBPoolStats is a database handle's connection pool, read without a query
// (sql.DB.Stats, pgxpool.Stat).
type DBPoolStats struct {
	Open, InUse, Idle int
	// WaitCount is the acquires that had to wait for a connection, and
	// WaitDuration their total wait, both since the pool was opened.
	WaitCount    int64
	WaitDuration time.Duration
}

// TokenBudget is one tenant's token budget: what it used of its limit in the
// current period.
type TokenBudget struct {
	Tenant      string
	Period      string
	Used, Limit int64
}

// observables holds the sources the observable instruments read at collect
// time. Process-wide like the meter provider: the store, the consolidator and
// the budget tracker are built by the binary after InitMetrics and register
// themselves here.
type observables struct {
	mu      sync.Mutex
	dbPools map[string]func() DBPoolStats
	budgets func() []TokenBudget
	consol  map[string]*consolidationGauge
}

type consolidationGauge struct {
	watermark atomic.Int64 // unix nanos of the latest pass's watermark; 0 = none known
	pending   atomic.Int64
	hasPend   atomic.Bool
}

var obs = observables{dbPools: map[string]func() DBPoolStats{}, consol: map[string]*consolidationGauge{}}

// RegisterDBPool reports the pool stats of the database named name (the db
// label) on every collection; stats must not query the database. Returns the
// function that removes it.
func RegisterDBPool(name string, stats func() DBPoolStats) func() {
	obs.mu.Lock()
	obs.dbPools[name] = stats
	obs.mu.Unlock()
	return func() {
		obs.mu.Lock()
		delete(obs.dbPools, name)
		obs.mu.Unlock()
	}
}

// SetTokenBudgetSource sets where the token-budget gauge reads tenants'
// budgets; budgets must read memory only. Returns the restore function.
func SetTokenBudgetSource(budgets func() []TokenBudget) func() {
	obs.mu.Lock()
	prev := obs.budgets
	obs.budgets = budgets
	obs.mu.Unlock()
	return func() {
		obs.mu.Lock()
		obs.budgets = prev
		obs.mu.Unlock()
	}
}

func consolidationGaugeFor(backend string) *consolidationGauge {
	obs.mu.Lock()
	defer obs.mu.Unlock()
	g, ok := obs.consol[backend]
	if !ok {
		g = &consolidationGauge{}
		obs.consol[backend] = g
	}
	return g
}

// RecordConsolidationWatermark notes the watermark the latest consolidation
// pass on backend left its target at. The lag gauge is now minus it, read at
// collection, so a consolidator that stops completing passes shows a lag that
// keeps growing instead of the last one it reported.
func RecordConsolidationWatermark(backend string, watermark time.Time) {
	if watermark.IsZero() {
		// A target that never advanced has no lag to report, not a lag of 0.
		return
	}
	consolidationGaugeFor(backend).watermark.Store(watermark.UnixNano())
}

// SetConsolidationPending notes how many targets had consolidation work
// waiting when the backend's latest fan-out looked.
func SetConsolidationPending(backend string, n int) {
	g := consolidationGaugeFor(backend)
	g.pending.Store(int64(n))
	g.hasPend.Store(true)
}

// ResetObservablesForTest clears every registered observable source.
func ResetObservablesForTest() {
	obs.mu.Lock()
	defer obs.mu.Unlock()
	obs.dbPools = map[string]func() DBPoolStats{}
	obs.budgets = nil
	obs.consol = map[string]*consolidationGauge{}
}

func registerObservables(m metric.Meter) error {
	var errs []error
	gauge := func(name, unit, desc string) metric.Int64ObservableGauge {
		g, err := m.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return g
	}
	open := gauge("loomcycle.db.pool.open", "{connection}", "Connections a database pool holds open.")
	inUse := gauge("loomcycle.db.pool.in_use", "{connection}", "Connections of a database pool in use.")
	idle := gauge("loomcycle.db.pool.idle", "{connection}", "Idle connections of a database pool.")
	waits, err := m.Int64ObservableCounter("loomcycle.db.pool.wait", metric.WithUnit("{wait}"),
		metric.WithDescription("Connection acquires that had to wait for a free connection."))
	errs = append(errs, err)
	waitTime, err := m.Float64ObservableCounter("loomcycle.db.pool.wait.duration", metric.WithUnit("s"),
		metric.WithDescription("Total time connection acquires spent waiting for a free connection."))
	errs = append(errs, err)
	lag, err := m.Float64ObservableGauge("loomcycle.memory.consolidation.lag", metric.WithUnit("s"),
		metric.WithDescription("Now minus the watermark (the newest settled chat folded in) the latest consolidation pass left its target at. It grows while nothing new arrives too, so read it with loomcycle_memory_consolidation_pending: lag is a backlog only while targets have work waiting."))
	errs = append(errs, err)
	pending := gauge("loomcycle.memory.consolidation.pending", "{target}", "Consolidation targets with work waiting at the latest fan-out.")
	budget, err := m.Float64ObservableGauge("loomcycle.token_budget.used", metric.WithUnit("1"),
		metric.WithDescription("Fraction of a tenant's token budget used in the current period (the hard limit, else the soft one)."))
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("otel: build observable instruments: %w", err)
	}

	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		obs.mu.Lock()
		pools := make([]string, 0, len(obs.dbPools))
		for n := range obs.dbPools {
			pools = append(pools, n)
		}
		sort.Strings(pools)
		fns := make([]func() DBPoolStats, len(pools))
		for i, n := range pools {
			fns[i] = obs.dbPools[n]
		}
		budgets := obs.budgets
		backends := make([]string, 0, len(obs.consol))
		for b := range obs.consol {
			backends = append(backends, b)
		}
		sort.Strings(backends)
		gauges := make([]*consolidationGauge, len(backends))
		for i, b := range backends {
			gauges[i] = obs.consol[b]
		}
		obs.mu.Unlock()

		for i, n := range pools {
			s := fns[i]()
			db := metric.WithAttributes(attribute.String(LabelDB, n))
			o.ObserveInt64(open, int64(s.Open), db)
			o.ObserveInt64(inUse, int64(s.InUse), db)
			o.ObserveInt64(idle, int64(s.Idle), db)
			o.ObserveInt64(waits, s.WaitCount, db)
			o.ObserveFloat64(waitTime, s.WaitDuration.Seconds(), db)
		}
		now := time.Now()
		for i, b := range backends {
			g := gauges[i]
			be := metric.WithAttributes(attribute.String(LabelBackend, b))
			if w := g.watermark.Load(); w != 0 {
				o.ObserveFloat64(lag, max(0, now.Sub(time.Unix(0, w)).Seconds()), be)
			}
			if g.hasPend.Load() {
				o.ObserveInt64(pending, g.pending.Load(), be)
			}
		}
		if budgets != nil {
			for _, b := range budgets() {
				if b.Limit <= 0 {
					continue
				}
				o.ObserveFloat64(budget, float64(b.Used)/float64(b.Limit), metric.WithAttributes(
					attribute.String(LabelTenant, b.Tenant), attribute.String(LabelPeriod, b.Period)))
			}
		}
		return nil
	}, open, inUse, idle, waits, waitTime, lag, pending, budget)
	return err
}
