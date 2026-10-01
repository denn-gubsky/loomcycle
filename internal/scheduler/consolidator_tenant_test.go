package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These tests pin WHICH tenants a consolidation fan-out reaches. The rule: an
// OPERATOR's def in the operator layer (tenant "" — a yaml `scheduled_runs:`
// entry with no `tenant_id`, or a version an admin wrote, which carries
// operator_layer) reaches every tenant, and each dispatched run executes in its
// target's own tenant; a tenant's def reaches only that tenant, and so does a
// tenant-less def nobody with operator authority wrote.

// operatorFanoutDef is fanoutDef as the operator's: the ScheduleDef tool stamps
// operator_layer on a tenant-less version an admin (or an open-mode / stdio
// operator) writes.
func operatorFanoutDef(extraMeta map[string]any) scheduleDef {
	def := fanoutDef(extraMeta)
	def.OperatorLayer = true
	return def
}

// enqueuePending banks one undrained queue row for a user-scope target.
func enqueuePending(t *testing.T, st store.Store, id, tenantID, userID string) {
	t.Helper()
	if err := st.MemoryPendingEnqueue(context.Background(), store.MemoryPendingRow{
		ID: id, TenantID: tenantID, Scope: store.MemoryScopeUser, ScopeID: userID,
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("enqueue %s: %v", id, err)
	}
}

// dispatched renders the children as "tenant/user" in dispatch order.
func dispatched(fr *fakeRunner) []string {
	var out []string
	for _, c := range fr.Calls() {
		out = append(out, c.TenantID+"/"+c.UserID)
	}
	return out
}

// TestFanout_OperatorScheduleConsolidatesLegacyBearerUsers is the reported bug.
// The legacy LOOMCYCLE_AUTH_TOKEN resolves to tenant "default", so its users'
// sessions and queued memory live there, while a yaml consolidation schedule
// lives in the operator layer "". Matching the tenant exactly, the schedule
// found nobody: such a deployment never consolidated its users' memory.
//
// Fails-before: zero runs — alice's session and bob's queue are both in
// "default", and the fan-out only looked in "".
func TestFanout_OperatorScheduleConsolidatesLegacyBearerUsers(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})

	seedSettledSession(t, st, "default", "alice")
	enqueuePending(t, st, "p_bob", "default", "bob")

	fireT(t, sched)

	got := dispatched(fr)
	sort.Strings(got) // dispatched in parallel: compare the set
	if got, want := strings.Join(got, ","), "default/alice,default/bob"; got != want {
		t.Fatalf("dispatched = %s, want %s — the operator's schedule must reach the legacy bearer's users; logs:\n%s", got, want, logs.all())
	}
}

// TestFanout_OperatorScheduleRunsEachTargetInItsOwnTenant: reaching every tenant
// must not mean reading one tenant's memory from another's run. Each child
// carries the tenant its session or queue row lives in — that tenant is what
// every read and write of the run is scoped to — and the same user id in two
// tenants is two targets, each run in its own tenant.
//
// Fails-before: only the operator layer's own target is dispatched.
func TestFanout_OperatorScheduleRunsEachTargetInItsOwnTenant(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), nil)
	// No provider resolver: the batch runs serially, so dispatch order is the
	// target order and can be asserted.

	seedSettledSession(t, st, "acme", "sam")
	seedSettledSession(t, st, "", "olga")
	seedSettledSession(t, st, "default", "sam")
	enqueuePending(t, st, "p_beta", "beta", "quinn")

	fireT(t, sched)

	want := "/olga,acme/sam,beta/quinn,default/sam"
	if got := strings.Join(dispatched(fr), ","); got != want {
		t.Fatalf("dispatched = %s, want %s (every tenant's target, in its own tenant, ordered by tenant then user); logs:\n%s", got, want, logs.all())
	}
	if !logs.contains("across every tenant") {
		t.Errorf("the fire announcement must say the operator-layer schedule reaches every tenant; logs:\n%s", logs.all())
	}
}

// TestFanout_OperatorScheduleHonoursCapAcrossTenants: the per-tick cap bounds
// the WHOLE fan-out, not each tenant — an operator's schedule over many tenants
// must not multiply its run count by the tenant count. What does not fit is
// deferred and logged, never silently dropped.
//
// Fails-before: zero runs (no target is in "").
func TestFanout_OperatorScheduleHonoursCapAcrossTenants(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
		c.MaxConsolidationTargets = 2
	})

	for i, tenant := range []string{"acme", "beta", "default", "gamma"} {
		seedSettledSession(t, st, tenant, fmt.Sprintf("u%d", i))
	}

	fireT(t, sched)

	if got := len(fr.Calls()); got != 2 {
		t.Fatalf("RunOnce calls = %d, want 2 (the cap, across all tenants); logs:\n%s", got, logs.all())
	}
	if !logs.contains("2 target(s) with new work deferred") {
		t.Errorf("the deferred remainder must be logged; logs:\n%s", logs.all())
	}
	calls := fr.Calls()
	if a, b := calls[0], calls[1]; a.TenantID > b.TenantID || (a.TenantID == b.TenantID && a.UserID > b.UserID) {
		t.Errorf("dispatch order %s/%s then %s/%s is not (tenant, user) order", a.TenantID, a.UserID, b.TenantID, b.UserID)
	}
}

// TestFanout_OperatorScheduleWithholdsItsCredentialsFromOtherTenants: a run
// dispatched into another tenant resolves its agent in THAT tenant, where a
// tenant's fork of the consolidator wins — so the schedule's literal
// credentials would reach code the operator did not author. They stay with runs
// in the schedule's own tenant.
//
// Fails-before: the "default" target is never dispatched, so there is no run to
// withhold from. Fails without the strip: that run carries the credential.
func TestFanout_OperatorScheduleWithholdsItsCredentialsFromOtherTenants(t *testing.T) {
	def := operatorFanoutDef(nil)
	def.UserCredentials = map[string]string{"svc": "operator-literal"}
	sched, fr, st, logs := fanoutFixture(t, def, nil)

	seedSettledSession(t, st, "", "olga")
	seedSettledSession(t, st, "default", "alice")

	fireT(t, sched)

	byTenant := map[string]map[string]string{}
	for _, c := range fr.Calls() {
		byTenant[c.TenantID] = c.UserCredentials
	}
	creds, ok := byTenant["default"]
	if !ok {
		t.Fatalf("no run dispatched into tenant default; dispatched = %v", dispatched(fr))
	}
	if len(creds) != 0 {
		t.Errorf("the run in tenant default carries the operator schedule's credentials %v", creds)
	}
	if got := byTenant[""]["svc"]; got != "operator-literal" {
		t.Errorf("the run in the schedule's own tenant lost its credential (got %q)", got)
	}
	if !logs.contains("carry none of this schedule's credentials") {
		t.Errorf("withholding credentials must be logged; logs:\n%s", logs.all())
	}
}

// TestFanout_LegacyEmptyBodyTenantRunsInOwningTenant: a row a tenant wrote
// before create stamped the author's tenant into the body has an EMPTY body
// tenant but a real owner. It is that tenant's schedule: it must neither sweep
// every tenant nor consolidate the operator layer's users — it consolidates its
// owner's.
//
// Fails-before: dispatched "/olga" (confined to the operator layer "").
func TestFanout_LegacyEmptyBodyTenantRunsInOwningTenant(t *testing.T) {
	sched, fr, st := fanoutFixtureOwnedBy(t, fanoutDef(nil), "acme")

	seedSettledSession(t, st, "", "olga")
	seedSettledSession(t, st, "acme", "sam")
	seedSettledSession(t, st, "default", "alice")

	fireT(t, sched)

	if got, want := strings.Join(dispatched(fr), ","), "acme/sam"; got != want {
		t.Fatalf("dispatched = %s, want %s — a tenant-owned def runs in its owner's tenant and nowhere else", got, want)
	}
}

// TestFanout_EmptyTenantDefWithoutOperatorAuthorityStaysConfined is sched-1:
// a tenant-less fan-out that nobody with operator authority wrote — a config
// principal with no tenant, or an agent in a run executing in "" — must not run
// its agent and prompt as every user of every tenant. It stays in "", and the
// log says why.
//
// Fails-before: dispatched "/olga,acme/sam" — the whole deployment.
func TestFanout_EmptyTenantDefWithoutOperatorAuthorityStaysConfined(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, fanoutDef(nil), nil)

	seedSettledSession(t, st, "", "olga")
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	if got, want := strings.Join(dispatched(fr), ","), "/olga"; got != want {
		t.Fatalf("dispatched = %s, want %s — a tenant-less def without operator authority swept other tenants; logs:\n%s", got, want, logs.all())
	}
	if !logs.contains("not authored with operator authority") {
		t.Errorf("confining the fan-out must say why; logs:\n%s", logs.all())
	}
}

// TestFanout_AdminAuthoredFanoutStillSweeps: the operator's own tenant-less
// fan-out — an admin's version (operator_layer) or the row bootstrapped from
// the yaml — keeps its reach across every tenant.
func TestFanout_AdminAuthoredFanoutStillSweeps(t *testing.T) {
	sched, fr, st, _ := fanoutFixture(t, operatorFanoutDef(nil), nil)
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	if got, want := strings.Join(dispatched(fr), ","), "acme/sam"; got != want {
		t.Fatalf("dispatched = %s, want %s — an admin's fan-out must sweep every tenant", got, want)
	}
}

func TestFanout_BootstrappedYamlFanoutStillSweeps(t *testing.T) {
	sched, fr, st := fanoutFixtureRow(t, fanoutDef(nil), "", true)
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	if got, want := strings.Join(dispatched(fr), ","), "acme/sam"; got != want {
		t.Fatalf("dispatched = %s, want %s — the yaml's fan-out must sweep every tenant", got, want)
	}
}

// fanoutFixtureOwnedBy is fanoutFixture with the schedule row OWNED by a given
// tenant (schedulerFixture's row is owned by "").
func fanoutFixtureOwnedBy(t *testing.T, def scheduleDef, owner string) (*Scheduler, *fakeRunner, store.Store) {
	t.Helper()
	return fanoutFixtureRow(t, def, owner, false)
}

// fanoutFixtureRow is fanoutFixtureOwnedBy with the row's
// bootstrapped_from_static flag set as given.
func fanoutFixtureRow(t *testing.T, def scheduleDef, owner string, bootstrapped bool) (*Scheduler, *fakeRunner, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	ctx := context.Background()
	const defID = "sd-owned"
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID: defID, Name: "sched-owned", Definition: defJSON, TenantID: owner,
		BootstrappedFromStatic: bootstrapped,
	}); err != nil {
		t.Fatalf("def create: %v", err)
	}
	if err := st.ScheduleDefSetActive(ctx, owner, "sched-owned", defID, "test"); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if err := st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	fr := &fakeRunner{}
	sched := New(Config{TickInterval: 10 * time.Millisecond, FireTimeout: 5 * time.Second}, st, fr, nil, &fakeMCP{}, t.Logf)
	sched.SetChannelWriter(&channels.StorePublisher{Store: st})
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	return sched, fr, st
}
