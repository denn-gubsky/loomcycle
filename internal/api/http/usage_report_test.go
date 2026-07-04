package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// GET /v1/_usage aggregates the ledger and validates group_by. Open mode = admin
// (sees all tenants); the tenant-confinement path is the shared
// principalTenantScope, covered by the events-audit tests.
func TestUsageReport_Endpoint(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usagerep.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	seed := []store.TokenUsageRow{
		{RunID: "r1", TenantID: "acme", Provider: "anthropic", Model: "m", CredentialSource: "operator", InputTokens: 100, Cost: 1.0, CostCurrency: "USD"},
		{RunID: "r2", TenantID: "acme", Provider: "anthropic", Model: "m", CredentialSource: "tenant", InputTokens: 200, Cost: 2.0, CostCurrency: "USD"},
		{RunID: "r3", TenantID: "globex", Provider: "openai", Model: "m", CredentialSource: "operator", InputTokens: 400, Cost: 4.0, CostCurrency: "USD"},
	}
	for _, r := range seed {
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2, QueueTimeoutMS: 500}}
	cfg.Env.AuthToken = "" // open mode → admin, sees all tenants
	srv := New(cfg, &stubResolver{p: &scriptedProvider{}}, []tools.Tool{}, concurrency.New(2, 2, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Group by tenant,source over all tenants.
	var got usageReportResponse
	resp, err := http.Get(ts.URL + "/v1/_usage?group_by=tenant,source")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()

	if len(got.Rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(got.Rows), got.Rows)
	}
	// Operator bill across tenants = 1.0 (acme) + 4.0 (globex) = 5.0.
	var opCost, acmeCost float64
	for _, a := range got.Rows {
		if a.CredentialSource == "operator" {
			opCost += a.Cost
		}
		if a.TenantID == "acme" {
			acmeCost += a.Cost
		}
	}
	if opCost != 5.0 {
		t.Errorf("operator bill = %v, want 5.0", opCost)
	}
	if acmeCost != 3.0 {
		t.Errorf("acme consumption = %v, want 3.0", acmeCost)
	}

	// An unknown group_by dimension is a 400 (whitelist guard).
	bad, err := http.Get(ts.URL + "/v1/_usage?group_by=tenant,bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Errorf("bad group_by status = %d, want 400", bad.StatusCode)
	}
}

// TestUsageReport_EmptyRowsIsArrayNotNull guards the Web-UI crash: a no-usage
// window must serialize `"rows":[]`, not `"rows":null` (a Go nil slice). The UI
// types rows as an array and does `resp.rows.length`; a null crashed the page to
// a blank overlay. Asserts the RAW body — decoding into the struct would coerce
// null→nil and hide the regression (len(nil)==0 either way).
func TestUsageReport_EmptyRowsIsArrayNotNull(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage_empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2, QueueTimeoutMS: 500}}
	cfg.Env.AuthToken = "" // open mode → admin
	srv := New(cfg, &stubResolver{p: &scriptedProvider{}}, []tools.Tool{}, concurrency.New(2, 2, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/_usage?group_by=tenant,source")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `"rows":null`) {
		t.Fatalf(`empty report serialized "rows":null (crashes the Web UI); want "rows":[]. body=%s`, body)
	}
	if !strings.Contains(string(body), `"rows":[]`) {
		t.Fatalf(`empty report must serialize "rows":[]; body=%s`, body)
	}
}
