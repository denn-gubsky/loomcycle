package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// observabilityDir is deploy/observability, the dashboards and alert rules
// shipped with the binary.
var observabilityDir = filepath.Join("..", "..", "..", "deploy", "observability")

var loomcycleMetricName = regexp.MustCompile(`\bloomcycle_[a-zA-Z0-9_]*[a-zA-Z0-9]`)

// emittedMetrics renders /metrics with every instrument holding data and
// returns each sample name it carries (a family name, and a histogram's
// _bucket, _sum and _count series) with the label names its samples have.
func emittedMetrics(t *testing.T) map[string]map[string]bool {
	t.Helper()
	t.Setenv("LOOMCYCLE_REPLICA_ID", "r1") // so the replica label is emitted, as in a cluster
	installMetrics(t)
	lcotel.ResetObservablesForTest()
	t.Cleanup(lcotel.ResetObservablesForTest)
	t.Cleanup(lcotel.SetUserIDPolicyForTest([]byte("0123456789abcdef0123456789abcdef"), false))
	ctx := context.Background()

	_, run := lcotel.RecordRunStart(ctx, lcotel.RunStartAttrs{Tenant: "t"})
	lcotel.SetRunDone(run, lcotel.RunDoneAttrs{})
	run.End()
	lcotel.RecordUsage(ctx, lcotel.UsageRecord{Tenant: "t", Provider: "p", Model: "m", Key: "operator",
		Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, CostUSD: 0.01, CostKnown: true, Chat: true})
	_, attempt := lcotel.RecordProviderCall(ctx, lcotel.ProviderCallAttrs{Provider: "p", Model: "m"})
	lcotel.SetProviderResponse(attempt, &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429"}, nil)
	attempt.End()
	_, call := lcotel.RecordModelCall(ctx, lcotel.ModelCallAttrs{Provider: "p", Model: "m"})
	lcotel.SetModelCallResult(call, lcotel.ModelCallResult{TTFT: time.Millisecond})
	call.End()
	lcotel.RecordProviderRetry(ctx, "p", "header")
	lcotel.RecordProviderFallback(ctx, "p", "m", "q", "n")
	lcotel.RecordToolCallMetric(ctx, "Read", lcotel.ToolOutcomeOK, time.Millisecond)
	lcotel.RegisterDBPool("store", func() lcotel.DBPoolStats { return lcotel.DBPoolStats{Open: 1, WaitCount: 1, WaitDuration: time.Second} })
	lcotel.RecordConsolidationWatermark(lcotel.ConsolidationBackendInProcess, time.Now().Add(-time.Minute))
	lcotel.SetConsolidationPending(lcotel.ConsolidationBackendInProcess, 1)
	lcotel.SetTokenBudgetSource(func() []lcotel.TokenBudget {
		return []lcotel.TokenBudget{{Tenant: "t", Period: "month", Used: 1, Limit: 2}}
	})

	srv := newMetricsPromServer(t, true)
	rel, err := srv.sem.AcquireForUser(t.Context(), "t", "u")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	out := map[string]map[string]bool{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series := line[:strings.LastIndexByte(line, ' ')]
		name, labels, _ := strings.Cut(series, "{")
		if out[name] == nil {
			out[name] = map[string]bool{}
		}
		for _, m := range promLabelNameRe.FindAllStringSubmatch(labels, -1) {
			out[name][m[1]] = true
		}
	}
	return out
}

var promLabelNameRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="`)

// dashboardQueries is every PromQL a Grafana dashboard holds: panel target
// exprs and templating queries, wherever they nest.
func dashboardQueries(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && (k == "expr" || k == "query" || k == "definition") {
					out = append(out, s)
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	return out
}

// ruleQueries is every alerting and recording rule's expr in a rules file.
func ruleQueries(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Expr string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []string
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			out = append(out, r.Expr)
		}
	}
	return out
}

// referencedMetrics maps each loomcycle_* name the shipped dashboards and
// rules query to the files that query it.
func referencedMetrics(t *testing.T) map[string][]string {
	t.Helper()
	refs := map[string][]string{}
	add := func(file string, queries []string) {
		if len(queries) == 0 {
			t.Errorf("%s holds no query: the walk is broken or the file is empty", file)
		}
		for _, q := range queries {
			for _, n := range loomcycleMetricName.FindAllString(q, -1) {
				refs[n] = append(refs[n], filepath.Base(file))
			}
		}
	}
	dashboards, _ := filepath.Glob(filepath.Join(observabilityDir, "grafana", "dashboards", "*.json"))
	rules, _ := filepath.Glob(filepath.Join(observabilityDir, "prometheus", "rules", "*.yml"))
	if len(dashboards) < 2 || len(rules) < 1 {
		t.Fatalf("found %d dashboards and %d rule files under %s; the paths are wrong", len(dashboards), len(rules), observabilityDir)
	}
	for _, f := range dashboards {
		add(f, dashboardQueries(t, f))
	}
	for _, f := range rules {
		add(f, ruleQueries(t, f))
	}
	return refs
}

// Every loomcycle_* metric a shipped dashboard or alert rule queries is one
// /metrics emits. A renamed or removed series would otherwise leave a panel
// empty and an alert that can never fire, with nothing failing.
func TestObservabilityAssets_EveryMetricIsEmitted(t *testing.T) {
	emitted := emittedMetrics(t)
	refs := referencedMetrics(t)
	if len(refs) < 20 {
		t.Fatalf("only %d metric names found in the shipped assets; the extraction is broken", len(refs))
	}
	var missing []string
	for name, files := range refs {
		if emitted[name] == nil {
			sort.Strings(files)
			missing = append(missing, name+" ("+strings.Join(files, ", ")+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("shipped dashboards or rules query metrics /metrics does not emit:\n  %s", strings.Join(missing, "\n  "))
	}
}

// targetLabels are the labels Prometheus adds to every series it scrapes.
var targetLabels = map[string]bool{"instance": true, "job": true}

// labelValues are the label values the code can emit, for the labels whose
// set is closed, by metric family prefix.
func labelValues() map[string]map[string]map[string]bool {
	set := func(vs ...string) map[string]bool {
		m := map[string]bool{}
		for _, v := range vs {
			m[v] = true
		}
		return m
	}
	return map[string]map[string]map[string]bool{
		"loomcycle_runs_total": {
			"status": set(string(store.RunCompleted), string(store.RunFailed), string(store.RunCancelled), string(store.RunRejected), lcotel.RunStatusUnknown),
			"trigger": set(lcotel.TriggerHTTP, lcotel.TriggerGRPC, lcotel.TriggerMCP, lcotel.TriggerSchedule,
				lcotel.TriggerWebhook, lcotel.TriggerA2A, lcotel.TriggerSubagent, lcotel.TriggerResume),
		},
		"loomcycle_provider_requests_total": {
			"outcome": set(lcotel.ProviderOutcomeOK, lcotel.ErrorTypeRateLimited, lcotel.ErrorTypeOverloaded, lcotel.ErrorTypeTimeout,
				lcotel.ErrorTypeNetwork, lcotel.ErrorTypeCancelled, lcotel.ErrorTypeClientError, lcotel.ErrorTypeServerError),
		},
		"loomcycle_tool_calls_total": {
			"outcome": set(lcotel.ToolOutcomeOK, lcotel.ToolOutcomeError, lcotel.ToolOutcomeIsError),
		},
	}
}

var (
	selectorRe    = regexp.MustCompile(`\b(loomcycle_[a-zA-Z0-9_]*[a-zA-Z0-9])\{([^}]*)\}`)
	matcherRe     = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*"((?:[^"\\]|\\.)*)"`)
	groupingRe    = regexp.MustCompile(`\b(by|without|on|ignoring)\s*\(([^)]*)\)`)
	labelValuesRe = regexp.MustCompile(`label_values\(\s*(loomcycle_[a-zA-Z0-9_]*[a-zA-Z0-9])\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)`)
)

// queryLabelProblems checks one PromQL query: every label it matches on a
// loomcycle_* selector, groups by (by, without, on, ignoring) or asks the
// values of, is one the metrics it applies to carry; and a literal value of
// a closed label is one the code emits. A grouping clause is checked
// against every loomcycle_* metric of the query, which holds for the
// shipped queries (each groups one metric, or joins two that share it).
func queryLabelProblems(q string, emitted map[string]map[string]bool) []string {
	var problems []string
	has := func(metric, label string) bool {
		return targetLabels[label] || emitted[metric][label]
	}
	values := labelValues()
	for _, sel := range selectorRe.FindAllStringSubmatch(q, -1) {
		metric := sel[1]
		for _, m := range matcherRe.FindAllStringSubmatch(sel[2], -1) {
			label, op, val := m[1], m[2], m[3]
			if !has(metric, label) {
				problems = append(problems, metric+" has no label "+label)
				continue
			}
			allowed := values[metric][label]
			if allowed == nil || (op != "=" && op != "=~") || strings.Contains(val, "$") {
				continue
			}
			for _, v := range strings.Split(val, "|") {
				if !allowed[v] {
					problems = append(problems, metric+"{"+label+"} is never "+v)
				}
			}
		}
	}
	metrics := loomcycleMetricName.FindAllString(q, -1)
	for _, g := range groupingRe.FindAllStringSubmatch(q, -1) {
		for _, label := range strings.Split(g[2], ",") {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			for _, metric := range metrics {
				if !has(metric, label) {
					problems = append(problems, metric+" has no label "+label+" ("+g[1]+")")
				}
			}
		}
	}
	for _, lv := range labelValuesRe.FindAllStringSubmatch(q, -1) {
		if !has(lv[1], lv[2]) {
			problems = append(problems, lv[1]+" has no label "+lv[2]+" (label_values)")
		}
	}
	return problems
}

// Every label a shipped dashboard or rule matches, groups by or lists the
// values of is one its metric carries, and a literal status, trigger or
// outcome is a value the code emits: a renamed label or value would
// otherwise leave a panel empty or an alert silent, as a renamed metric
// would.
func TestObservabilityAssets_EveryLabelIsEmitted(t *testing.T) {
	emitted := emittedMetrics(t)
	dashboards, _ := filepath.Glob(filepath.Join(observabilityDir, "grafana", "dashboards", "*.json"))
	rules, _ := filepath.Glob(filepath.Join(observabilityDir, "prometheus", "rules", "*.yml"))
	checked := 0
	for _, f := range dashboards {
		for _, q := range dashboardQueries(t, f) {
			checked++
			for _, p := range queryLabelProblems(q, emitted) {
				t.Errorf("%s: %s\n  in %s", filepath.Base(f), p, q)
			}
		}
	}
	for _, f := range rules {
		for _, q := range ruleQueries(t, f) {
			checked++
			for _, p := range queryLabelProblems(q, emitted) {
				t.Errorf("%s: %s\n  in %s", filepath.Base(f), p, q)
			}
		}
	}
	if checked < 30 {
		t.Fatalf("only %d queries checked; the walk is broken", checked)
	}
}

// The checker catches what it exists for: a missing label, a grouping
// label, an impossible value.
func TestQueryLabelProblems_FindsAWrongLabelOrValue(t *testing.T) {
	emitted := map[string]map[string]bool{"loomcycle_runs_total": {"tenant": true, "status": true}}
	for q, want := range map[string]int{
		`sum by (tenant) (rate(loomcycle_runs_total{status="failed"}[5m]))`:     0,
		`sum by (tenant) (rate(loomcycle_runs_total{statuz="failed"}[5m]))`:     1,
		`sum by (tenantz) (rate(loomcycle_runs_total[5m]))`:                     1,
		`sum(rate(loomcycle_runs_total{status="finished"}[5m]))`:                1,
		`sum by (instance) (rate(loomcycle_runs_total{tenant=~"$tenant"}[5m]))`: 0,
		`label_values(loomcycle_runs_total, model)`:                             1,
	} {
		if got := queryLabelProblems(q, emitted); len(got) != want {
			t.Errorf("%s: problems %v, want %d", q, got, want)
		}
	}
}
