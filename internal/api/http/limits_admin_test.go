package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func i64p(v int64) *int64 { return &v }

// seedOperatorUsage records a token_usage row so the operator month-to-date
// counter is non-zero after SeedLimits.
func seedOperatorUsage(t *testing.T, srv *Server, inputTokens int) {
	t.Helper()
	if err := srv.store.RecordCallUsage(context.Background(), store.TokenUsageRow{
		RunID: "seed_run", TenantID: "", UserID: "seed", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: inputTokens, TS: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

// completingProvider is a stub that finishes a run in one iteration (text +
// done), so a POST /v1/runs returns after the run completes and events persist.
func completingProvider() *scriptedProvider {
	return &scriptedProvider{scripts: [][]providers.Event{{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}}
}

// TestLimits_HardLimitRefusesRun is the RFC AW admission regression: an operator
// hard ceiling below the month-to-date usage refuses a new POST /v1/runs with
// 429 + code:"token_limit_exceeded", and NO run is started. Fails before the
// admission Check is wired (the run would 200).
func TestLimits_HardLimitRefusesRun(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ctx := context.Background()
	seedOperatorUsage(t, srv, 1000)
	if err := srv.store.TokenLimitPut(ctx, store.TokenLimitRow{Scope: "operator", HardLimit: i64p(500), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedLimits(ctx); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("hard limit must refuse with 429; got %d\nbody: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "token_limit_exceeded") {
		t.Fatalf("body missing token_limit_exceeded: %s", body)
	}
}

// TestLimits_SoftLimitEmitsEventInTranscript is the RFC AW soft-crossing
// regression: with the operator soft ceiling below usage, the run STARTS (200)
// and a `limit` event (severity soft) is persisted in the transcript. Fails
// before the soft-at-admission emit is wired (no limit event lands).
func TestLimits_SoftLimitEmitsEventInTranscript(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ctx := context.Background()
	seedOperatorUsage(t, srv, 1000)
	if err := srv.store.TokenLimitPut(ctx, store.TokenLimitRow{Scope: "operator", SoftLimit: i64p(500), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedLimits(ctx); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("soft limit must allow the run; got %d\nbody: %s", resp.StatusCode, body)
	}

	events, _, err := srv.store.ListEvents(ctx, store.EventFilter{Type: "limit"}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatalf("expected a persisted `limit` event in the transcript; found none")
	}
	var ev providers.Event
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("decode limit event: %v", err)
	}
	if ev.Type != providers.EventLimit || ev.Limit == nil {
		t.Fatalf("event is not a populated limit event: %+v", ev)
	}
	if ev.Limit.Severity != "soft" || ev.Limit.Scope != "operator" {
		t.Fatalf("limit event payload wrong: %+v", ev.Limit)
	}
}

// TestLimits_PutGetDeleteRoundTrip exercises the admin surface as an admin
// principal: PUT a tenant budget, GET it back (with live usage), DELETE it.
func TestLimits_PutGetDeleteRoundTrip(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	adminCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin}})

	// PUT a tenant budget for acme.
	putBody, _ := json.Marshal(limitPutRequest{
		TenantID: strPtr("acme"), Scope: "tenant", SoftLimit: i64p(100), HardLimit: i64p(200),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/_limits", bytes.NewReader(putBody)).WithContext(adminCtx)
	srv.handleLimitPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// GET the list — the acme tenant row must be present with the tiers.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/_limits", nil).WithContext(adminCtx)
	srv.handleLimitsList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var list limitsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range list.Limits {
		if row.TenantID == "acme" && row.Scope == "tenant" {
			found = true
			if row.SoftLimit == nil || *row.SoftLimit != 100 || row.HardLimit == nil || *row.HardLimit != 200 {
				t.Fatalf("acme row tiers wrong: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("acme tenant row not in list: %+v", list.Limits)
	}

	// DELETE it.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/_limits?scope=tenant&tenant=acme", nil).WithContext(adminCtx)
	srv.handleLimitDelete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	rows, err := srv.store.TokenLimitsAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.TenantID == "acme" && r.Scope == "tenant" {
			t.Fatalf("acme row still present after delete: %+v", r)
		}
	}
}

// TestLimits_TenantScopingConfinesWrites is the RFC AW tenant-isolation
// regression: a substrate:tenant operator may NOT write the operator-global row
// nor a foreign tenant's row (403), and its own-tenant write is stamped from the
// principal (never the wire tenant_id).
func TestLimits_TenantScopingConfinesWrites(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	tenantCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "op@acme", Scopes: []string{auth.ScopeTenant}})

	call := func(body limitPutRequest) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v1/_limits", bytes.NewReader(b)).WithContext(tenantCtx)
		srv.handleLimitPut(rec, req)
		return rec
	}

	// Operator-global write → 403.
	if rec := call(limitPutRequest{Scope: "operator", HardLimit: i64p(10)}); rec.Code != http.StatusForbidden {
		t.Fatalf("operator write by tenant op: status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Cross-tenant write → 403.
	if rec := call(limitPutRequest{Scope: "tenant", TenantID: strPtr("evil"), HardLimit: i64p(10)}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant write: status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Own-tenant write (no wire tenant_id) → 200, stamped to acme.
	if rec := call(limitPutRequest{Scope: "user", ScopeID: "u1", HardLimit: i64p(10)}); rec.Code != http.StatusOK {
		t.Fatalf("own-tenant write: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	rows, err := srv.store.TokenLimitsAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Scope == "user" && r.ScopeID == "u1" {
			found = true
			if r.TenantID != "acme" {
				t.Fatalf("user row stamped tenant = %q, want acme (from principal)", r.TenantID)
			}
			if r.UpdatedBy != "op@acme" {
				t.Fatalf("updated_by = %q, want op@acme", r.UpdatedBy)
			}
		}
	}
	if !found {
		t.Fatalf("own-tenant user row not persisted: %+v", rows)
	}
}

func strPtr(s string) *string { return &s }
