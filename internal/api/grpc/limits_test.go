package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestEventToProto_LimitFrame is the RFC AW event-parity regression: a
// providers.EventLimit must map onto the proto Event with type="limit" and a
// populated LimitInfo, so a gRPC Run stream carries the budget crossing that
// HTTP SSE already delivers. Fails before eventToProto learns the limit field
// (the frame would arrive with type="limit" but a nil Limit → adapters render
// nothing).
func TestEventToProto_LimitFrame(t *testing.T) {
	ev := providers.Event{Type: providers.EventLimit, Limit: &providers.LimitInfo{
		Scope: "tenant", ScopeID: "acme", Severity: "soft", Window: "month",
		Used: 1200, Limit: 1000, Message: "tenant acme soft budget reached",
	}}
	out := eventToProto(ev)
	if out.GetType() != "limit" {
		t.Fatalf("type = %q, want limit", out.GetType())
	}
	li := out.GetLimit()
	if li == nil {
		t.Fatal("limit payload is nil; the frame would carry no budget info")
	}
	if li.GetScope() != "tenant" || li.GetScopeId() != "acme" || li.GetSeverity() != "soft" ||
		li.GetWindow() != "month" || li.GetUsed() != 1200 || li.GetLimit() != 1000 ||
		li.GetMessage() != "tenant acme soft budget reached" {
		t.Fatalf("limit payload mismatch: %+v", li)
	}
}

// limitsTestServer builds a gRPC adapter with a real store + a shared
// limits.Tracker, so the TokenLimit RPC exercises the same used-lookup + reload
// path the live server wires from srv.LimitsTracker().
func limitsTestServer(t *testing.T) (*Server, store.Store, *limits.Tracker) {
	t.Helper()
	st := newTestStore(t)
	tr := limits.New(st)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Limits: tr})
	return adapter, st, tr
}

// TestGrpcTokenLimit_RoundTripAndScope is the RFC AW CRUD-parity regression for
// the gRPC twin of /v1/_limits: set/list/delete round-trips, tenant scoping
// confines a substrate:tenant operator to its own tenant, and the operator +
// cross-tenant writes are admin-only. Fails before the TokenLimit RPC is wired.
func TestGrpcTokenLimit_RoundTripAndScope(t *testing.T) {
	adapter, st, tr := limitsTestServer(t)
	ctx := context.Background()

	// Seed operator month-to-date usage so the live `used` is observable.
	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{
		RunID: "r", TenantID: "", UserID: "seed", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 1000, TS: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Seed(ctx); err != nil {
		t.Fatal(err)
	}

	admin := scopedCtx("ops", "root", auth.ScopeAdmin)
	acme := scopedCtx("acme", "op@acme", auth.ScopeTenant)

	set := func(c context.Context, op *loomcyclepb.TokenLimitRequest) (*loomcyclepb.TokenLimitResponse, error) {
		return adapter.TokenLimit(c, op)
	}
	i64 := func(v int64) *int64 { return &v }

	// Admin sets the operator-global budget + an evil-tenant budget.
	if _, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "operator", HardLimit: i64(5000)}); err != nil {
		t.Fatalf("admin set operator: %v", err)
	}
	if _, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "tenant", Tenant: "evil", HardLimit: i64(10)}); err != nil {
		t.Fatalf("admin set evil tenant: %v", err)
	}

	// Admin list sees both rows; the operator row reports the seeded used=1000.
	all, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "list"})
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	var sawOperator, sawEvil bool
	for _, e := range all.GetLimits() {
		if e.GetScope() == "operator" {
			sawOperator = true
			if e.GetUsed() != 1000 {
				t.Errorf("operator used = %d, want 1000 (from shared tracker)", e.GetUsed())
			}
			if e.GetHardLimit() != 5000 || e.SoftLimit != nil {
				t.Errorf("operator tiers wrong: hard=%d soft=%v", e.GetHardLimit(), e.SoftLimit)
			}
		}
		if e.GetScope() == "tenant" && e.GetTenantId() == "evil" {
			sawEvil = true
		}
	}
	if !sawOperator || !sawEvil {
		t.Fatalf("admin list missing rows: operator=%v evil=%v (%+v)", sawOperator, sawEvil, all.GetLimits())
	}

	// A scoped acme operator sets its OWN user budget (stamped acme, never the
	// wire tenant), and its list never leaks evil's row nor the operator scope.
	wrote, err := set(acme, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "user", ScopeId: "u1", SoftLimit: i64(50), HardLimit: i64(100)})
	if err != nil {
		t.Fatalf("acme set own user: %v", err)
	}
	if len(wrote.GetLimits()) != 1 || wrote.GetLimits()[0].GetTenantId() != "acme" {
		t.Fatalf("acme write not stamped to acme: %+v", wrote.GetLimits())
	}
	if wrote.GetLimits()[0].GetUpdatedBy() != "op@acme" {
		t.Errorf("updated_by = %q, want op@acme", wrote.GetLimits()[0].GetUpdatedBy())
	}
	acmeList, err := set(acme, &loomcyclepb.TokenLimitRequest{Op: "list"})
	if err != nil {
		t.Fatalf("acme list: %v", err)
	}
	for _, e := range acmeList.GetLimits() {
		if e.GetTenantId() == "evil" || e.GetScope() == "operator" {
			t.Errorf("scoped acme list leaked foreign row: %+v", e)
		}
	}

	// Authz: a scoped operator cannot touch the operator-global budget nor a
	// foreign tenant → PermissionDenied.
	if _, err := set(acme, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "operator", HardLimit: i64(1)}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("scoped operator write code=%s, want PermissionDenied", status.Code(err))
	}
	if _, err := set(acme, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "tenant", Tenant: "evil", HardLimit: i64(1)}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("scoped cross-tenant write code=%s, want PermissionDenied", status.Code(err))
	}

	// Bad op + negative limit → InvalidArgument.
	if _, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "bogus"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad op code=%s, want InvalidArgument", status.Code(err))
	}
	if _, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "tenant", Tenant: "acme", SoftLimit: i64(-1)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative limit code=%s, want InvalidArgument", status.Code(err))
	}

	// Delete the evil row (admin) → gone from a subsequent list.
	if _, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "delete", Scope: "tenant", Tenant: "evil"}); err != nil {
		t.Fatalf("admin delete evil: %v", err)
	}
	after, err := set(admin, &loomcyclepb.TokenLimitRequest{Op: "list"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after.GetLimits() {
		if e.GetScope() == "tenant" && e.GetTenantId() == "evil" {
			t.Fatalf("evil row still present after delete: %+v", e)
		}
	}
}
