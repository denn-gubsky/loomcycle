package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A consolidation fan-out with no tenant sweeps EVERY tenant, running its agent
// and prompt as each discovered user. Tenant "" is not proof the operator wrote
// it — a config principal with no tenant and no substrate:admin, or an agent in
// a run executing in "", stores its defs there too — so the ScheduleDef tool
// records operator authority on the version itself (operator_layer), and refuses
// a tenant-less fan-out from anyone without it.

const fanoutOverlay = `{"agent":"job-search-batch","schedule":"0 * * * *","metadata":{"memory_consolidation_fanout":true}}`

// tenantlessOperatorAdmin is an admin principal in the operator layer.
func tenantlessOperatorAdmin(base context.Context) context.Context {
	ctx := asTenant(base, "")
	return auth.WithPrincipal(ctx, auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
}

// nonOperatorEmptyTenantCtxs are the two shapes that write in tenant "" without
// operator authority: a tenant-less principal that is not an admin, and an
// in-run context (no principal, no operator marker).
func nonOperatorEmptyTenantCtxs(base context.Context) map[string]context.Context {
	return map[string]context.Context{
		"tenant-less non-admin principal": auth.WithPrincipal(asTenant(base, ""),
			auth.Principal{Subject: "svc", Scopes: []string{auth.ScopeTenant}}),
		"in-run context with no principal": asTenant(base, ""),
	}
}

// storedOperatorLayer reads operator_layer off the active version of name in
// tenant "".
func storedOperatorLayer(t *testing.T, sd *ScheduleDef, name string) bool {
	t.Helper()
	row, err := sd.Store.ScheduleDefGetActive(context.Background(), "", name)
	if err != nil {
		t.Fatalf("get active %q: %v", name, err)
	}
	var body struct {
		OperatorLayer bool `json:"operator_layer"`
	}
	if err := json.Unmarshal(row.Definition, &body); err != nil {
		t.Fatalf("decode %q: %v", name, err)
	}
	return body.OperatorLayer
}

func TestScheduleDef_NonAdminEmptyTenantCannotAuthorOperatorLayerFanout(t *testing.T) {
	sd, base, done := scheduleDefFixture(t)
	defer done()
	// The operator's own yaml fan-out, which a fork would materialise.
	sd.Cfg.ScheduledRuns["consolidate"] = config.ScheduledRun{
		Agent: "job-search-batch", Schedule: "0 * * * *", Enabled: true,
		Metadata: map[string]any{"memory_consolidation_fanout": true},
	}
	for who, ctx := range nonOperatorEmptyTenantCtxs(base) {
		t.Run(who, func(t *testing.T) {
			res := execDef(t, sd, ctx, `{"op":"create","name":"sweep","overlay":`+fanoutOverlay+`}`)
			requireRefusedAsValidation(t, "create", res)
			if _, err := sd.Store.ScheduleDefGetActive(context.Background(), "", "sweep"); err == nil {
				t.Error("a refused create stored a version")
			}
			requireRefusedAsValidation(t, "fork",
				execDef(t, sd, ctx, `{"op":"fork","name":"consolidate","overlay":{"enabled":true}}`))
		})
	}
	// The same calls as an admin are the operator's, and store the bit.
	admin := tenantlessOperatorAdmin(base)
	if res := execDef(t, sd, admin, `{"op":"create","name":"sweep","overlay":`+fanoutOverlay+`}`); res.IsError {
		t.Fatalf("admin create: %s", res.Text)
	}
	if !storedOperatorLayer(t, sd, "sweep") {
		t.Error("an admin's tenant-less fan-out was stored without operator_layer")
	}
	if res := execDef(t, sd, admin, `{"op":"fork","name":"consolidate","overlay":{"enabled":true}}`); res.IsError {
		t.Fatalf("admin fork of the yaml fan-out: %s", res.Text)
	}
	if !storedOperatorLayer(t, sd, "consolidate") {
		t.Error("an admin's fork of the yaml fan-out was stored without operator_layer")
	}
}

// operator_layer is server authority: no overlay can set or clear it, and every
// write path re-derives it from the caller.
func TestScheduleDef_OperatorLayerIsStampedFromTheCallerNeverTheOverlay(t *testing.T) {
	sd, base, done := scheduleDefFixture(t)
	defer done()
	plain := `{"agent":"job-search-batch","schedule":"0 * * * *","operator_layer":%s}`
	admin := tenantlessOperatorAdmin(base)
	inRun := asTenant(base, "")

	if res := execDef(t, sd, admin, `{"op":"create","name":"a","overlay":`+fmt.Sprintf(plain, "false")+`}`); res.IsError {
		t.Fatalf("admin create: %s", res.Text)
	}
	if !storedOperatorLayer(t, sd, "a") {
		t.Error("an overlay operator_layer:false cleared an admin's bit")
	}
	if res := execDef(t, sd, inRun, `{"op":"create","name":"b","overlay":`+fmt.Sprintf(plain, "true")+`}`); res.IsError {
		t.Fatalf("in-run create: %s", res.Text)
	}
	if storedOperatorLayer(t, sd, "b") {
		t.Error("an overlay operator_layer:true set the bit for a caller without operator authority")
	}
	if res := execDef(t, sd, admin, `{"op":"fork","name":"b","overlay":{"enabled":true}}`); res.IsError {
		t.Fatalf("admin fork: %s", res.Text)
	}
	if !storedOperatorLayer(t, sd, "b") {
		t.Error("an admin's re-save (fork) did not stamp the bit")
	}

	// A hook edit re-derives it from the EDITOR: the one adding hooks that
	// would fire in every tenant.
	activeID := func(name string) string {
		row, err := sd.Store.ScheduleDefGetActive(context.Background(), "", name)
		if err != nil {
			t.Fatalf("get active: %v", err)
		}
		return row.DefID
	}
	hook := `{"kind":"channel.publish","channel":"done"}`
	if res := execDef(t, sd, inRun, `{"op":"add_hook","def_id":"`+activeID("a")+`","hook":`+hook+`}`); res.IsError {
		t.Fatalf("in-run add_hook: %s", res.Text)
	}
	if storedOperatorLayer(t, sd, "a") {
		t.Error("a hook edit without operator authority kept the operator's bit")
	}
	if res := execDef(t, sd, admin, `{"op":"remove_hook","def_id":"`+activeID("a")+`","hook_index":0}`); res.IsError {
		t.Fatalf("admin remove_hook: %s", res.Text)
	}
	if !storedOperatorLayer(t, sd, "a") {
		t.Error("an admin's hook edit did not stamp the bit")
	}

	// An admin writing in a tenant is not writing the operator layer.
	tenantAdmin := tenantlessOperatorAdmin(base)
	tenantAdmin = tools.WithRunIdentity(tenantAdmin, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	if res := execDef(t, sd, tenantAdmin, `{"op":"create","name":"c","overlay":`+fmt.Sprintf(plain, "true")+`}`); res.IsError {
		t.Fatalf("tenant admin create: %s", res.Text)
	}
	row, err := sd.Store.ScheduleDefGetActive(context.Background(), "acme", "c")
	if err != nil {
		t.Fatalf("get c: %v", err)
	}
	var body struct {
		OperatorLayer bool `json:"operator_layer"`
	}
	_ = json.Unmarshal(row.Definition, &body)
	if body.OperatorLayer {
		t.Error("a def executing in a tenant was stamped operator_layer")
	}
}
