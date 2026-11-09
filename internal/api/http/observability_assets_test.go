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
)

// observabilityDir is deploy/observability, the dashboards and alert rules
// shipped with the binary.
var observabilityDir = filepath.Join("..", "..", "..", "deploy", "observability")

var loomcycleMetricName = regexp.MustCompile(`\bloomcycle_[a-zA-Z0-9_]*[a-zA-Z0-9]`)

// emittedMetricNames renders /metrics with every instrument holding data and
// returns the sample names it carries: each family name, and a histogram's
// _bucket, _sum and _count series.
func emittedMetricNames(t *testing.T) map[string]bool {
	t.Helper()
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
	lcotel.RecordProviderFallback(ctx, "p/m", "q/n")
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

	names := map[string]bool{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != "#" || fields[1] != "TYPE" {
			continue
		}
		name := fields[2]
		names[name] = true
		if fields[3] == "histogram" {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				names[name+suffix] = true
			}
		}
	}
	return names
}

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
	emitted := emittedMetricNames(t)
	refs := referencedMetrics(t)
	if len(refs) < 20 {
		t.Fatalf("only %d metric names found in the shipped assets; the extraction is broken", len(refs))
	}
	var missing []string
	for name, files := range refs {
		if !emitted[name] {
			sort.Strings(files)
			missing = append(missing, name+" ("+strings.Join(files, ", ")+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("shipped dashboards or rules query metrics /metrics does not emit:\n  %s", strings.Join(missing, "\n  "))
	}
}
