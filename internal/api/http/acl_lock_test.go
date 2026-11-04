package http

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An admin's list on a delegate holds (security re-audit 2026-11-04, B2/B3):
// the documented lock — a deny on author/retire def:acl/<name> — keeps the
// list's binding, the group that carries it and the membership in that group
// in place, and a tenant list cannot take the name of a list the operator
// declared in config or stored for the shared tenant.

// adminLockRules denies the shell and locks the list itself: the delegate
// may neither rewrite nor retire it.
const adminLockRules = `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},` +
	`{"effect":"deny","actions":["author","retire"],"resources":["def:acl/admin-deny"]}]`

// aclEvalBash is how the bearer's own set answers invoke tool:Bash.
func aclEvalBash(t *testing.T, call func(bearer, method, path, body string) (int, string), bearer string) string {
	t.Helper()
	code, body := call(bearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 {
		t.Fatalf("evaluation as %s: %d %s", bearer, code, body)
	}
	out, _ := jsonField(t, body, "outcome").(string)
	return out
}

func TestACLBinding_ADelegateCannotRetireAnAdminsBinding(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			srv, ts, st := aclMgmtServer(t, mode)
			addDelegateOperator(t, st)
			call := func(bearer, method, path, body string) (int, string) {
				return bearerCall(t, ts, bearer, method, path, body)
			}
			if code, body := call(legacyAdminBearer, "POST", "/v1/_acldef?tenant=acme", `{"op":"create","name":"admin-deny","rules":`+adminLockRules+`}`); code != 201 {
				t.Fatalf("admin creates the list: %d %s", code, body)
			}
			code, body := call(legacyAdminBearer, "POST", "/v1/_aclbinding?tenant=acme", `{"acl":"admin-deny","subject_kind":"user","subject_id":"delegate"}`)
			if code != 201 {
				t.Fatalf("admin binds the delegate: %d %s", code, body)
			}
			id, _ := jsonField(t, body, "binding_id").(string)
			if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
				t.Fatalf("before: delegate's Bash = %q, want deny", got)
			}
			if code, body := call(delegateBearer, "POST", "/v1/_acldef", `{"op":"retire","name":"admin-deny"}`); code != 403 {
				t.Errorf("delegate retires the locked list: %d %s, want 403", code, body)
			}
			if code, body := call(delegateBearer, "DELETE", "/v1/_aclbinding/"+id, ""); code != 403 || !strings.Contains(body, `"acl_denied"`) {
				t.Errorf("delegate retires the admin's binding: %d %s, want 403 acl_denied", code, body)
			}
			deleg := aclCaller(t, srv, "acme", "delegate", auth.ScopeTenant)
			if res := twinCall(t, srv, deleg, "aclbinding", `{"op":"delete","id":"`+id+`"}`); !res.IsError || res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryPermission {
				t.Errorf("delegate retires the binding over the twin: %+v, want a permission refusal", res)
			}
			if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
				t.Errorf("after: delegate's Bash = %q, want deny", got)
			}
			// A binding to a list nobody locked stays the operator's to retire.
			if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"plain","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Read"]}]}`); code != 201 {
				t.Fatalf("create plain: %d %s", code, body)
			}
			_, body = call(operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"plain","subject_kind":"user","subject_id":"alice"}`)
			plain, _ := jsonField(t, body, "binding_id").(string)
			if code, body := call(delegateBearer, "DELETE", "/v1/_aclbinding/"+plain, ""); code != 200 {
				t.Errorf("delegate retires an unlocked binding: %d %s, want 200", code, body)
			}
		})
	}
}

func TestACLGroup_ADelegateCannotLeaveOrDeleteABoundGroup(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			srv, ts, st := aclMgmtServer(t, mode)
			addDelegateOperator(t, st)
			call := func(bearer, method, path, body string) (int, string) {
				return bearerCall(t, ts, bearer, method, path, body)
			}
			admin := func(method, path, body string) {
				t.Helper()
				sep := "?"
				if strings.Contains(path, "?") {
					sep = "&"
				}
				if code, out := call(legacyAdminBearer, method, path+sep+"tenant=acme", body); code >= 300 {
					t.Fatalf("admin %s %s: %d %s", method, path, code, out)
				}
			}
			admin("POST", "/v1/_acldef", `{"op":"create","name":"admin-deny","rules":`+adminLockRules+`}`)
			// bound holds the delegate directly; outer holds it through inner.
			for _, g := range []string{"bound", "inner", "outer"} {
				admin("POST", "/v1/_groups", `{"name":"`+g+`"}`)
			}
			admin("POST", "/v1/_groups/bound/members", `{"kind":"user","id":"delegate"}`)
			admin("POST", "/v1/_groups/inner/members", `{"kind":"user","id":"delegate"}`)
			admin("POST", "/v1/_groups/outer/members", `{"kind":"group","id":"inner"}`)
			admin("POST", "/v1/_aclbinding", `{"acl":"admin-deny","subject_kind":"group","subject_id":"bound"}`)
			admin("POST", "/v1/_aclbinding", `{"acl":"admin-deny","subject_kind":"group","subject_id":"outer"}`)
			if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
				t.Fatalf("before: delegate's Bash = %q, want deny", got)
			}
			for _, tc := range []struct{ method, path string }{
				{"DELETE", "/v1/_groups/bound/members/user/delegate"},
				{"DELETE", "/v1/_groups/bound"},
				{"DELETE", "/v1/_groups/inner/members/user/delegate"},
				{"DELETE", "/v1/_groups/outer/members/group/inner"},
				{"DELETE", "/v1/_groups/inner"},
				{"DELETE", "/v1/_groups/outer"},
			} {
				if code, body := call(delegateBearer, tc.method, tc.path, ""); code != 403 || !strings.Contains(body, `"acl_denied"`) {
					t.Errorf("delegate %s %s: %d %s, want 403 acl_denied", tc.method, tc.path, code, body)
				}
			}
			deleg := aclCaller(t, srv, "acme", "delegate", auth.ScopeTenant)
			for _, in := range []string{
				`{"op":"remove_member","name":"bound","kind":"user","id":"delegate"}`,
				`{"op":"delete","name":"bound"}`,
			} {
				if res := twinCall(t, srv, deleg, "groupdef", in); !res.IsError || res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryPermission {
					t.Errorf("delegate %s over the twin: %+v, want a permission refusal", in, res)
				}
			}
			if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
				t.Errorf("after: delegate's Bash = %q, want deny", got)
			}
			// A group no locked list is bound to stays the operator's to change.
			admin("POST", "/v1/_groups", `{"name":"free"}`)
			admin("POST", "/v1/_groups/free/members", `{"kind":"user","id":"delegate"}`)
			if code, body := call(delegateBearer, "DELETE", "/v1/_groups/free/members/user/delegate", ""); code != 200 {
				t.Errorf("delegate leaves an unbound group: %d %s, want 200", code, body)
			}
			if code, body := call(delegateBearer, "DELETE", "/v1/_groups/free", ""); code != 200 {
				t.Errorf("delegate deletes an unbound group: %d %s, want 200", code, body)
			}
		})
	}
}

// A tenant-wide config list, for the shadowing tests.
func withConfigDenyList(name string) func(*config.Config) {
	return func(c *config.Config) {
		c.ResolvedACL = &authz.Static{
			ACLs:     map[string]authz.ACL{name: {Name: name, Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}}}},
			Bindings: []authz.StaticBinding{{Tenant: "acme", Binding: authz.Binding{ACL: name, SubjectKind: authz.SubjectTenant}}},
		}
	}
}

const harmlessRules = `[{"effect":"allow","actions":["invoke"],"resources":["tool:Read"]}]`

func TestACLDef_ATenantListCannotShadowAConfigList(t *testing.T) {
	srv, ts, _ := aclMgmtServerWith(t, authz.ModeCompat, withConfigDenyList("corp-yaml"))
	call := func(bearer, method, path, body string) (int, string) {
		return bearerCall(t, ts, bearer, method, path, body)
	}
	if got := aclEvalBash(t, call, aliceBearer); got != "deny" {
		t.Fatalf("before: alice's Bash = %q, want deny", got)
	}
	for _, op := range []string{"create", "put", "update"} {
		code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"`+op+`","name":"corp-yaml","rules":`+harmlessRules+`}`)
		if code != 409 || !strings.Contains(body, `"acl_name_reserved"`) {
			t.Errorf("operator %s corp-yaml: %d %s, want 409 acl_name_reserved", op, code, body)
		}
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	res := twinCall(t, srv, op, "acldef", `{"op":"create","name":"corp-yaml","rules":`+harmlessRules+`}`)
	if !res.IsError || !strings.Contains(res.Text, `"acl_name_reserved"`) || res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryBusiness {
		t.Errorf("operator creates corp-yaml over the twin: %+v, want a business refusal acl_name_reserved", res)
	}
	if got := aclEvalBash(t, call, aliceBearer); got != "deny" {
		t.Errorf("after: alice's Bash = %q, want deny", got)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"corp-local","rules":`+harmlessRules+`}`); code != 201 {
		t.Errorf("a name nobody holds: %d %s, want 201", code, body)
	}
}

// The shared tenant ("") is the one an admin declared in config with no
// tenant acts in: the legacy admin's unfocused tenant is "default", an empty
// ?tenant= is no focus, and a minted token always names a tenant.
func TestACLDef_ATenantListCannotShadowASharedList(t *testing.T) {
	const rootBearer = "lct_root"
	_, ts, _ := aclMgmtServerWith(t, authz.ModeCompat, func(c *config.Config) {
		c.ResolvedPrincipals = []auth.DeclaredPrincipal{{Secret: rootBearer, Principal: auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}}}
	})
	call := func(bearer, method, path, body string) (int, string) {
		return bearerCall(t, ts, bearer, method, path, body)
	}
	deny := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	if code, body := call(rootBearer, "POST", "/v1/_acldef", `{"op":"create","name":"shared-deny","rules":`+deny+`}`); code != 201 || !strings.Contains(body, `"name":"shared-deny"`) || strings.Contains(body, `"tenant_id"`) {
		t.Fatalf("admin creates the shared list: %d %s", code, body)
	}
	if code, body := call(rootBearer, "POST", "/v1/_aclbinding?tenant=acme", `{"acl":"shared-deny","subject_kind":"tenant"}`); code != 201 {
		t.Fatalf("admin binds the shared list tenant-wide in acme: %d %s", code, body)
	}
	if got := aclEvalBash(t, call, aliceBearer); got != "deny" {
		t.Fatalf("before: alice's Bash = %q, want deny", got)
	}
	code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"shared-deny","rules":`+harmlessRules+`}`)
	if code != 409 || !strings.Contains(body, `"acl_name_reserved"`) {
		t.Errorf("operator shadows the shared list: %d %s, want 409 acl_name_reserved", code, body)
	}
	if got := aclEvalBash(t, call, aliceBearer); got != "deny" {
		t.Errorf("after: alice's Bash = %q, want deny", got)
	}
}

// A tenant list that already shadows a config list (written before create
// refused the name) keeps resolving first; boot names each one so the
// operator can rename it, and explain names it beside the decision.
func TestACLBoot_ExistingShadowIsReported(t *testing.T) {
	srv, ts, st := aclMgmtServerWith(t, authz.ModeCompat, withConfigDenyList("corp-yaml"))
	ctx := context.Background()
	for _, row := range []store.AclDefRow{
		{TenantID: "acme", Name: "corp-yaml", Rules: []byte(harmlessRules)},
		{TenantID: "", Name: "corp-yaml", Rules: []byte(harmlessRules)},
		{TenantID: "acme", Name: "unrelated", Rules: []byte(harmlessRules)},
		{TenantID: "other", Name: "gone", Rules: []byte(harmlessRules)},
	} {
		if _, err := st.AclDefPut(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AclDefRetire(ctx, "other", "gone"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n := srv.ReportACLShadows(ctx, slog.New(slog.NewTextHandler(&buf, nil)))
	logged := buf.String()
	if n != 2 || strings.Count(logged, "level=WARN") != 2 ||
		!strings.Contains(logged, "tenant=acme name=corp-yaml") || !strings.Contains(logged, `tenant="" name=corp-yaml`) ||
		strings.Contains(logged, "unrelated") || strings.Contains(logged, "gone") {
		t.Errorf("boot report: %d shadows, log:\n%s\nwant the acme and shared corp-yaml rows only", n, logged)
	}
	code, body := bearerCall(t, ts, aliceBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || !strings.Contains(body, `"shadowed_lists":[{"acl":"corp-yaml","stored_in_tenant":"acme","shadows":"config"}]`) {
		t.Errorf("explain over a shadowing list: %d %s, want it named under shadowed_lists", code, body)
	}
}

// Explain on a group names the lists whose retire governs leaving or
// deleting it: those bound to the group and to every group containing it.
func TestAuthzExplain_NamesTheListsThatGovernAGroup(t *testing.T) {
	_, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	op := func(method, path, body string) {
		t.Helper()
		if code, out := bearerCall(t, ts, operatorBearer, method, path, body); code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, code, out)
		}
	}
	for _, name := range []string{"lock-outer", "lock-inner", "elsewhere"} {
		op("POST", "/v1/_acldef", `{"op":"create","name":"`+name+`","rules":`+harmlessRules+`}`)
	}
	for _, g := range []string{"inner", "outer", "side"} {
		op("POST", "/v1/_groups", `{"name":"`+g+`"}`)
	}
	op("POST", "/v1/_groups/outer/members", `{"kind":"group","id":"inner"}`)
	op("POST", "/v1/_aclbinding", `{"acl":"lock-inner","subject_kind":"group","subject_id":"inner"}`)
	op("POST", "/v1/_aclbinding", `{"acl":"lock-outer","subject_kind":"group","subject_id":"outer"}`)
	op("POST", "/v1/_aclbinding", `{"acl":"elsewhere","subject_kind":"group","subject_id":"side"}`)
	code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"manage"},"resource":{"type":"principal","id":"group:inner"}}`)
	if code != 200 || !strings.Contains(body, `"governed_by":{"group":"inner","retire":["lock-inner","lock-outer"]}`) {
		t.Errorf("explain manage principal:group:inner: %d %s, want governed_by naming lock-inner and lock-outer", code, body)
	}
	code, body = bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || strings.Contains(body, "governed_by") {
		t.Errorf("explain on a tool names no governing lists: %d %s", code, body)
	}
}
