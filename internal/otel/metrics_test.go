package otel_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// withMetrics installs a fresh meter state for t and returns a function that
// renders /metrics' instrument section.
func withMetrics(t *testing.T, agentLabel bool, limit int, extra ...sdkmetric.Reader) func(lcotel.PromOptions) string {
	t.Helper()
	restore, err := lcotel.SetMetricsForTest(agentLabel, limit, extra...)
	if err != nil {
		t.Fatalf("SetMetricsForTest: %v", err)
	}
	t.Cleanup(restore)
	t.Cleanup(lcotel.ResetObservablesForTest)
	return func(opts lcotel.PromOptions) string {
		t.Helper()
		var b bytes.Buffer
		if err := lcotel.WritePrometheus(context.Background(), &b, opts); err != nil {
			t.Fatalf("WritePrometheus: %v", err)
		}
		return b.String()
	}
}

var promLabelRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// promValue sums the samples of name whose labels include every pair in
// match. ok is false when no sample matched.
func promValue(text, name string, match map[string]string) (float64, bool) {
	var sum float64
	found := false
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		series, val := line[:sp], line[sp+1:]
		n, labels := series, ""
		if i := strings.IndexByte(series, '{'); i >= 0 {
			n, labels = series[:i], series[i:]
		}
		if n != name {
			continue
		}
		got := map[string]string{}
		for _, m := range promLabelRe.FindAllStringSubmatch(labels, -1) {
			got[m[1]] = m[2]
		}
		ok := true
		for k, v := range match {
			if got[k] != v {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		sum += f
		found = true
	}
	return sum, found
}

func mustValue(t *testing.T, text, name string, match map[string]string, want float64) {
	t.Helper()
	got, ok := promValue(text, name, match)
	if !ok {
		t.Fatalf("no %s%v sample in:\n%s", name, match, text)
	}
	if got != want {
		t.Errorf("%s%v = %v, want %v", name, match, got, want)
	}
}

// A run is counted whether or not its span is sampled: the counters are the
// in-process truth a sampled trace stream cannot give.
func TestRunMetrics_CountedWithTracingSampledOff(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	t.Cleanup(lcotel.SetTracerProviderForTest(tp))
	render := withMetrics(t, false, 0)

	for i, status := range []string{"completed", "failed", "completed"} {
		ctx := lcotel.WithRunTrigger(context.Background(), lcotel.TriggerSchedule)
		_, span := lcotel.RecordRunStart(ctx, lcotel.RunStartAttrs{RunID: "r" + strconv.Itoa(i), Tenant: "acme"})
		if span.IsRecording() {
			t.Fatal("the fixture is wrong: the span records")
		}
		if i == 0 {
			mustValue(t, render(lcotel.PromOptions{}), "loomcycle_runs_active", map[string]string{"tenant": "acme"}, 1)
		}
		var err error
		if status == "failed" {
			err = errors.New("boom")
		}
		lcotel.SetRunDone(span, lcotel.RunDoneAttrs{Err: err})
		span.End()
		span.End() // a second End never counts the run twice
	}
	out := render(lcotel.PromOptions{})
	mustValue(t, out, "loomcycle_runs_total", map[string]string{"tenant": "acme", "status": "completed", "trigger": "schedule"}, 2)
	mustValue(t, out, "loomcycle_runs_total", map[string]string{"tenant": "acme", "status": "failed", "trigger": "schedule"}, 1)
	mustValue(t, out, "loomcycle_runs_active", map[string]string{"tenant": "acme"}, 0)
	mustValue(t, out, "loomcycle_run_duration_seconds_count", map[string]string{"tenant": "acme"}, 3)
}

// The status SetRunDone is given wins over the one derived from Err; a run
// whose span ended without SetRunDone is "unknown", not a completion; the
// trigger falls back to http.
func TestRunMetrics_StatusAndTriggerResolution(t *testing.T) {
	render := withMetrics(t, false, 0)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{Tenant: "t1", Trigger: lcotel.TriggerSubagent})
	lcotel.SetRunDone(span, lcotel.RunDoneAttrs{Status: "rejected"})
	span.End()
	_, span = lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{Tenant: "t1"})
	span.End()
	out := render(lcotel.PromOptions{})
	mustValue(t, out, "loomcycle_runs_total", map[string]string{"status": "rejected", "trigger": "subagent"}, 1)
	mustValue(t, out, "loomcycle_runs_total", map[string]string{"status": lcotel.RunStatusUnknown, "trigger": "http"}, 1)
}

// The agent label is the operator's opt-in; without it no series names one.
func TestRunMetrics_AgentLabelOnlyWhenOptedIn(t *testing.T) {
	for _, on := range []bool{false, true} {
		render := withMetrics(t, on, 0)
		_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{Tenant: "t", AgentName: "researcher"})
		lcotel.SetRunDone(span, lcotel.RunDoneAttrs{})
		span.End()
		// The ledger write names the agent through ctx, not the record.
		lcotel.RecordUsage(lcotel.WithAgentName(context.Background(), "writer"), lcotel.UsageRecord{Tenant: "t", Provider: "p", Model: "m", Key: "operator", Input: 3})
		out := render(lcotel.PromOptions{})
		if got := strings.Contains(out, `agent="researcher"`); got != on {
			t.Errorf("run agent label present = %v with LOOMCYCLE_METRICS_AGENT_LABEL %v", got, on)
		}
		if got := strings.Contains(out, `agent="writer"`); got != on {
			t.Errorf("token agent label present = %v with LOOMCYCLE_METRICS_AGENT_LABEL %v", got, on)
		}
	}
}

func TestRecordUsage_CountsEachTokenTypeAndUSDCost(t *testing.T) {
	render := withMetrics(t, false, 0)
	ctx := context.Background()
	lcotel.RecordUsage(ctx, lcotel.UsageRecord{Tenant: "acme", Provider: "anthropic", Model: "m1", Key: "tenant",
		Input: 100, Output: 20, CacheRead: 7, CacheWrite: 3, CostUSD: 0.25, CostKnown: true})
	lcotel.RecordUsage(ctx, lcotel.UsageRecord{Tenant: "acme", Provider: "anthropic", Model: "m1", Key: "tenant",
		Input: 1, CostUSD: 9, CostKnown: false}) // priced in another currency
	out := render(lcotel.PromOptions{})
	base := map[string]string{"tenant": "acme", "provider": "anthropic", "model": "m1", "key": "tenant"}
	for typ, want := range map[string]float64{"input": 101, "output": 20, "cache_read": 7, "cache_write": 3} {
		m := map[string]string{"type": typ}
		for k, v := range base {
			m[k] = v
		}
		mustValue(t, out, "loomcycle_tokens_total", m, want)
	}
	mustValue(t, out, "loomcycle_cost_usd_total", map[string]string{"tenant": "acme"}, 0.25)
}

// A provider attempt's outcome label and its span's error.type come from one
// classification, so a 429 on a dashboard is a 429 in the trace.
func TestProviderAttemptMetric_OutcomeAgreesWithSpanErrorType(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
	}{
		{"ok", 200, nil},
		{"rate limited", 429, nil},
		{"overloaded", 529, nil},
		{"unavailable", 503, nil},
		{"request timeout", 408, nil},
		{"client error", 400, nil},
		{"server error", 500, nil},
		{"cancelled", 0, context.Canceled},
		{"deadline", 0, context.DeadlineExceeded},
		{"net timeout", 0, timeoutErr{}},
		{"network", 0, errors.New("dial tcp: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := withInMemoryExporter(t)
			render := withMetrics(t, false, 0)
			_, span := lcotel.RecordProviderCall(context.Background(), lcotel.ProviderCallAttrs{Provider: "anthropic", Model: "m"})
			var resp *http.Response
			if tc.err == nil {
				resp = &http.Response{StatusCode: tc.status, Status: http.StatusText(tc.status)}
			}
			lcotel.SetProviderResponse(span, resp, tc.err)
			span.End()
			et, _ := attrOf(exp.GetSpans()[0], "error.type")
			want := et.AsString()
			if want == "" {
				want = lcotel.ProviderOutcomeOK
			}
			out := render(lcotel.PromOptions{})
			mustValue(t, out, "loomcycle_provider_requests_total", map[string]string{"provider": "anthropic", "model": "m", "outcome": want}, 1)
			if n, _ := promValue(out, "loomcycle_provider_requests_total", nil); n != 1 {
				t.Errorf("attempts counted = %v, want 1", n)
			}
			mustValue(t, out, "loomcycle_provider_request_duration_seconds_count", map[string]string{"provider": "anthropic"}, 1)
		})
	}
}

func TestModelCallMetric_RecordsDurationAndTTFTOnce(t *testing.T) {
	render := withMetrics(t, false, 0)
	_, span := lcotel.RecordModelCall(context.Background(), lcotel.ModelCallAttrs{Provider: "openai", Model: "gpt", Purpose: "turn"})
	lcotel.SetModelCallResult(span, lcotel.ModelCallResult{TTFT: 120 * time.Millisecond})
	lcotel.SetModelCallResult(span, lcotel.ModelCallResult{TTFT: time.Second})
	span.End()
	_, span = lcotel.RecordModelCall(context.Background(), lcotel.ModelCallAttrs{Provider: "openai", Model: "gpt", Purpose: "gateway"})
	lcotel.SetModelCallResult(span, lcotel.ModelCallResult{Cancelled: true}) // nothing streamed
	span.End()
	out := render(lcotel.PromOptions{})
	mustValue(t, out, "loomcycle_model_call_duration_seconds_count", map[string]string{"provider": "openai", "model": "gpt"}, 2)
	mustValue(t, out, "loomcycle_model_ttft_seconds_count", map[string]string{"provider": "openai", "model": "gpt"}, 1)
	mustValue(t, out, "loomcycle_model_ttft_seconds_sum", nil, 0.12)
	mustValue(t, out, "loomcycle_model_call_duration_seconds_count", map[string]string{"purpose": "turn"}, 1)
	mustValue(t, out, "loomcycle_model_call_duration_seconds_count", map[string]string{"purpose": "gateway"}, 1)
}

func TestToolMetricName_CollapsesUnboundedNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		known bool
		want  string
	}{
		{"Read", true, "Read"},
		{"mcp__github__create_issue", true, "mcp:github"},
		{"client__open_file", true, "client"},
		{"Read", false, "unknown"},
		{"mcp__made_up__x", false, "unknown"},
	} {
		if got := lcotel.ToolMetricName(tc.name, tc.known); got != tc.want {
			t.Errorf("ToolMetricName(%q, %v) = %q, want %q", tc.name, tc.known, got, tc.want)
		}
	}
}

func TestRecordToolCallMetric_CountsByOutcome(t *testing.T) {
	render := withMetrics(t, false, 0)
	ctx := context.Background()
	lcotel.RecordToolCallMetric(ctx, "Read", lcotel.ToolOutcomeOK, time.Millisecond)
	lcotel.RecordToolCallMetric(ctx, "Read", lcotel.ToolOutcomeIsError, time.Millisecond)
	lcotel.RecordToolCallMetric(ctx, "mcp:github", lcotel.ToolOutcomeError, time.Millisecond)
	out := render(lcotel.PromOptions{})
	mustValue(t, out, "loomcycle_tool_calls_total", map[string]string{"tool": "Read", "outcome": "ok"}, 1)
	mustValue(t, out, "loomcycle_tool_calls_total", map[string]string{"tool": "Read", "outcome": "is_error"}, 1)
	mustValue(t, out, "loomcycle_tool_calls_total", map[string]string{"tool": "mcp:github", "outcome": "error"}, 1)
	mustValue(t, out, "loomcycle_tool_call_duration_seconds_count", map[string]string{"tool": "Read"}, 2)
}

func TestProviderRetryAndFallbackMetrics(t *testing.T) {
	render := withMetrics(t, false, 0)
	lcotel.RecordProviderRetry(context.Background(), "anthropic", "header")
	lcotel.RecordProviderFallback(context.Background(), "anthropic", "claude", "openai", "gpt")
	out := render(lcotel.PromOptions{})
	mustValue(t, out, "loomcycle_provider_retries_total", map[string]string{"provider": "anthropic", "reason": "header"}, 1)
	mustValue(t, out, "loomcycle_provider_fallbacks_total", map[string]string{"from": "anthropic/claude", "to": "openai/gpt"}, 1)
}

// The observable gauges read what their sources hold at collection; a
// consolidation lag grows with wall time until the next pass reports.
func TestObservableMetrics_PoolsConsolidationAndBudgets(t *testing.T) {
	render := withMetrics(t, false, 0)
	unregister := lcotel.RegisterDBPool("store", func() lcotel.DBPoolStats {
		return lcotel.DBPoolStats{Open: 5, InUse: 2, Idle: 3, WaitCount: 4, WaitDuration: 1500 * time.Millisecond}
	})
	lcotel.RecordConsolidationWatermark(lcotel.ConsolidationBackendInProcess, time.Now().Add(-time.Hour))
	lcotel.SetConsolidationPending(lcotel.ConsolidationBackendInProcess, 7)
	lcotel.SetTokenBudgetSource(func() []lcotel.TokenBudget {
		return []lcotel.TokenBudget{{Tenant: "acme", Period: "month", Used: 90, Limit: 100}, {Tenant: "nolimit", Period: "month", Used: 5}}
	})
	out := render(lcotel.PromOptions{})
	db := map[string]string{"db": "store"}
	mustValue(t, out, "loomcycle_db_pool_open", db, 5)
	mustValue(t, out, "loomcycle_db_pool_in_use", db, 2)
	mustValue(t, out, "loomcycle_db_pool_idle", db, 3)
	mustValue(t, out, "loomcycle_db_pool_wait_total", db, 4)
	mustValue(t, out, "loomcycle_db_pool_wait_duration_seconds_total", db, 1.5)
	mustValue(t, out, "loomcycle_memory_consolidation_pending", map[string]string{"backend": "inprocess"}, 7)
	if lag, _ := promValue(out, "loomcycle_memory_consolidation_lag_seconds", map[string]string{"backend": "inprocess"}); lag < 3599 || lag > 3700 {
		t.Errorf("consolidation lag = %v, want about an hour", lag)
	}
	mustValue(t, out, "loomcycle_token_budget_used_ratio", map[string]string{"tenant": "acme", "period": "month"}, 0.9)
	if strings.Contains(out, `tenant="nolimit"`) {
		t.Error("a tenant with no budget has a budget series")
	}
	unregister()
	if strings.Contains(render(lcotel.PromOptions{}), "loomcycle_db_pool_open") {
		t.Error("an unregistered pool is still reported")
	}
}

// Past the cardinality limit new label sets fold into the SDK's overflow
// series instead of growing the series set without bound.
func TestMetrics_OverflowSeriesPastTheCardinalityLimit(t *testing.T) {
	render := withMetrics(t, false, 3)
	for i := range 10 {
		lcotel.RecordUsage(context.Background(), lcotel.UsageRecord{Tenant: "t" + strconv.Itoa(i), Provider: "p", Model: "m", Key: "operator", Input: 1})
	}
	out := render(lcotel.PromOptions{})
	n, ok := promValue(out, "loomcycle_tokens_total", map[string]string{"otel_metric_overflow": "true"})
	if !ok || n == 0 {
		t.Fatalf("no overflow series past the limit:\n%s", out)
	}
	if total, _ := promValue(out, "loomcycle_tokens_total", nil); total != 10 {
		t.Errorf("tokens across all series = %v, want 10 (nothing is dropped, only folded)", total)
	}
	// A tenant reader never gets the overflow series: it mixes tenants.
	if strings.Contains(render(lcotel.PromOptions{Tenant: "t9"}), "otel_metric_overflow") {
		t.Error("the tenant view carries the overflow series")
	}
}

// The GenAI convention histograms reach an OTLP reader and never /metrics.
func TestGenAIHistograms_OnTheOTLPPathOnly(t *testing.T) {
	otlp := sdkmetric.NewManualReader()
	render := withMetrics(t, false, 0, otlp)
	lcotel.RecordUsage(context.Background(), lcotel.UsageRecord{Tenant: "t", Provider: "anthropic", Model: "m", Key: "operator", Input: 10, Output: 2, Chat: true})
	_, span := lcotel.RecordModelCall(context.Background(), lcotel.ModelCallAttrs{Provider: "anthropic", Model: "m"})
	lcotel.SetModelCallResult(span, lcotel.ModelCallResult{})
	span.End()

	var rm metricdata.ResourceMetrics
	if err := otlp.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	for _, want := range []string{"gen_ai.client.token.usage", "gen_ai.client.operation.duration", "loomcycle.tokens"} {
		if !names[want] {
			t.Errorf("the OTLP reader has no %s (has %v)", want, names)
		}
	}
	if out := render(lcotel.PromOptions{}); strings.Contains(out, "gen_ai") {
		t.Errorf("/metrics renders a GenAI convention series:\n%s", out)
	}
}

// No series names a run, a user or a session, whatever the recording sites
// are handed.
func TestMetrics_NoRunUserOrSessionLabel(t *testing.T) {
	render := withMetrics(t, true, 0)
	ctx := context.Background()
	_, span := lcotel.RecordRunStart(ctx, lcotel.RunStartAttrs{RunID: "run-1", UserID: "alice", SessionID: "sess-1", Tenant: "t", AgentName: "a", ParentAgentID: "p"})
	lcotel.SetRunDone(span, lcotel.RunDoneAttrs{})
	span.End()
	lcotel.RecordUsage(ctx, lcotel.UsageRecord{Tenant: "t", Provider: "p", Model: "m", Key: "user", Input: 1, Chat: true})
	_, ps := lcotel.RecordProviderCall(ctx, lcotel.ProviderCallAttrs{Provider: "p", Model: "m"})
	lcotel.SetProviderResponse(ps, &http.Response{StatusCode: 200}, nil)
	ps.End()
	lcotel.RecordToolCallMetric(ctx, "Read", lcotel.ToolOutcomeOK, time.Millisecond)
	out := render(lcotel.PromOptions{})
	for _, leak := range []string{"user_id", "run_id", "session", "alice", "run-1", "sess-1"} {
		if strings.Contains(out, leak) {
			t.Errorf("rendered metrics contain %q:\n%s", leak, out)
		}
	}
}

// A user on /metrics is the user_hash its spans carry; with no secret key
// there is none to give.
func TestMetricUserHash_IsTheSpanHashAndNeedsAKey(t *testing.T) {
	t.Cleanup(lcotel.SetUserIDPolicyForTest(testUserKey, false))
	got, ok := lcotel.MetricUserHash("acme", "alice")
	hash, _ := runSpanUserAttrs(t, "acme", "alice")
	if !ok || got != hash {
		t.Errorf("MetricUserHash = %q, %v; the span's loomcycle.user_hash is %q", got, ok, hash)
	}
	lcotel.SetUserIDPolicyForTest(nil, true)
	if got, ok := lcotel.MetricUserHash("acme", "alice"); ok || got != "" {
		t.Errorf("with no key MetricUserHash = %q, %v; want none", got, ok)
	}
}

// A no-trace run's model call has a content-free span, but it is still a
// call: the metrics count it, since they carry no content either.
func TestModelCallMetric_CountsANoTraceCall(t *testing.T) {
	render := withMetrics(t, false, 0)
	for _, noTrace := range []bool{false, true} {
		_, span := lcotel.RecordModelCall(context.Background(), lcotel.ModelCallAttrs{Provider: "nt-p", Model: "nt-m", Purpose: "turn", NoTrace: noTrace})
		lcotel.SetModelCallResult(span, lcotel.ModelCallResult{TTFT: time.Millisecond})
		span.End()
	}
	mustValue(t, render(lcotel.PromOptions{}), "loomcycle_model_call_duration_seconds_count", map[string]string{"provider": "nt-p"}, 2)
}

// A model the operator did not configure is "other" on every instrument
// that labels a model, so a caller cannot grow the series set with
// invented names; a configured model, or one matching a configured
// model_pattern, keeps its name. The set is rebuilt when the config is.
func TestMetrics_ModelLabelBoundedToConfiguredModels(t *testing.T) {
	render := withMetrics(t, false, 0)
	version := "v1"
	t.Cleanup(lcotel.SetModelLabelSource(func() any { return version }, func(v any) ([]string, []string) {
		if v == "v1" {
			return []string{"m-configured"}, []string{"claude-haiku-*"}
		}
		return []string{"m-configured", "m-added"}, nil
	}))
	ctx := context.Background()
	for _, m := range []string{"m-configured", "claude-haiku-4-5", "random-1", "random-2", "m-added"} {
		lcotel.RecordUsage(ctx, lcotel.UsageRecord{Tenant: "t", Provider: "p", Model: m, Key: "operator", Input: 1})
		_, ps := lcotel.RecordProviderCall(ctx, lcotel.ProviderCallAttrs{Provider: "p", Model: m})
		lcotel.SetProviderResponse(ps, &http.Response{StatusCode: 200}, nil)
		ps.End()
		_, ms := lcotel.RecordModelCall(ctx, lcotel.ModelCallAttrs{Provider: "p", Model: m, Purpose: "gateway"})
		lcotel.SetModelCallResult(ms, lcotel.ModelCallResult{})
		ms.End()
	}
	lcotel.RecordProviderFallback(ctx, "p", "random-3", "p", "m-configured")
	out := render(lcotel.PromOptions{})
	for _, leak := range []string{"random-", `"m-added"`} {
		if strings.Contains(out, leak) {
			t.Errorf("an unconfigured model became a label (%q):\n%s", leak, out)
		}
	}
	for _, name := range []string{"loomcycle_tokens_total", "loomcycle_provider_requests_total", "loomcycle_model_call_duration_seconds_count"} {
		mustValue(t, out, name, map[string]string{"model": "m-configured"}, 1)
		mustValue(t, out, name, map[string]string{"model": "claude-haiku-4-5"}, 1)
		mustValue(t, out, name, map[string]string{"model": lcotel.ModelLabelOther}, 3)
	}
	mustValue(t, out, "loomcycle_provider_fallbacks_total", map[string]string{"from": "p/other", "to": "p/m-configured"}, 1)

	version = "v2" // a config reload added m-added
	lcotel.RecordUsage(ctx, lcotel.UsageRecord{Tenant: "t", Provider: "p", Model: "m-added", Key: "operator", Input: 1})
	mustValue(t, render(lcotel.PromOptions{}), "loomcycle_tokens_total", map[string]string{"model": "m-added"}, 1)
}
