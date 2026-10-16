package scheduler

import (
	"encoding/json"
	"testing"
)

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

// An untrusted block's kind on a stored schedule reaches the fired run, so a
// "code" block is fenced as written rather than escaped.
func TestBuildRunInput_CarriesTheUntrustedKind(t *testing.T) {
	var def scheduleDef
	body := `{"agent":"review","prompt":[{"role":"user","content":[{"type":"trusted-text","text":"Review this."},{"type":"untrusted-block","kind":"code","text":"a<b"}]}]}`
	if err := json.Unmarshal([]byte(body), &def); err != nil {
		t.Fatal(err)
	}
	in := buildRunInput(def, map[string]bool{}, nil)
	if got := in.Segments[0].Content[1]; got.Kind != "code" || got.Text != "a<b" {
		t.Errorf("block = %+v, want kind code", got)
	}
}
