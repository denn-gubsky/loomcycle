package otel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func attrs(kv ...string) attribute.Set {
	var kvs []attribute.KeyValue
	for i := 0; i+1 < len(kv); i += 2 {
		kvs = append(kvs, attribute.String(kv[i], kv[i+1]))
	}
	return attribute.NewSet(kvs...)
}

// goldenSnapshot is one of each data kind, data points out of order, plus a
// scope /metrics must not render.
func goldenSnapshot() *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{
		{Scope: instrumentation.Scope{Name: genAIMeterName}, Metrics: []metricdata.Metrics{{
			Name: "gen_ai.client.token.usage", Unit: "{token}",
			Data: metricdata.Histogram[int64]{DataPoints: []metricdata.HistogramDataPoint[int64]{{Count: 1}}},
		}}},
		{Scope: instrumentation.Scope{Name: MeterName}, Metrics: []metricdata.Metrics{
			{Name: "loomcycle.runs", Unit: "{run}", Description: "Runs that ended.",
				Data: metricdata.Sum[int64]{IsMonotonic: true, DataPoints: []metricdata.DataPoint[int64]{
					{Attributes: attrs("tenant", "beta", "status", "failed"), Value: 2},
					{Attributes: attrs("tenant", "acme", "status", "completed"), Value: 5},
				}}},
			{Name: "loomcycle.runs.active", Unit: "{run}", Description: "Active runs.",
				Data: metricdata.Sum[int64]{IsMonotonic: false, DataPoints: []metricdata.DataPoint[int64]{
					{Attributes: attrs("tenant", "acme"), Value: 1},
				}}},
			{Name: "loomcycle.token_budget.used", Unit: "1", Description: "Budget used.\nSecond \\ line.",
				Data: metricdata.Gauge[float64]{DataPoints: []metricdata.DataPoint[float64]{
					{Attributes: attrs("tenant", `we"ird\name`+"\n"), Value: 0.5},
				}}},
			{Name: "loomcycle.run.duration", Unit: "s", Description: "Run time.",
				Data: metricdata.Histogram[float64]{DataPoints: []metricdata.HistogramDataPoint[float64]{{
					Attributes: attrs("tenant", "acme"), Bounds: []float64{1, 2.5}, BucketCounts: []uint64{1, 2, 3}, Count: 6, Sum: 12.5,
				}}}},
		}},
	}}
}

// The exposition: families sorted by name, series by labels, every series
// carrying the extra (replica) labels, counters suffixed _total, the unit's
// suffix, histograms as cumulative buckets with +Inf, escaped labels and HELP.
func TestRenderPrometheus_GoldenFormat(t *testing.T) {
	got := renderPrometheus(goldenSnapshot(), PromOptions{Labels: map[string]string{"replica_id": "r1"}})
	want := `# HELP loomcycle_run_duration_seconds Run time.
# TYPE loomcycle_run_duration_seconds histogram
loomcycle_run_duration_seconds_bucket{le="1",replica_id="r1",tenant="acme"} 1
loomcycle_run_duration_seconds_bucket{le="2.5",replica_id="r1",tenant="acme"} 3
loomcycle_run_duration_seconds_bucket{le="+Inf",replica_id="r1",tenant="acme"} 6
loomcycle_run_duration_seconds_sum{replica_id="r1",tenant="acme"} 12.5
loomcycle_run_duration_seconds_count{replica_id="r1",tenant="acme"} 6

# HELP loomcycle_runs_active Active runs.
# TYPE loomcycle_runs_active gauge
loomcycle_runs_active{replica_id="r1",tenant="acme"} 1

# HELP loomcycle_runs_total Runs that ended.
# TYPE loomcycle_runs_total counter
loomcycle_runs_total{replica_id="r1",status="completed",tenant="acme"} 5
loomcycle_runs_total{replica_id="r1",status="failed",tenant="beta"} 2

# HELP loomcycle_token_budget_used_ratio Budget used.\nSecond \\ line.
# TYPE loomcycle_token_budget_used_ratio gauge
loomcycle_token_budget_used_ratio{replica_id="r1",tenant="we\"ird\\name\n"} 0.5

`
	if got != want {
		t.Errorf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A tenant reader gets its own tenant's series and nothing else: no other
// tenant's, and no series without a tenant label (which is operator-wide).
func TestRenderPrometheus_TenantViewKeepsOnlyThatTenantsSeries(t *testing.T) {
	rm := goldenSnapshot()
	rm.ScopeMetrics[1].Metrics = append(rm.ScopeMetrics[1].Metrics,
		metricdata.Metrics{Name: "loomcycle.db.pool.open", Unit: "{connection}",
			Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: attrs("db", "store"), Value: 3}}}},
		metricdata.Metrics{Name: "loomcycle.tokens", Unit: "{token}",
			Data: metricdata.Sum[int64]{IsMonotonic: true, DataPoints: []metricdata.DataPoint[int64]{
				{Attributes: attribute.NewSet(attribute.Bool("otel.metric.overflow", true)), Value: 9},
				{Attributes: attrs("tenant", "acme", "type", "input"), Value: 4},
			}}},
	)
	got := renderPrometheus(rm, PromOptions{Tenant: "acme"})
	for _, absent := range []string{"beta", "weird", "loomcycle_db_pool_open", "otel_metric_overflow", "loomcycle_token_budget_used_ratio", "gen_ai"} {
		if strings.Contains(got, absent) {
			t.Errorf("tenant view contains %q:\n%s", absent, got)
		}
	}
	for _, present := range []string{
		`loomcycle_runs_total{status="completed",tenant="acme"} 5`,
		`loomcycle_runs_active{tenant="acme"} 1`,
		`loomcycle_tokens_total{tenant="acme",type="input"} 4`,
		`loomcycle_run_duration_seconds_count{tenant="acme"} 6`,
	} {
		if !strings.Contains(got, present) {
			t.Errorf("tenant view lacks %q:\n%s", present, got)
		}
	}
	// A family the tenant has no series in leaves no HELP/TYPE lines behind.
	if strings.Contains(got, "# TYPE loomcycle_db_pool_open") {
		t.Error("empty family header rendered")
	}
}

func TestRenderPrometheus_OmitUserSeriesDropsUserLabelledSeries(t *testing.T) {
	rm := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: MeterName}, Metrics: []metricdata.Metrics{
		{Name: "loomcycle.x", Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{
			{Attributes: attrs("user_hash", "abc"), Value: 1},
			{Attributes: attrs("tenant", "t"), Value: 2},
		}}},
	}}}}
	if got := renderPrometheus(rm, PromOptions{}); !strings.Contains(got, `user_hash="abc"`) {
		t.Fatalf("the fixture is wrong: the full view lacks the user series:\n%s", got)
	}
	got := renderPrometheus(rm, PromOptions{OmitUserSeries: true})
	if strings.Contains(got, "user_hash") || !strings.Contains(got, `loomcycle_x{tenant="t"} 2`) {
		t.Errorf("omit-user view = \n%s", got)
	}
}

func TestPromName_TranslatesUnitsAndCounters(t *testing.T) {
	for _, tc := range []struct{ name, unit, typ, want string }{
		{"loomcycle.runs", "{run}", "counter", "loomcycle_runs_total"},
		{"loomcycle.cost_usd", "{USD}", "counter", "loomcycle_cost_usd_total"},
		{"loomcycle.db.pool.wait.duration", "s", "counter", "loomcycle_db_pool_wait_duration_seconds_total"},
		{"loomcycle.model.ttft", "s", "histogram", "loomcycle_model_ttft_seconds"},
		{"loomcycle.token_budget.used", "1", "gauge", "loomcycle_token_budget_used_ratio"},
		{"x.size", "By", "gauge", "x_size_bytes"},
		{"already_seconds", "s", "gauge", "already_seconds"},
		{"a-b/c", "", "gauge", "a_b_c"},
	} {
		if got := PromName(tc.name, tc.unit, tc.typ); got != tc.want {
			t.Errorf("PromName(%q, %q, %q) = %q, want %q", tc.name, tc.unit, tc.typ, got, tc.want)
		}
	}
}

// OTLP metric export is off unless asked for; asked for, it needs a
// collector; only then are its own variables validated.
func TestResolveMetricsConfig_EnvironmentTable(t *testing.T) {
	const ep = "http://collector:4318"
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c MetricsConfig)
	}{
		{name: "default: no export, default limit", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep}, check: func(t *testing.T, c MetricsConfig) {
			if c.Export || c.CardinalityLimit != 2000 || c.AgentLabel {
				t.Errorf("defaults = %+v", c)
			}
		}},
		{name: "none", env: map[string]string{"OTEL_METRICS_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": ep}, check: func(t *testing.T, c MetricsConfig) {
			if c.Export {
				t.Error("export on with none")
			}
		}},
		{name: "otlp with endpoint", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep}, check: func(t *testing.T, c MetricsConfig) {
			if !c.Export || c.Protocol != ProtocolHTTPProtobuf {
				t.Errorf("cfg = %+v", c)
			}
		}},
		{name: "otlp with metrics endpoint", env: map[string]string{"OTEL_METRICS_EXPORTER": "OTLP", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": ep + "/v1/metrics"}, check: func(t *testing.T, c MetricsConfig) {
			if !c.Export {
				t.Error("export off")
			}
		}},
		{name: "otlp without endpoint stops the boot", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp"}, wantErr: "needs OTEL_EXPORTER_OTLP_ENDPOINT"},
		{name: "sdk disabled turns export off", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_SDK_DISABLED": "true"}, check: func(t *testing.T, c MetricsConfig) {
			if c.Export {
				t.Error("export on with the SDK disabled")
			}
		}},
		{name: "unsupported exporter is ignored with a warning", env: map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"}, check: func(t *testing.T, c MetricsConfig) {
			if c.Export || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], `"prometheus"`) {
				t.Errorf("cfg = %+v", c)
			}
		}},
		{name: "unsupported beside otlp still exports", env: map[string]string{"OTEL_METRICS_EXPORTER": "console,otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep}, check: func(t *testing.T, c MetricsConfig) {
			if !c.Export || len(c.Warnings) != 1 {
				t.Errorf("cfg = %+v", c)
			}
		}},
		{name: "malformed endpoint with export on", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "collector:4318"}, wantErr: "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT must be an absolute"},
		{name: "malformed endpoint with export off is ignored", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"}, check: func(t *testing.T, c MetricsConfig) {
			if c.Export {
				t.Error("export on")
			}
		}},
		{name: "malformed metrics headers with export on", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_EXPORTER_OTLP_METRICS_HEADERS": "no-equals-sign"}, wantErr: "OTEL_EXPORTER_OTLP_METRICS_HEADERS"},
		{name: "grpc by the generic protocol", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, check: func(t *testing.T, c MetricsConfig) {
			if c.Protocol != ProtocolGRPC {
				t.Errorf("protocol = %q", c.Protocol)
			}
		}},
		{name: "metrics protocol wins", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"}, check: func(t *testing.T, c MetricsConfig) {
			if c.Protocol != ProtocolGRPC {
				t.Errorf("protocol = %q", c.Protocol)
			}
		}},
		{name: "bad metrics protocol with export on", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json"}, wantErr: "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"},
		{name: "bad metrics protocol with export off is ignored", env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json"}, check: func(t *testing.T, c MetricsConfig) {
			if c.Export {
				t.Error("export on")
			}
		}},
		{name: "headers file applies to metrics", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep, "LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE": "/run/secrets/otel"}, check: func(t *testing.T, c MetricsConfig) {
			if c.HeadersFile != "/run/secrets/otel" {
				t.Errorf("headers file = %q", c.HeadersFile)
			}
		}},
		{name: "headers file beside metrics headers", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": ep, "LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE": "/f", "OTEL_EXPORTER_OTLP_METRICS_HEADERS": "a=b"}, wantErr: "OTEL_EXPORTER_OTLP_METRICS_HEADERS are both set"},
		{name: "cardinality limit", env: map[string]string{"LOOMCYCLE_METRICS_CARDINALITY_LIMIT": "500"}, check: func(t *testing.T, c MetricsConfig) {
			if c.CardinalityLimit != 500 {
				t.Errorf("limit = %d", c.CardinalityLimit)
			}
		}},
		{name: "bad cardinality limit", env: map[string]string{"LOOMCYCLE_METRICS_CARDINALITY_LIMIT": "0"}, wantErr: "LOOMCYCLE_METRICS_CARDINALITY_LIMIT"},
		{name: "agent label", env: map[string]string{"LOOMCYCLE_METRICS_AGENT_LABEL": "1"}, check: func(t *testing.T, c MetricsConfig) {
			if !c.AgentLabel {
				t.Error("agent label off")
			}
		}},
		{name: "bad agent label", env: map[string]string{"LOOMCYCLE_METRICS_AGENT_LABEL": "sometimes"}, wantErr: "LOOMCYCLE_METRICS_AGENT_LABEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ResolveMetricsConfig(envOf(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			tc.check(t, c)
		})
	}
}

func TestNewMetricExporter_PicksTheProtocolsExporter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	for _, tc := range []struct {
		protocol Protocol
		check    func(sdkmetric.Exporter) bool
	}{
		{ProtocolHTTPProtobuf, func(e sdkmetric.Exporter) bool { _, ok := e.(*otlpmetrichttp.Exporter); return ok }},
		{ProtocolGRPC, func(e sdkmetric.Exporter) bool { _, ok := e.(*otlpmetricgrpc.Exporter); return ok }},
	} {
		exp, err := newMetricExporter(context.Background(), MetricsConfig{Export: true, Protocol: tc.protocol})
		if err != nil {
			t.Fatalf("%s: %v", tc.protocol, err)
		}
		le, ok := exp.(*loggingMetricExporter)
		if !ok || !tc.check(le.Exporter) {
			t.Errorf("%s: exporter = %T", tc.protocol, exp)
		}
		_ = exp.Shutdown(context.Background())
	}
}

type failingExporter struct{ sdkmetric.Exporter }

func (failingExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return errors.New("connection refused")
}

// A failing collector is one Warn, not one line per interval, and never an
// error handed to the global OTEL handler (whose messages name spans).
func TestLoggingMetricExporter_SummarisesFailures(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	now := time.Unix(0, 0)
	e := &loggingMetricExporter{Exporter: failingExporter{}, log: newMetricErrorLog(func() time.Time { return now })}
	for range 5 {
		if err := e.Export(context.Background(), &metricdata.ResourceMetrics{}); err != nil {
			t.Fatalf("Export returned %v; the reader would hand it to the global handler", err)
		}
	}
	if n := strings.Count(buf.String(), "metric export failing"); n != 1 {
		t.Errorf("failure lines = %d, want 1:\n%s", n, buf.String())
	}
}

// Confined with the shared "" tenant keeps the series labelled tenant="" and
// drops every other tenant's and every untenanted one.
func TestRenderPrometheus_ConfinedToTheSharedTenant(t *testing.T) {
	rm := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: MeterName}, Metrics: []metricdata.Metrics{
		{Name: "loomcycle.x", Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{
			{Attributes: attrs("tenant", ""), Value: 1},
			{Attributes: attrs("tenant", "beta"), Value: 2},
			{Attributes: attrs("db", "store"), Value: 3},
		}}},
	}}}}
	got := renderPrometheus(rm, PromOptions{Confined: true})
	if !strings.Contains(got, `loomcycle_x{tenant=""} 1`) || strings.Contains(got, "beta") || strings.Contains(got, "store") {
		t.Errorf("confined to \"\" =\n%s", got)
	}
}
