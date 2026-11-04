package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The list lock holds outside the ACL management routes too: deleting,
// re-enabling or erasing a user, the removal policy's own unbind, and
// handing a group to an identity provider all drop bindings or memberships,
// so each answers to the same lock (security review of RFC FY P3).

// lockedFixture is an admin's self-locking admin-deny list bound to the
// delegate, to user victim and to group bound (holding the delegate and
// victim). It returns the server, its test server and store.
func lockedFixture(t *testing.T) (*Server, func(bearer, method, path, body string) (int, string), store.Store) {
	t.Helper()
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	addDelegateOperator(t, st)
	call := func(bearer, method, path, body string) (int, string) {
		return bearerCall(t, ts, bearer, method, path, body)
	}
	admin := func(method, path, body string) {
		t.Helper()
		if code, out := call(legacyAdminBearer, method, path+"?tenant=acme", body); code >= 300 {
			t.Fatalf("admin %s %s: %d %s", method, path, code, out)
		}
	}
	admin("POST", "/v1/_acldef", `{"op":"create","name":"admin-deny","rules":`+adminLockRules+`}`)
	admin("POST", "/v1/_groups", `{"name":"bound"}`)
	for _, u := range []string{"delegate", "victim"} {
		admin("POST", "/v1/_users", `{"subject":"`+u+`"}`)
		admin("POST", "/v1/_groups/bound/members", `{"kind":"user","id":"`+u+`"}`)
		admin("POST", "/v1/_aclbinding", `{"acl":"admin-deny","subject_kind":"user","subject_id":"`+u+`"}`)
	}
	admin("POST", "/v1/_aclbinding", `{"acl":"admin-deny","subject_kind":"group","subject_id":"bound"}`)
	return srv, call, st
}

// liveLocks counts subject's live bindings to admin-deny and its membership
// of bound.
func liveLocks(t *testing.T, st store.Store, subject string) (bindings int, member bool) {
	t.Helper()
	rows, err := st.AclBindingList(context.Background(), "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.ACLName == "admin-deny" && b.SubjectKind == "user" && b.SubjectID == subject {
			bindings++
		}
	}
	groups, err := st.GroupsOfSubject(context.Background(), "acme", subject)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		member = member || g == "bound"
	}
	return bindings, member
}

func TestUserDelete_ADelegateCannotDeleteAUserOutOfALockedList(t *testing.T) {
	_, call, st := lockedFixture(t)
	for _, subject := range []string{"delegate", "victim"} {
		if code, body := call(delegateBearer, "DELETE", "/v1/_users/"+subject, ""); code != 403 || !strings.Contains(body, `"acl_denied"`) {
			t.Errorf("delegate deletes user %s: %d %s, want 403 acl_denied", subject, code, body)
		}
		if b, m := liveLocks(t, st, subject); b != 1 || !m {
			t.Errorf("%s after the refused delete: %d live admin-deny bindings, member of bound %v; want 1 and true", subject, b, m)
		}
	}
	if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
		t.Errorf("after: delegate's Bash = %q, want deny", got)
	}
	if code, body := call(legacyAdminBearer, "DELETE", "/v1/_users/victim?tenant=acme", ""); code != 204 {
		t.Errorf("the admin deletes victim: %d %s, want 204", code, body)
	}
}

func TestUserReenable_ADelegateCannotStripALockedList(t *testing.T) {
	srv, call, st := lockedFixture(t)
	if err := srv.startRemoval(context.Background(), "acme", "victim", "", store.RemovalReasonOperator, "ops"); err != nil {
		t.Fatal(err)
	}
	if code, body := call(delegateBearer, "PATCH", "/v1/_users/victim", `{"status":"active"}`); code != 403 || !strings.Contains(body, `"acl_denied"`) {
		t.Errorf("delegate re-enables a removed victim: %d %s, want 403 acl_denied", code, body)
	}
	if b, m := liveLocks(t, st, "victim"); b != 1 || !m {
		t.Errorf("victim after the refused re-enable: %d live admin-deny bindings, member of bound %v; want 1 and true", b, m)
	}
	if code, body := call(legacyAdminBearer, "PATCH", "/v1/_users/victim?tenant=acme", `{"status":"active"}`); code != 200 {
		t.Errorf("the admin re-enables victim: %d %s, want 200", code, body)
	}
}

// The removal policy's unbind has no acting principal, and a delegate can
// set one going. It keeps a binding or a membership whose list is locked
// against the tenant's operators (a tenant-wide deny on retiring it), and
// still drops the rest.
func TestUserLifecycle_TheUnbindKeepsWhatIsLockedAgainstOperators(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	ctx := context.Background()
	admin := func(method, path, body string) {
		t.Helper()
		if code, out := bearerCall(t, ts, legacyAdminBearer, method, path+"?tenant=acme", body); code >= 300 {
			t.Fatalf("admin %s %s: %d %s", method, path, code, out)
		}
	}
	deny := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	admin("POST", "/v1/_acldef", `{"op":"create","name":"guard","rules":`+deny+`}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"plain","rules":`+harmlessRules+`}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"t-lock","rules":[{"effect":"deny","actions":["author","retire"],"resources":["def:acl/guard"]}]}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"t-lock","subject_kind":"tenant"}`)
	admin("POST", "/v1/_users", `{"subject":"victim"}`)
	for _, g := range []store.GroupRow{{TenantID: "acme", Name: "idp-guarded", ExternalID: "okta:g1"}, {TenantID: "acme", Name: "idp-plain", ExternalID: "okta:g2"}} {
		if _, err := st.GroupPut(ctx, g); err != nil {
			t.Fatal(err)
		}
		if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: g.Name, MemberKind: store.GroupMemberUser, MemberID: "victim"}); err != nil {
			t.Fatal(err)
		}
	}
	admin("POST", "/v1/_aclbinding", `{"acl":"guard","subject_kind":"group","subject_id":"idp-guarded"}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"plain","subject_kind":"group","subject_id":"idp-plain"}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"guard","subject_kind":"user","subject_id":"victim"}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"plain","subject_kind":"user","subject_id":"victim"}`)

	if _, err := srv.unbindSubject(ctx, "acme", "victim", false); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AclBindingList(ctx, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, b := range rows {
		if b.SubjectKind == "user" && b.SubjectID == "victim" {
			live[b.ACLName] = true
		}
	}
	if !live["guard"] || live["plain"] {
		t.Errorf("victim's own bindings after the unbind: %v, want guard kept and plain retired", live)
	}
	groups, err := st.GroupsOfSubject(ctx, "acme", "victim")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(groups, ",") != "idp-guarded" {
		t.Errorf("victim's groups after the unbind: %v, want idp-guarded kept and idp-plain left", groups)
	}
}
