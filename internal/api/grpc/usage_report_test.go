package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestGrpcUsageReport is the gRPC twin of the HTTP /v1/_usage tests: grouped
// aggregation, tenant scoping (a scoped principal is confined to its own
// tenant; admin sees all), and group_by whitelist validation.
func TestGrpcUsageReport(t *testing.T) {
	adapter, st := tenantTestServer(t)
	ctx := context.Background()
	seed := func(tenant, source string, in int, cost float64) {
		if err := st.RecordCallUsage(ctx, store.TokenUsageRow{
			RunID: "r", TenantID: tenant, Provider: "p", Model: "m",
			CredentialSource: source, InputTokens: in, Cost: cost, CostCurrency: "USD",
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("acme", "operator", 100, 1.0)
	seed("acme", "tenant", 200, 2.0)
	seed("evil", "operator", 400, 4.0)

	// A scoped acme principal sees only acme rows (total cost 3.0), never evil.
	acme := scopedCtx("acme", "alice", auth.ScopeTenant)
	resp, err := adapter.UsageReport(acme, &loomcyclepb.UsageReportRequest{GroupBy: []string{"tenant", "source"}})
	if err != nil {
		t.Fatalf("UsageReport(acme): %v", err)
	}
	var acmeTotal float64
	for _, r := range resp.GetRows() {
		if r.GetTenantId() == "evil" {
			t.Errorf("scoped report leaked tenant evil: %+v", r)
		}
		acmeTotal += r.GetCost()
	}
	if acmeTotal != 3.0 {
		t.Errorf("acme total cost = %v, want 3.0", acmeTotal)
	}

	// Admin sees all tenants: the operator bill across both = 5.0.
	admin := scopedCtx("acme", "ops", auth.ScopeAdmin)
	all, err := adapter.UsageReport(admin, &loomcyclepb.UsageReportRequest{GroupBy: []string{"source"}})
	if err != nil {
		t.Fatalf("UsageReport(admin): %v", err)
	}
	var opBill float64
	for _, r := range all.GetRows() {
		if r.GetCredentialSource() == "operator" {
			opBill = r.GetCost()
		}
	}
	if opBill != 5.0 {
		t.Errorf("operator bill = %v, want 5.0", opBill)
	}

	// An admin tenant focus scopes back down to one tenant.
	focused, _ := adapter.UsageReport(admin, &loomcyclepb.UsageReportRequest{GroupBy: []string{"tenant"}, Tenant: "evil"})
	if len(focused.GetRows()) != 1 || focused.GetRows()[0].GetTenantId() != "evil" {
		t.Errorf("admin ?tenant=evil = %+v, want only evil", focused.GetRows())
	}

	// Unknown group_by dimension → InvalidArgument.
	if _, err := adapter.UsageReport(admin, &loomcyclepb.UsageReportRequest{GroupBy: []string{"bogus"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad group_by code=%s, want InvalidArgument", status.Code(err))
	}
}

// TestGrpcUsageReport_GroupByPurposeCarriesPurposeAndAudioSeconds — the gRPC
// twin of group_by=purpose: one row per purpose, each carrying its purpose, and
// the speech-to-text row the audio seconds it transcribed.
func TestGrpcUsageReport_GroupByPurposeCarriesPurposeAndAudioSeconds(t *testing.T) {
	adapter, st := tenantTestServer(t)
	ctx := context.Background()
	for _, r := range []store.TokenUsageRow{
		{RunID: "r", TenantID: "acme", Provider: "p", Model: "m", CredentialSource: "operator",
			InputTokens: 100, Cost: 1, CostCurrency: "USD"},
		{RunID: "r", TenantID: "acme", Provider: "p", Model: "m", CredentialSource: "operator",
			InputTokens: 300, Cost: 0.3, CostCurrency: "USD", Purpose: store.UsagePurposeOCR},
		{RunID: "r", TenantID: "acme", Provider: "w", Model: "w", CredentialSource: "operator",
			Cost: 0.5, CostCurrency: "USD", Purpose: store.UsagePurposeSTT, AudioSeconds: 75},
	} {
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := adapter.UsageReport(scopedCtx("acme", "ops", auth.ScopeAdmin),
		&loomcyclepb.UsageReportRequest{GroupBy: []string{"purpose"}})
	if err != nil {
		t.Fatalf("UsageReport(group_by=purpose): %v", err)
	}
	byPurpose := map[string]*loomcyclepb.UsageAggregate{}
	for _, r := range resp.GetRows() {
		byPurpose[r.GetPurpose()] = r
	}
	if len(byPurpose) != 3 {
		t.Fatalf("rows by purpose = %v, want chat, ocr and stt", resp.GetRows())
	}
	if r := byPurpose["ocr"]; r.GetInputTokens() != 300 || r.GetCost() != 0.3 || r.GetAudioSeconds() != 0 {
		t.Errorf("ocr row = %v, want 300 tokens, cost 0.3, no audio", r)
	}
	if r := byPurpose["stt"]; r.GetCallCount() != 1 || r.GetAudioSeconds() != 75 {
		t.Errorf("stt row = %v, want 1 call with 75 audio seconds", r)
	}
	if r := byPurpose["chat"]; r.GetInputTokens() != 100 {
		t.Errorf("chat row = %v, want 100 tokens", r)
	}
}

// TestGrpcUsageReport_ChartParameters — the gRPC twin of bucket, filters, top
// and the new dimensions; a scoped principal's filters narrow only within its
// own tenant, and tenant is not a filter.
func TestGrpcUsageReport_ChartParameters(t *testing.T) {
	adapter, st := tenantTestServer(t)
	ctx := context.Background()
	for _, r := range []store.TokenUsageRow{
		{Model: "big", TenantID: "acme", Tier: "high", Cost: 9, InputTokens: 9},
		{Model: "mid", TenantID: "acme", Tier: "middle", Cost: 3, InputTokens: 3},
		{Model: "small", TenantID: "acme", Purpose: store.UsagePurposeEmbedding, Cost: 1, InputTokens: 1, InputChars: 40},
		{Model: "big", TenantID: "evil", Tier: "high", Cost: 100, InputTokens: 100},
	} {
		r.RunID, r.Provider, r.CredentialSource, r.CostCurrency = "r", "p", "operator", "USD"
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	acme := scopedCtx("acme", "alice", auth.ScopeTenant)

	resp, err := adapter.UsageReport(acme, &loomcyclepb.UsageReportRequest{
		GroupBy: []string{"model"}, Bucket: "day", Top: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := resp.GetRows()
	if resp.GetBucket() != "day" || len(rows) != 2 || rows[0].GetModel() != "big" || rows[0].GetCost() != 9 ||
		rows[0].GetPeriod() == "" || !rows[1].GetOther() || rows[1].GetCost() != 4 || rows[1].GetInputChars() != 40 {
		t.Errorf("day buckets, top 1 = %+v, want acme's big (9) then other (4, 40 chars), never evil's", rows)
	}

	resp, err = adapter.UsageReport(acme, &loomcyclepb.UsageReportRequest{
		GroupBy: []string{"tier", "class"},
		Filters: []*loomcyclepb.UsageFilter{{Dimension: "model", Values: []string{"big", "small"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, r := range resp.GetRows() {
		total += r.GetCost()
		if r.GetTier() == "high" && r.GetClass() != "foreground" {
			t.Errorf("high-tier row class = %q, want foreground", r.GetClass())
		}
	}
	if total != 10 {
		t.Errorf("acme's big+small = %v, want 10 (evil's big must not count)", total)
	}

	for name, req := range map[string]*loomcyclepb.UsageReportRequest{
		"tenant as a filter": {Filters: []*loomcyclepb.UsageFilter{{Dimension: "tenant", Values: []string{"evil"}}}},
		"unknown bucket":     {Bucket: "fortnight"},
		"unknown dimension":  {Filters: []*loomcyclepb.UsageFilter{{Dimension: "model; DROP TABLE token_usage", Values: []string{"x"}}}},
	} {
		if _, err := adapter.UsageReport(acme, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code %s, want InvalidArgument", name, status.Code(err))
		}
	}
}
