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
