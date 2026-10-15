package scheduler

import "testing"

// A schedule's user_bearer reaches the fired run as the reference, unopened.
func TestBuildRunInput_CarriesTheBearerReference(t *testing.T) {
	in := buildRunInput(scheduleDef{Agent: "digest", UserBearer: "$cred:tok"}, map[string]bool{}, nil)
	if in.UserBearer != "$cred:tok" {
		t.Errorf("UserBearer = %q, want the reference", in.UserBearer)
	}
}

// A consolidation run in another tenant drops the bearer with the schedule's
// credentials: its reference would resolve against that tenant's credential
// of the same name. A run in the schedule's own tenant keeps it.
func TestFanout_OperatorScheduleWithholdsItsBearerFromOtherTenants(t *testing.T) {
	def := operatorFanoutDef(nil)
	def.UserBearer = "$cred:operator-tok"
	sched, fr, st, logs := fanoutFixture(t, def, nil)
	seedSettledSession(t, st, "", "olga")
	seedSettledSession(t, st, "default", "alice")

	fireT(t, sched)

	byTenant := map[string]string{}
	seen := map[string]bool{}
	for _, c := range fr.Calls() {
		byTenant[c.TenantID] = c.UserBearer
		seen[c.TenantID] = true
	}
	if !seen["default"] || !seen[""] {
		t.Fatalf("dispatched = %v, want runs in both tenants", dispatched(fr))
	}
	if got := byTenant["default"]; got != "" {
		t.Errorf("the run in tenant default carries the bearer %q", got)
	}
	if got := byTenant[""]; got != "$cred:operator-tok" {
		t.Errorf("the run in the schedule's own tenant lost its bearer (got %q)", got)
	}
	if !logs.contains("carry none of this schedule's credentials") {
		t.Errorf("withholding the bearer must be logged; logs:\n%s", logs.all())
	}
}
