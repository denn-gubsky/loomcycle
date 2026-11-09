package http

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// recordTwoTenantsRuns ends one run for each of two tenants in the metrics
// installMetrics set up.
func recordTwoTenantsRuns() {
	for _, tenant := range []string{"acme", "beta"} {
		_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{Tenant: tenant})
		lcotel.SetRunDone(span, lcotel.RunDoneAttrs{})
		span.End()
		lcotel.RecordUsage(context.Background(), lcotel.UsageRecord{Tenant: tenant, Provider: "p", Model: "m", Key: "operator", Input: 5})
	}
}

// /metrics appends the instruments to the substrate series, carrying the same
// replica label, so one scrape config reads both.
func TestMetricsProm_AppendsTheInstrumentsWithTheReplicaLabel(t *testing.T) {
	t.Setenv("LOOMCYCLE_REPLICA_ID", "r-7")
	installMetrics(t)
	recordTwoTenantsRuns()
	rec := httptest.NewRecorder()
	newMetricsPromServer(t, false).handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	want := `loomcycle_runs_total{replica_id="r-7",status="completed",tenant="acme",trigger="http"} 1`
	i, j := strings.Index(body, "loomcycle_build_info"), strings.Index(body, want)
	if j < 0 {
		t.Fatalf("no %q in:\n%s", want, body)
	}
	if i < 0 || j < i {
		t.Errorf("the instruments are not appended after the substrate series:\n%s", body)
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("body does not end in a newline")
	}
}

// A tenant-confined reader gets its own tenant's series and nothing else: no
// other tenant's, no substrate series, no untenanted instrument.
func TestMetricsProm_TenantViewIsConfinedToItsTenant(t *testing.T) {
	installMetrics(t)
	recordTwoTenantsRuns()
	t.Cleanup(lcotel.RegisterDBPool("store", func() lcotel.DBPoolStats { return lcotel.DBPoolStats{Open: 1} }))
	lcotel.RecordProviderRetry(context.Background(), "p", "header")
	srv := newMetricsPromServer(t, true)
	rel, err := srv.sem.AcquireForUser(t.Context(), "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	full := httptest.NewRecorder()
	srv.renderMetricsProm(full, promView{})
	for _, present := range []string{"loomcycle_process_goroutines", "loomcycle_db_pool_open", "loomcycle_provider_retries_total", `tenant="beta"`} {
		if !strings.Contains(full.Body.String(), present) {
			t.Fatalf("the fixture is wrong: the operator view lacks %q", present)
		}
	}

	rec := httptest.NewRecorder()
	srv.renderMetricsProm(rec, promView{tenant: "acme"})
	body := rec.Body.String()
	for _, absent := range []string{
		`tenant="beta"`, "loomcycle_process_", "loomcycle_concurrency_", "loomcycle_build_info",
		"loomcycle_otel_spans", "loomcycle_db_pool", "loomcycle_provider_retries_total", "loomcycle_acl_", "loomcycle_tool_panics",
		"user_hash", "user_id",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("tenant view contains %q:\n%s", absent, body)
		}
	}
	for _, present := range []string{
		`loomcycle_runs_total{status="completed",tenant="acme",trigger="http"} 1`,
		`loomcycle_tokens_total{key="operator",model="m",provider="p",tenant="acme",type="input"} 5`,
	} {
		if !strings.Contains(body, present) {
			t.Errorf("tenant view lacks %q:\n%s", present, body)
		}
	}
}

// The unauthenticated view keeps every series but those naming a user.
func TestMetricsProm_OmitUserSeriesViewDropsThePerUserSeries(t *testing.T) {
	t.Cleanup(lcotel.SetUserIDPolicyForTest([]byte("0123456789abcdef0123456789abcdef"), false))
	srv := newMetricsPromServer(t, true)
	rel, err := srv.sem.AcquireForUser(t.Context(), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	full, open := httptest.NewRecorder(), httptest.NewRecorder()
	srv.renderMetricsProm(full, promView{})
	srv.renderMetricsProm(open, promView{omitUserSeries: true})
	if !strings.Contains(full.Body.String(), "loomcycle_concurrency_per_user") {
		t.Fatalf("the fixture is wrong: no per-user series in the full view:\n%s", full.Body.String())
	}
	if strings.Contains(open.Body.String(), "loomcycle_concurrency_per_user") || !strings.Contains(open.Body.String(), "loomcycle_concurrency_active") {
		t.Errorf("omit-user view:\n%s", open.Body.String())
	}
}

// After a real run through the server, no series names a run, a user or a
// session.
func TestMetricsProm_NoRunUserOrSessionLabelAfterARun(t *testing.T) {
	render := installMetrics(t)
	ts, _ := subAgentFixture(t)
	postParentRun(t, ts)
	waitForSample(t, render, `loomcycle_runs_active{tenant=""}`, 0)
	rec := httptest.NewRecorder()
	newMetricsPromServer(t, false).handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "loomcycle_tokens_total") {
		t.Fatalf("the fixture is wrong: no instrument series:\n%s", body)
	}
	for _, leak := range []string{"user_id", "run_id", "session"} {
		if strings.Contains(body, leak) {
			t.Errorf("/metrics contains %q:\n%s", leak, body)
		}
	}
}
