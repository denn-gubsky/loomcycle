package http

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
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
