package scheduler

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// A row a tenant wrote before create stamped the author's tenant into the body
// has an EMPTY body tenant but a real owner. The fire path reads only the body,
// so tenant X's schedule ran and published in the operator layer "". It is X's
// schedule, and executes in X. A row bootstrapped from the operator's yaml keeps
// its empty tenant: the yaml said "operator layer".

// TestFireOne_LegacyEmptyBodyTenantRunsInOwningTenant fails-before: the run
// executed in tenant "".
func TestFireOne_LegacyEmptyBodyTenantRunsInOwningTenant(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}

	sched, fr, _ := fanoutFixtureOwnedBy(t, def, "acme")
	fireT(t, sched)
	calls := fr.Calls()
	if len(calls) != 1 || calls[0].TenantID != "acme" {
		t.Fatalf("a legacy row owned by acme ran in %v, want one run in acme", tenantsOf(calls))
	}

	sched, fr, _ = fanoutFixtureRow(t, def, "acme", true)
	fireT(t, sched)
	calls = fr.Calls()
	if len(calls) != 1 || calls[0].TenantID != "" {
		t.Fatalf("a row bootstrapped from the yaml ran in %v, want the operator layer \"\"", tenantsOf(calls))
	}
}

// TestChannelDelivery_LegacyEmptyBodyTenantPublishesInOwningTenant fails-before:
// the tick resolved and landed in tenant "".
func TestChannelDelivery_LegacyEmptyBodyTenantPublishesInOwningTenant(t *testing.T) {
	enabled := true
	def := scheduleDef{Delivery: "channel", Channel: "clock", Schedule: "0 * * * *", Enabled: &enabled}
	sched, _, st := fanoutFixtureOwnedBy(t, def, "acme")
	rec := &tenantRecorder{scope: "global"}
	sched.SetChannelScope(rec.resolve)

	fireT(t, sched)

	if got := rec.tenants(); len(got) != 1 || got[0] != "acme" {
		t.Fatalf("resolved the tick's channel in tenants %q, want [acme]", got)
	}
	if n := len(globalLayer(t, st, "acme", "clock")); n != 1 {
		t.Errorf("acme's layer holds %d tick(s), want 1", n)
	}
	if n := len(globalLayer(t, st, "", "clock")); n != 0 {
		t.Errorf("the operator layer, which every tenant reads, holds %d tick(s) from acme's schedule", n)
	}
}

func tenantsOf(calls []runner.RunInput) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.TenantID)
	}
	return out
}
