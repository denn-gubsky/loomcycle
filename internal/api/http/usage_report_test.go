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

	"github.com/denn-gubsky/loomcycle/internal/auth"
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

	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2}}
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

	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2}}
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

// TestUsageReport_GroupByPurposeSplitsChatOCRAndSTT — an operator asks what OCR
// and speech-to-text cost: group_by=purpose answers with one row per purpose,
// the STT row carrying the audio seconds it transcribed. Decoded raw so the
// wire keys themselves are asserted.
func TestUsageReport_GroupByPurposeSplitsChatOCRAndSTT(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := context.Background()
	for _, r := range []store.TokenUsageRow{
		{RunID: "r1", TenantID: "acme", Provider: "anthropic", Model: "m", CredentialSource: "operator",
			InputTokens: 100, Cost: 1.0, CostCurrency: "USD"},
		{RunID: "r1", TenantID: "acme", Provider: "anthropic", Model: "m", CredentialSource: "operator",
			InputTokens: 3000, Cost: 0.25, CostCurrency: "USD", Purpose: store.UsagePurposeOCR},
		{RunID: "r1", TenantID: "acme", Provider: "anthropic", Model: "m", CredentialSource: "operator",
			InputTokens: 1000, Cost: 0.25, CostCurrency: "USD", Purpose: store.UsagePurposeOCR},
		{RunID: "r1", TenantID: "acme", Provider: "whisper", Model: "w", CredentialSource: "operator",
			Cost: 0.5, CostCurrency: "USD", Purpose: store.UsagePurposeSTT, AudioSeconds: 90},
	} {
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/_usage?group_by=purpose")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("group_by=purpose status = %d: %s", resp.StatusCode, body)
	}
	var raw struct {
		GroupBy []string         `json:"group_by"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.GroupBy) != 1 || raw.GroupBy[0] != "purpose" {
		t.Errorf("group_by echo = %v, want [purpose]", raw.GroupBy)
	}
	type got struct {
		in, calls float64
		cost      float64
		seconds   any
	}
	byPurpose := map[string]got{}
	for _, r := range raw.Rows {
		p, _ := r["purpose"].(string)
		byPurpose[p] = got{r["input_tokens"].(float64), r["call_count"].(float64), r["cost"].(float64), r["audio_seconds"]}
	}
	if len(byPurpose) != 3 {
		t.Fatalf("rows by purpose = %+v, want chat, ocr and stt", byPurpose)
	}
	if g := byPurpose["chat"]; g.in != 100 || g.calls != 1 || g.cost != 1.0 || g.seconds != nil {
		t.Errorf("chat = %+v, want 100 tokens over 1 call, cost 1, no audio_seconds key", g)
	}
	if g := byPurpose["ocr"]; g.in != 4000 || g.calls != 2 || g.cost != 0.5 || g.seconds != nil {
		t.Errorf("ocr = %+v, want 4000 tokens over 2 calls, cost 0.5, no audio_seconds key", g)
	}
	if g := byPurpose["stt"]; g.in != 0 || g.calls != 1 || g.cost != 0.5 || g.seconds != 90.0 {
		t.Errorf("stt = %+v, want 0 tokens over 1 call, cost 0.5, audio_seconds 90", g)
	}
}

// usageReportServer serves /v1/_usage over a seeded ledger, with an admin
// bearer and a tenant operator of acme.
func usageReportServer(t *testing.T, rows []store.TokenUsageRow) (*httptest.Server, *Server) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "legacy-admin"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Env.UsageSweeperEnabled = true // older rows are archived, so detail_since is set
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	seedToken(t, srv.store, "tok-acme-op", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	for _, r := range rows {
		if err := srv.store.RecordCallUsage(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, srv
}

func decodeUsage(t *testing.T, ts *httptest.Server, bearer, path string) (int, usageReportResponse) {
	t.Helper()
	code, body := bearerCall(t, ts, bearer, "GET", path, "")
	var out usageReportResponse
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return code, out
}

// TestUsageReport_BucketsFiltersAndTopOverHTTP — the chart parameters reach
// the store: day buckets carry their period, a filter narrows, top folds the
// rest into "other", and a bad value is a 400.
func TestUsageReport_BucketsFiltersAndTopOverHTTP(t *testing.T) {
	now := time.Now().UTC()
	day := func(n int) time.Time {
		return now.Truncate(24 * time.Hour).Add(-time.Duration(n)*24*time.Hour + time.Hour)
	}
	ts, _ := usageReportServer(t, []store.TokenUsageRow{
		{RunID: "r1", TenantID: "acme", Provider: "p", Model: "big", CredentialSource: "operator", InputTokens: 100, Cost: 9, CostCurrency: "USD", TS: day(2), Tier: "high"},
		{RunID: "r2", TenantID: "acme", Provider: "p", Model: "mid", CredentialSource: "operator", InputTokens: 10, Cost: 3, CostCurrency: "USD", TS: day(1), Tier: "middle"},
		{RunID: "r3", TenantID: "acme", Provider: "p", Model: "small", CredentialSource: "operator", InputTokens: 1, Cost: 1, CostCurrency: "USD", TS: day(1), Purpose: store.UsagePurposeRerank},
	})

	code, rep := decodeUsage(t, ts, "legacy-admin", "/v1/_usage?group_by=model&bucket=day&top=1")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if rep.Bucket != "day" || len(rep.Rows) != 2 || rep.Rows[0].Model != "big" || rep.Rows[0].Period.IsZero() ||
		!rep.Rows[1].Other || rep.Rows[1].Cost != 4 {
		t.Errorf("day buckets, top 1 = %+v, want big on its day, then the other two folded into one day", rep)
	}
	if rep.DetailSince == "" {
		t.Error("detail_since missing with the sweeper on")
	}

	if _, rep = decodeUsage(t, ts, "legacy-admin", "/v1/_usage?group_by=tier&class=foreground"); len(rep.Rows) != 2 {
		t.Errorf("foreground by tier = %+v, want high and middle only", rep.Rows)
	}

	for _, bad := range []string{"bucket=fortnight", "top=nope", "top=99", "group_by=model&top_by=vibes", "class=sideways", "bucket=hour"} {
		if code, _ := decodeUsage(t, ts, "legacy-admin", "/v1/_usage?"+bad); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, code)
		}
	}
}

// TestUsageReport_TenantOperatorCannotFilterIntoOtherTenant — a tenant
// operator's report is confined to its own tenant whatever it filters on or
// names as tenant: a filter only narrows within acme.
func TestUsageReport_TenantOperatorCannotFilterIntoOtherTenant(t *testing.T) {
	ts, _ := usageReportServer(t, []store.TokenUsageRow{
		{RunID: "r1", TenantID: "acme", UserID: "ann", Provider: "p", Model: "shared", CredentialSource: "operator", InputTokens: 1, Tier: "middle"},
		{RunID: "r2", TenantID: "globex", UserID: "gus", Provider: "p", Model: "shared", CredentialSource: "operator", InputTokens: 100, Tier: "middle"},
		{RunID: "r3", TenantID: "globex", UserID: "gus", Provider: "p", Model: "secret", CredentialSource: "operator", InputTokens: 1000, Tier: "high"},
	})
	for _, path := range []string{
		"/v1/_usage?group_by=tenant,model&model=shared",
		"/v1/_usage?group_by=tenant,model&tenant=globex",
		"/v1/_usage?group_by=tenant,user&user=gus",
		"/v1/_usage?group_by=tenant,tier&tier=high",
		"/v1/_usage?group_by=model&model=secret&tenant=globex",
	} {
		code, rep := decodeUsage(t, ts, "tok-acme-op", path)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", path, code)
		}
		for _, r := range rep.Rows {
			if r.InputTokens > 1 || (r.TenantID != "" && r.TenantID != "acme") {
				t.Errorf("%s: row %+v reaches beyond acme", path, r)
			}
		}
	}
}

// TestUsageSpeed_PerModelAndConfinedToTheTenant — GET /v1/_usage/speed sums a
// model's timed calls into its percentiles; a tenant operator's figures come
// from its own tenant's calls only, whatever tenant it names.
func TestUsageSpeed_PerModelAndConfinedToTheTenant(t *testing.T) {
	now := time.Now().UTC()
	var rows []store.TokenUsageRow
	for i := 1; i <= 10; i++ {
		rows = append(rows, store.TokenUsageRow{RunID: "r", TenantID: "acme", Provider: "p", Model: "m", CredentialSource: "operator",
			OutputTokens: 200, DurationMs: int64(i * 100), TTFTMs: 50, DecodeMs: 1000, TS: now.Add(-time.Duration(i) * time.Minute)})
	}
	rows = append(rows, store.TokenUsageRow{RunID: "r", TenantID: "globex", Provider: "p", Model: "m", CredentialSource: "operator",
		OutputTokens: 1, DurationMs: 999999, DecodeMs: 999999, TS: now.Add(-time.Minute)})
	ts, _ := usageReportServer(t, rows)

	decode := func(bearer, path string) (int, usageSpeedResponse) {
		code, body := bearerCall(t, ts, bearer, "GET", path, "")
		var out usageSpeedResponse
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
		}
		return code, out
	}
	code, rep := decode("tok-acme-op", "/v1/_usage/speed?tenant=globex")
	if code != http.StatusOK || len(rep.Models) != 1 {
		t.Fatalf("status %d, models %+v", code, rep.Models)
	}
	if m := rep.Models[0]; m.Calls != 10 || m.DurationP50Ms != 500 || m.DurationP95Ms != 1000 || m.TTFTP50Ms != 50 || m.OutputTokensPS != 200 {
		t.Errorf("acme's speed = %+v, want 10 calls, p50 500, p95 1000, ttft 50, 200 tok/s (no globex call)", m)
	}
	if _, all := decode("legacy-admin", "/v1/_usage/speed"); len(all.Models) != 1 || all.Models[0].Calls != 11 {
		t.Errorf("admin's speed = %+v, want both tenants' 11 calls", all.Models)
	}
	if code, _ := decode("legacy-admin", "/v1/_usage/speed?class=sideways"); code != http.StatusBadRequest {
		t.Errorf("bad filter: status %d, want 400", code)
	}
}
