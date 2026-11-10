package limits

import (
	"context"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The budget gauge's source: each tenant with a tenant-scope ceiling, its
// month-to-date use against the hard ceiling (else the soft one). Operator
// and user ceilings, and tenants with none, are not tenant budgets.
func TestTracker_TenantBudgetsReportsUseAgainstTheCeiling(t *testing.T) {
	tr := New(&fakeStore{
		aggs: []store.UsageAggregate{agg("acme", "u1", 600, 0), agg("beta", "u2", 50, 0), agg("free", "u3", 10, 0)},
		limits: []store.TokenLimitRow{
			{TenantID: "acme", Scope: "tenant", SoftLimit: i64(500), HardLimit: i64(1000)},
			{TenantID: "beta", Scope: "tenant", SoftLimit: i64(100)},
			{Scope: "operator", HardLimit: i64(1_000_000)},
			{TenantID: "free", Scope: "user", ScopeID: "u3", HardLimit: i64(5)},
		},
	})
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr.Add("acme", "u1", 100)
	want := []TenantBudget{{TenantID: "acme", Used: 700, Limit: 1000}, {TenantID: "beta", Used: 50, Limit: 100}}
	if got := tr.TenantBudgets(); !reflect.DeepEqual(got, want) {
		t.Errorf("TenantBudgets = %+v, want %+v", got, want)
	}
	if got := New(nil).TenantBudgets(); got != nil {
		t.Errorf("store-less tracker = %+v, want none", got)
	}
}
