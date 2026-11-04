package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
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

func TestErasure_ADelegateCannotEraseAUserOutOfALockedList(t *testing.T) {
	srv, call, st := lockedFixture(t)
	srv.SetEraseAudit(&recordingSink{})
	for _, subject := range []string{"delegate", "victim"} {
		for _, body := range []string{`{"subject":"` + subject + `","dry_run":true}`, `{"subject":"` + subject + `","dry_run":false,"confirm":"` + subject + `"}`} {
			if code, out := call(delegateBearer, "POST", "/v1/_erasure", body); code != 403 {
				t.Errorf("delegate erases %s: %s → %d %s, want 403", subject, body, code, out)
			}
		}
		if b, m := liveLocks(t, st, subject); b != 1 || !m {
			t.Errorf("%s after the refused erasure: %d live admin-deny bindings, member of bound %v; want 1 and true", subject, b, m)
		}
	}
	if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
		t.Errorf("after: delegate's Bash = %q, want deny", got)
	}
	if code, out := call(legacyAdminBearer, "POST", "/v1/_erasure?tenant=acme", `{"subject":"victim","dry_run":true}`); code != 200 {
		t.Errorf("the admin's dry run: %d %s, want 200", code, out)
	}
}

// Setting external_id hands a group's membership to an identity provider:
// its sync and the removal policy then drop members, and local edits are
// refused. On a group a locked list is bound to, that is a lift.
func TestACLGroup_ADelegateCannotHandALockedGroupToAnIdentityProvider(t *testing.T) {
	_, call, st := lockedFixture(t)
	if code, body := call(delegateBearer, "POST", "/v1/_groups", `{"name":"bound","external_id":"okta:x"}`); code != 403 || !strings.Contains(body, `"acl_denied"`) {
		t.Errorf("delegate sets external_id on the locked group: %d %s, want 403 acl_denied", code, body)
	}
	g, err := st.GroupGet(context.Background(), "acme", "bound")
	if err != nil || g.ExternalID != "" {
		t.Errorf("bound after the refused upsert: %+v %v, want no external_id", g, err)
	}
	if code, body := call(delegateBearer, "POST", "/v1/_groups", `{"name":"bound","description":"renamed"}`); code != 201 {
		t.Errorf("delegate edits the description only: %d %s, want 201", code, body)
	}
	if code, body := call(delegateBearer, "POST", "/v1/_groups", `{"name":"free","external_id":"okta:y"}`); code != 201 {
		t.Errorf("delegate creates an IdP group no list is bound to: %d %s, want 201", code, body)
	}
}

// Whether explain adds its list notes is asked without recording: an
// operator denied reading lists gets no acl_denied audit row per explain.
func TestAuthzExplain_TheNotesGateRecordsNothing(t *testing.T) {
	srv, ts, st := aclMgmtServerWith(t, authz.ModeCompat, withConfigDenyList("corp-yaml"))
	addDelegateOperator(t, st)
	sink := &lockedSink{}
	srv.SetTokenAudit(sink)
	for _, req := range []struct{ path, body string }{
		{"/v1/_acldef", `{"op":"create","name":"no-list-reads","rules":[{"effect":"deny","actions":["read"],"resources":["def:acl/*"]}]}`},
		{"/v1/_aclbinding", `{"acl":"no-list-reads","subject_kind":"user","subject_id":"delegate"}`},
	} {
		if code, out := bearerCall(t, ts, operatorBearer, "POST", req.path, req.body); code >= 300 {
			t.Fatalf("%s: %d %s", req.path, code, out)
		}
	}
	before := len(sink.events)
	code, body := bearerCall(t, ts, delegateBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || strings.Contains(body, "shadowed_lists") {
		t.Fatalf("explain as a delegate denied list reads: %d %s", code, body)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, ev := range sink.events[before:] {
		if strings.HasPrefix(ev.Action, "acl_") {
			t.Errorf("explain recorded %s on %s %s; the notes gate must record nothing", ev.Action, ev.AclAction, ev.TargetName)
		}
	}
}

// The shared-tenant branch of the reserved-name check: an admin acting in
// the shared tenant may not store a list a config list's name either.
func TestACLDef_ASharedListCannotShadowAConfigList(t *testing.T) {
	const rootBearer = "lct_root"
	_, ts, _ := aclMgmtServerWith(t, authz.ModeCompat, func(c *config.Config) {
		withConfigDenyList("corp-yaml")(c)
		c.ResolvedPrincipals = []auth.DeclaredPrincipal{{Secret: rootBearer, Principal: auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}}}
	})
	code, body := bearerCall(t, ts, rootBearer, "POST", "/v1/_acldef", `{"op":"create","name":"corp-yaml","rules":`+harmlessRules+`}`)
	if code != 409 || !strings.Contains(body, `"acl_name_reserved"`) {
		t.Errorf("an admin in the shared tenant creates corp-yaml: %d %s, want 409 acl_name_reserved", code, body)
	}
}

// The removal policy's erase stage has no caller either, and a delegate can
// set a removal going. Erasure is all or nothing, so a subject carrying a
// list locked against the tenant's operators is not erased this pass: the
// stage is deferred and the binding survives. Another subject is erased.
func TestUserLifecycle_TheEraseStageDefersASubjectWithALockedList(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	srv.SetEraseAudit(&recordingSink{})
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
	for _, u := range []struct{ subject, list string }{{"victim", "guard"}, {"other", "plain"}} {
		admin("POST", "/v1/_users", `{"subject":"`+u.subject+`"}`)
		admin("POST", "/v1/_aclbinding", `{"acl":"`+u.list+`","subject_kind":"user","subject_id":"`+u.subject+`"}`)
		if err := srv.startRemoval(ctx, "acme", u.subject, "", store.RemovalReasonOperator, "delegate"); err != nil {
			t.Fatal(err)
		}
	}
	erase := func(subject string) error {
		t.Helper()
		row, err := st.AuthRemovalGet(ctx, "acme", subject)
		if err != nil {
			t.Fatal(err)
		}
		_, err = srv.eraseRemoved(ctx, row, time.Now())
		return err
	}
	if err := erase("victim"); !errors.Is(err, erasure.ErrLocked) {
		t.Errorf("the erase stage on victim: %v, want it deferred with erasure.ErrLocked", err)
	}
	rows, err := st.AclBindingList(ctx, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, b := range rows {
		kept = kept || (b.ACLName == "guard" && b.SubjectID == "victim")
	}
	if !kept {
		t.Error("victim's guard binding is gone after the erase stage; want it kept")
	}
	if err := erase("other"); err != nil {
		t.Errorf("the erase stage on a subject with no locked list: %v, want it erased", err)
	}
}
