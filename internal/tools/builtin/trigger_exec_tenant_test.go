package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A trigger def's execution tenant (the definition's tenant_id) is where the
// fired run resolves agents / skills / MCP servers and writes memory and runs.
// A non-admin author may only point it at its own tenant; these tests run every
// case against both run-triggering def tools.

// triggerHarness drives one trigger-def tool the same way for every case.
type triggerHarness struct {
	kind    string
	tool    tools.Tool
	base    context.Context // fixture ctx: authoring policy granted, no identity
	overlay string          // a valid overlay body, WITHOUT tenant_id and braces
	// execTenant reads the definition's tenant_id of the active version of
	// name under owningTenant; ok=false when there is no active version.
	execTenant func(t *testing.T, owningTenant, name string) (string, bool)
}

func triggerHarnesses(t *testing.T) []triggerHarness {
	t.Helper()
	wh, whCtx, whClean := webhookDefFixture(t)
	t.Cleanup(whClean)
	sd, sdCtx, sdClean := scheduleDefFixture(t)
	t.Cleanup(sdClean)
	decodeTenant := func(t *testing.T, body []byte) string {
		t.Helper()
		var m struct {
			TenantID string `json:"tenant_id"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("decode definition: %v", err)
		}
		return m.TenantID
	}
	return []triggerHarness{
		{
			kind: "WebhookDef", tool: wh, base: whCtx,
			overlay: `"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`,
			execTenant: func(t *testing.T, owning, name string) (string, bool) {
				row, err := wh.Store.WebhookDefGetActive(context.Background(), owning, name)
				if err != nil {
					return "", false
				}
				return decodeTenant(t, row.Definition), true
			},
		},
		{
			kind: "ScheduleDef", tool: sd, base: sdCtx,
			overlay: `"agent":"job-search-batch","schedule":"0 9 * * 1"`,
			execTenant: func(t *testing.T, owning, name string) (string, bool) {
				row, err := sd.Store.ScheduleDefGetActive(context.Background(), owning, name)
				if err != nil {
					return "", false
				}
				return decodeTenant(t, row.Definition), true
			},
		},
	}
}

// as returns the harness ctx acting as a principal of tenant: a tenant
// operator, or an admin.
func (h triggerHarness) as(tenant string, admin bool) context.Context {
	scopes := []string{auth.ScopeTenant}
	if admin {
		scopes = []string{auth.ScopeAdmin}
	}
	ctx := tools.WithRunIdentity(h.base, tools.RunIdentityValue{AgentID: "a_test", TenantID: tenant})
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "op-" + tenant, Scopes: scopes})
}

// call runs one op; tenantID "" leaves tenant_id out of the overlay.
func (h triggerHarness) call(ctx context.Context, op, name, tenantID string) tools.Result {
	ov := h.overlay
	if op == "fork" {
		ov = `"enabled":true` // a fork restates nothing; the parent carries the body
	}
	if tenantID != "" {
		ov += `,"tenant_id":"` + tenantID + `"`
	}
	res, _ := h.tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"`+name+`","overlay":{`+ov+`}}`))
	return res
}

func requireRefusedAsValidation(t *testing.T, kind string, res tools.Result) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("%s: want a refusal, got success: %s", kind, res.Text)
	}
	if res.Error == nil || res.Error.Category != tools.CategoryValidation {
		t.Fatalf("%s: want a validation refusal, got %+v: %s", kind, res.Error, res.Text)
	}
}

func TestTriggerDefs_NonAdminCreateNamingAnotherTenantIsRefusedAndStoresNothing(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		res := h.call(h.as("acme", false), "create", "grab", "globex")
		requireRefusedAsValidation(t, h.kind, res)
		if got, ok := h.execTenant(t, "acme", "grab"); ok {
			t.Errorf("%s: a refused create stored a version executing in %q", h.kind, got)
		}
	}
}

func TestTriggerDefs_NonAdminForkNamingAnotherTenantIsRefused(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		acme := h.as("acme", false)
		if res := h.call(acme, "create", "mine", ""); res.IsError {
			t.Fatalf("%s: create: %s", h.kind, res.Text)
		}
		requireRefusedAsValidation(t, h.kind, h.call(acme, "fork", "mine", "globex"))
		if got, _ := h.execTenant(t, "acme", "mine"); got != "acme" {
			t.Errorf("%s: active version executes in %q after a refused fork, want acme", h.kind, got)
		}
	}
}

// A shared ("") def an admin pointed at globex is forkable by every tenant.
// A tenant operator's fork must not inherit globex: it is refused until the
// fork names the forker's own tenant, which re-homes it.
func TestTriggerDefs_NonAdminForkDoesNotInheritAnotherTenantsExecTenant(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		if res := h.call(h.as("", true), "create", "shared", "globex"); res.IsError {
			t.Fatalf("%s: admin create of the shared parent: %s", h.kind, res.Text)
		}
		acme := h.as("acme", false)
		requireRefusedAsValidation(t, h.kind, h.call(acme, "fork", "shared", ""))
		if got, ok := h.execTenant(t, "acme", "shared"); ok {
			t.Errorf("%s: a refused fork left an acme version executing in %q", h.kind, got)
		}
		if res := h.call(acme, "fork", "shared", "acme"); res.IsError {
			t.Fatalf("%s: fork re-homed to the forker's own tenant: %s", h.kind, res.Text)
		}
		if got, _ := h.execTenant(t, "acme", "shared"); got != "acme" {
			t.Errorf("%s: re-homed fork executes in %q, want acme", h.kind, got)
		}
	}
}

func TestTriggerDefs_NonAdminOwnOrOmittedTenantExecutesInOwnTenant(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		acme := h.as("acme", false)
		if res := h.call(acme, "create", "explicit", "acme"); res.IsError {
			t.Fatalf("%s: create naming own tenant: %s", h.kind, res.Text)
		}
		if res := h.call(acme, "create", "omitted", ""); res.IsError {
			t.Fatalf("%s: create omitting tenant: %s", h.kind, res.Text)
		}
		if res := h.call(acme, "fork", "omitted", "acme"); res.IsError {
			t.Fatalf("%s: fork naming own tenant: %s", h.kind, res.Text)
		}
		for _, name := range []string{"explicit", "omitted"} {
			if got, _ := h.execTenant(t, "acme", name); got != "acme" {
				t.Errorf("%s %s: executes in %q, want acme", h.kind, name, got)
			}
		}
	}
}

func TestTriggerDefs_AdminMayNameAnotherTenant(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		admin := h.as("acme", true)
		if res := h.call(admin, "create", "ops", "globex"); res.IsError {
			t.Fatalf("%s: admin create naming another tenant: %s", h.kind, res.Text)
		}
		if res := h.call(admin, "fork", "ops", "initech"); res.IsError {
			t.Fatalf("%s: admin fork naming another tenant: %s", h.kind, res.Text)
		}
		if got, _ := h.execTenant(t, "acme", "ops"); got != "initech" {
			t.Errorf("%s: admin-authored version executes in %q, want initech", h.kind, got)
		}
	}
}

// Rows written before the guard stay as they are; the boot audit names each
// one whose runs execute outside its owning tenant, and nothing else.
func TestTriggerDefs_AuditWarnsOnlyOnRowsExecutingOutsideTheirOwningTenant(t *testing.T) {
	wh, _, whClean := webhookDefFixture(t)
	defer whClean()
	sd, _, sdClean := scheduleDefFixture(t)
	defer sdClean()
	ctx := context.Background()
	rows := []struct {
		name, owning, body string
		bootstrapped       bool
	}{
		{"pre-guard", "acme", `{"tenant_id":"globex"}`, false},
		{"own", "acme", `{"tenant_id":"acme"}`, false},
		{"legacy-unstamped", "acme", `{}`, false},
		{"from-yaml", "", `{"tenant_id":"globex"}`, true},
	}
	for i, r := range rows {
		id := fmt.Sprintf("def_%d", i)
		if _, err := wh.Store.WebhookDefCreate(ctx, store.WebhookDefRow{DefID: "wh" + id, Name: r.name, Definition: json.RawMessage(r.body), TenantID: r.owning, BootstrappedFromStatic: r.bootstrapped}); err != nil {
			t.Fatalf("seed webhook %s: %v", r.name, err)
		}
		if _, err := sd.Store.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: "sd" + id, Name: r.name, Definition: json.RawMessage(r.body), TenantID: r.owning, BootstrappedFromStatic: r.bootstrapped}); err != nil {
			t.Fatalf("seed schedule %s: %v", r.name, err)
		}
	}
	if err := wh.Store.WebhookDefSetActive(ctx, "acme", "pre-guard", "whdef_0", "test"); err != nil {
		t.Fatalf("set active: %v", err)
	}

	whWarn, err := wh.ForeignExecTenantWarnings(ctx)
	if err != nil {
		t.Fatalf("webhook audit: %v", err)
	}
	sdWarn, err := sd.ForeignExecTenantWarnings(ctx)
	if err != nil {
		t.Fatalf("schedule audit: %v", err)
	}
	for kind, warns := range map[string][]string{"webhook": whWarn, "schedule": sdWarn} {
		if len(warns) != 1 || !strings.Contains(warns[0], `"pre-guard"`) || !strings.Contains(warns[0], `tenant "globex"`) {
			t.Errorf("%s audit = %q, want exactly the pre-guard row", kind, warns)
		}
	}
	if !strings.Contains(whWarn[0], "ACTIVE") {
		t.Errorf("webhook audit does not flag the active version as ACTIVE: %q", whWarn[0])
	}
}

// A hook edit re-mints the parent's body as a new, auto-promoted version: a
// tenant operator must not use it on a def an admin pointed at another tenant,
// while a hook edit of its own def keeps working.
func TestScheduleDefTool_NonAdminHookEditOfDefExecutingElsewhereIsRefused(t *testing.T) {
	var h triggerHarness
	for _, c := range triggerHarnesses(t) {
		if c.kind == "ScheduleDef" {
			h = c
		}
	}
	if res := h.call(h.as("acme", true), "create", "admin-made", "globex"); res.IsError {
		t.Fatalf("admin create: %s", res.Text)
	}
	if res := h.call(h.as("acme", false), "create", "own", ""); res.IsError {
		t.Fatalf("create own: %s", res.Text)
	}
	acme := h.as("acme", false)
	sd := h.tool.(*ScheduleDef)
	hook := `"hook":{"kind":"channel.publish","channel":"results"}`
	for _, c := range []struct {
		name    string
		refused bool
	}{{"admin-made", true}, {"own", false}} {
		row, err := sd.Store.ScheduleDefGetActive(context.Background(), "acme", c.name)
		if err != nil {
			t.Fatalf("get active %s: %v", c.name, err)
		}
		res, _ := sd.Execute(acme, json.RawMessage(`{"op":"add_hook","def_id":"`+row.DefID+`",`+hook+`}`))
		if c.refused {
			requireRefusedAsValidation(t, "add_hook "+c.name, res)
			if again, _ := sd.Store.ScheduleDefGetActive(context.Background(), "acme", c.name); again.DefID != row.DefID {
				t.Errorf("add_hook %s: a refused hook edit promoted a new version", c.name)
			}
		} else if res.IsError {
			t.Errorf("add_hook %s: %s", c.name, res.Text)
		}
	}
}
