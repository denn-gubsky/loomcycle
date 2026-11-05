package http

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
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

	if _, err := srv.unbindSubject(ctx, "acme", "victim", false, lifecycleActor); err != nil {
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
// stage is deferred to a later sweep and the binding survives. Another
// subject is erased.
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
		if err := srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Millisecond, EraseAfter: time.Millisecond},
			"acme", u.subject, "", store.RemovalReasonOperator, "delegate"); err != nil {
			t.Fatal(err)
		}
		// The unbind ran (and kept guard): the erase is what is due.
		if ok, err := st.AuthRemovalMarkUnbound(ctx, "acme", u.subject, time.Now()); err != nil || !ok {
			t.Fatalf("unbind %s: %v %v", u.subject, ok, err)
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
	if err := erase("victim"); err != nil {
		t.Errorf("the erase stage on victim: %v, want it deferred without an error", err)
	}
	if row, err := st.AuthRemovalGet(ctx, "acme", "victim"); err != nil || !row.ErasedAt.IsZero() || row.Stage != "" || !row.EraseDueAt.After(time.Now()) {
		t.Errorf("victim's removal after the deferral: %+v %v, want not erased, no stage held, erase due later", row, err)
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

// A deferred erase must not hold the queue: the sweep reads due removals
// oldest first in batches, so a batch of subjects that stay locked ahead of
// an unlocked one would stop every sweep before reaching it. Each deferral
// moves the subject out of the due set until a later sweep re-checks it.
func TestUserLifecycle_LockedSubjectsDoNotStarveTheRemovalQueue(t *testing.T) {
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
	admin("POST", "/v1/_acldef", `{"op":"create","name":"t-lock","rules":[{"effect":"deny","actions":["author","retire"],"resources":["def:acl/guard"]}]}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"t-lock","subject_kind":"tenant"}`)
	policy := lifecycle.Policy{UnbindAfter: time.Millisecond, EraseAfter: time.Millisecond}
	start := time.Now()
	add := func(subject string, locked bool) {
		t.Helper()
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: subject, Status: "active", AccessMode: "isolated", CreatedAt: start}); err != nil {
			t.Fatal(err)
		}
		if locked {
			if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "guard", SubjectKind: "user", SubjectID: subject}); err != nil {
				t.Fatal(err)
			}
		}
		if err := srv.startRemovalWith(ctx, policy, "acme", subject, "", store.RemovalReasonOperator, "delegate"); err != nil {
			t.Fatal(err)
		}
	}
	// More locked subjects than one sweep batch, all older than the free one.
	for i := 0; i <= lifecycleBatch; i++ {
		add(fmt.Sprintf("a-locked-%03d", i), true)
	}
	add("z-free", false)

	// A sweep advances each removal by at most one stage: the first reaches
	// the free subject's unbind past the locked ones' deferred erases, the
	// second its erase.
	now := time.Now().Add(time.Minute)
	srv.AuthLifecycleSweep(ctx, now)
	if row, err := st.AuthRemovalGet(ctx, "acme", "z-free"); err != nil || row.UnboundAt.IsZero() {
		t.Fatalf("the unlocked subject behind %d locked ones after one sweep: %+v %v, want it unbound", lifecycleBatch+1, row, err)
	}
	srv.AuthLifecycleSweep(ctx, now)
	if row, err := st.AuthRemovalGet(ctx, "acme", "z-free"); err != nil || row.ErasedAt.IsZero() {
		t.Fatalf("the unlocked subject after a second sweep: %+v %v, want it erased", row, err)
	}
	locked, err := st.AuthRemovalGet(ctx, "acme", "a-locked-000")
	if err != nil || !locked.ErasedAt.IsZero() || locked.Stage != "" || !locked.EraseDueAt.After(now) {
		t.Fatalf("a locked subject after the sweep: %+v %v, want deferred: not erased, no stage held, erase due later", locked, err)
	}
	// A later sweep re-checks it: still locked, so deferred again, not lost.
	later := locked.EraseDueAt.Add(time.Minute)
	srv.AuthLifecycleSweep(ctx, later)
	again, err := st.AuthRemovalGet(ctx, "acme", "a-locked-000")
	if err != nil || !again.ErasedAt.IsZero() || !again.EraseDueAt.After(later) {
		t.Errorf("a locked subject on a later sweep: %+v %v, want re-checked and deferred again", again, err)
	}
}

// A lock bound to the user itself (not tenant-wide) is invisible to the
// tenant-operator set the removal policy is judged by, so a delegate locked
// into a list could go dormant and have the sweeper unbind it. The unbind
// also asks the removed subject's own set: what it may not retire itself
// survives its removal.
func TestUserLifecycle_TheUnbindKeepsWhatTheSubjectIsLockedInto(t *testing.T) {
	srv, call, st := lockedFixture(t)
	sink := &lockedSink{}
	srv.SetTokenAudit(sink)
	ctx := context.Background()
	// An identity provider's group, so the unbind would drop the membership.
	if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "bound", ExternalID: "okta:bound"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Millisecond},
		"acme", "delegate", "", store.RemovalReasonDormant, lifecycleActor); err != nil {
		t.Fatal(err)
	}
	row, err := st.AuthRemovalGet(ctx, "acme", "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.advanceRemoval(ctx, row, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if row, err := st.AuthRemovalGet(ctx, "acme", "delegate"); err != nil || row.UnboundAt.IsZero() {
		t.Fatalf("delegate's removal after the unbind stage: %+v %v, want unbound", row, err)
	}
	if b, m := liveLocks(t, st, "delegate"); b != 1 || !m {
		t.Errorf("delegate after the unbind: %d live admin-deny bindings, member of bound %v; want 1 and true", b, m)
	}
	if got := aclEvalBash(t, call, delegateBearer); got != "deny" {
		t.Errorf("after the unbind: delegate's Bash = %q, want deny", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	kept := map[string]bool{}
	for _, ev := range sink.events {
		if ev.Action == "auth_unbind_kept_locked" && ev.TargetSubject == "delegate" && ev.TargetName == "def:acl/admin-deny" {
			kept[strings.Fields(ev.AuthReason)[0]] = true
		}
	}
	if !kept["binding"] || !kept["membership"] {
		t.Errorf("audited keeps: %v, want the binding and the membership each recorded", kept)
	}
}

// The erase stage asks the same two sets: a subject locked into a list of
// its own is deferred, not erased out of it.
func TestUserLifecycle_TheEraseStageDefersASubjectLockedIntoItsOwnList(t *testing.T) {
	srv, _, st := lockedFixture(t)
	srv.SetEraseAudit(&recordingSink{})
	ctx := context.Background()
	if err := srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Millisecond, EraseAfter: time.Millisecond},
		"acme", "delegate", "", store.RemovalReasonDormant, lifecycleActor); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AuthRemovalMarkUnbound(ctx, "acme", "delegate", time.Now()); err != nil || !ok {
		t.Fatalf("unbind: %v %v", ok, err)
	}
	row, err := st.AuthRemovalGet(ctx, "acme", "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.eraseRemoved(ctx, row, time.Now()); err != nil {
		t.Fatalf("the erase stage: %v, want it deferred without an error", err)
	}
	if row, err := st.AuthRemovalGet(ctx, "acme", "delegate"); err != nil || !row.ErasedAt.IsZero() || !row.EraseDueAt.After(time.Now()) {
		t.Errorf("delegate's removal after the erase stage: %+v %v, want deferred", row, err)
	}
	if b, m := liveLocks(t, st, "delegate"); b != 1 || !m {
		t.Errorf("delegate after the erase stage: %d live admin-deny bindings, member of bound %v; want 1 and true", b, m)
	}
}

// An operator's re-enable is judged by the caller's own lock
// (aclSubjectLockForCaller), never by the unattended steps' keep: an admin
// (who is not held by a lock) re-enables at the new-user floor even over a
// list locked against the tenant's operators, while a delegate the lock
// holds is refused.
func TestUserReenable_AnAdminResetsToTheFloorOverATenantLock(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	addDelegateOperator(t, st)
	ctx := context.Background()
	admin := func(method, path, body string) (int, string) {
		t.Helper()
		return bearerCall(t, ts, legacyAdminBearer, method, path+"?tenant=acme", body)
	}
	allow := `[{"effect":"allow","actions":["invoke"],"resources":["tool:Bash"]}]`
	for _, req := range []struct{ path, body string }{
		{"/v1/_acldef", `{"op":"create","name":"perk","rules":` + allow + `}`},
		{"/v1/_acldef", `{"op":"create","name":"t-lock","rules":[{"effect":"deny","actions":["author","retire"],"resources":["def:acl/perk"]}]}`},
		{"/v1/_aclbinding", `{"acl":"t-lock","subject_kind":"tenant"}`},
		{"/v1/_users", `{"subject":"victim"}`},
		{"/v1/_aclbinding", `{"acl":"perk","subject_kind":"user","subject_id":"victim"}`},
	} {
		if code, out := admin("POST", req.path, req.body); code >= 300 {
			t.Fatalf("admin %s: %d %s", req.path, code, out)
		}
	}
	if err := srv.startRemoval(ctx, "acme", "victim", "", store.RemovalReasonOperator, "ops"); err != nil {
		t.Fatal(err)
	}
	perk := func() int {
		t.Helper()
		rows, err := st.AclBindingList(ctx, "acme", false)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, b := range rows {
			if b.ACLName == "perk" && b.SubjectKind == "user" && b.SubjectID == "victim" {
				n++
			}
		}
		return n
	}
	if code, body := bearerCall(t, ts, delegateBearer, "PATCH", "/v1/_users/victim", `{"status":"active"}`); code != 403 || !strings.Contains(body, `"acl_denied"`) {
		t.Errorf("a delegate re-enables victim over the tenant lock: %d %s, want 403 acl_denied", code, body)
	}
	if n := perk(); n != 1 {
		t.Errorf("victim's perk bindings after the refused re-enable: %d, want 1", n)
	}
	if code, body := admin("PATCH", "/v1/_users/victim", `{"status":"active"}`); code != 200 {
		t.Fatalf("the admin re-enables victim: %d %s, want 200", code, body)
	}
	if n := perk(); n != 0 {
		t.Errorf("victim's perk bindings after the admin's re-enable: %d, want 0 (the new-user floor)", n)
	}
}

// Deprovisioning keeps only what can only take away (a list with a deny
// rule and no allow) and was locked on purpose. The removed subject's own
// set locks a list only by a rule naming that exact list, never by what it
// denies the subject as an actor, so a broad `retire def:*` or
// `retire def:acl/*` the user carries locks nothing; an allow list goes
// even when locked exactly.
func TestUserLifecycle_TheUnbindDropsWhatIsNotAnExactlyLockedDenyList(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	ctx := context.Background()
	admin := func(method, path, body string) {
		t.Helper()
		if code, out := bearerCall(t, ts, legacyAdminBearer, method, path+"?tenant=acme", body); code >= 300 {
			t.Fatalf("admin %s %s: %d %s", method, path, code, out)
		}
	}
	deny := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	allow := `[{"effect":"allow","actions":["invoke"],"resources":["tool:Bash"]}]`
	admin("POST", "/v1/_acldef", `{"op":"create","name":"guard","rules":`+deny+`}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"wild-guard","rules":`+deny+`}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"perk","rules":`+allow+`}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"broad","rules":[{"effect":"deny","actions":["retire"],"resources":["def:*"]}]}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"self-wild","rules":[{"effect":"deny","actions":["retire"],"resources":["def:acl/*"]}]}`)
	// Tenant-wide: guard and perk locked by name.
	admin("POST", "/v1/_acldef", `{"op":"create","name":"t-lock","rules":[`+
		`{"effect":"deny","actions":["retire"],"resources":["def:acl/guard","def:acl/perk"]}]}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"t-lock","subject_kind":"tenant"}`)
	for _, u := range []string{"victim", "other"} {
		admin("POST", "/v1/_users", `{"subject":"`+u+`"}`)
	}
	for _, list := range []string{"guard", "wild-guard", "perk", "broad", "self-wild"} {
		admin("POST", "/v1/_aclbinding", `{"acl":"`+list+`","subject_kind":"user","subject_id":"victim"}`)
	}
	for _, list := range []string{"wild-guard", "perk", "broad", "self-wild"} {
		admin("POST", "/v1/_aclbinding", `{"acl":"`+list+`","subject_kind":"user","subject_id":"other"}`)
	}

	if _, err := srv.unbindSubject(ctx, "acme", "victim", false, lifecycleActor); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AclBindingList(ctx, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	for _, b := range rows {
		if b.SubjectKind == "user" && b.SubjectID == "victim" {
			live = append(live, b.ACLName)
		}
	}
	if strings.Join(live, ",") != "guard" {
		t.Errorf("victim's bindings after the unbind: %v, want only guard (wild-guard, perk, broad and self-wild retired)", live)
	}
	// The erase stage reads the same rule: other carries nothing it keeps.
	if list, err := srv.lifecycleEraseLock(ctx, "acme", "other"); err != nil || list != "" {
		t.Errorf("the erase lock on other: %q %v, want none (no exactly locked deny list)", list, err)
	}
}

// A lock whose deny carries a condition the unattended step cannot answer
// (the sweeper has no transport, source address, agent or tier) holds: a
// condition that could not be checked must not unlock the list.
func TestUserLifecycle_AConditionalLockHoldsForTheUnattendedUnbind(t *testing.T) {
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
	admin("POST", "/v1/_acldef", `{"op":"create","name":"t-lock","rules":[`+
		`{"effect":"deny","actions":["retire"],"resources":["def:acl/guard"],"when":{"transport":["http"]}}]}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"t-lock","subject_kind":"tenant"}`)
	admin("POST", "/v1/_users", `{"subject":"victim"}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"guard","subject_kind":"user","subject_id":"victim"}`)

	if _, err := srv.unbindSubject(ctx, "acme", "victim", false, lifecycleActor); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AclBindingList(ctx, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, b := range rows {
		kept = kept || (b.SubjectID == "victim" && b.ACLName == "guard")
	}
	if !kept {
		t.Error("victim's guard binding is gone after the unbind; want it kept (its lock names a transport the sweeper has none of)")
	}
}

// The tenant-operator set locks a list by any rule, a pattern included: an
// admin who locks a family of lists against the tenant's operators
// (`retire def:acl/corp-*`) keeps each of them through a removal.
func TestUserLifecycle_AnOperatorPatternLockKeepsAListThroughRemoval(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	ctx := context.Background()
	admin := func(method, path, body string) {
		t.Helper()
		if code, out := bearerCall(t, ts, legacyAdminBearer, method, path+"?tenant=acme", body); code >= 300 {
			t.Fatalf("admin %s %s: %d %s", method, path, code, out)
		}
	}
	admin("POST", "/v1/_acldef", `{"op":"create","name":"corp-deny","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}`)
	admin("POST", "/v1/_acldef", `{"op":"create","name":"corp-lock","rules":[{"effect":"deny","actions":["author","retire"],"resources":["def:acl/corp-*"]}]}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"corp-lock","subject_kind":"tenant"}`)
	admin("POST", "/v1/_users", `{"subject":"victim"}`)
	admin("POST", "/v1/_aclbinding", `{"acl":"corp-deny","subject_kind":"user","subject_id":"victim"}`)
	if err := srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Millisecond},
		"acme", "victim", "", store.RemovalReasonDormant, lifecycleActor); err != nil {
		t.Fatal(err)
	}
	row, err := st.AuthRemovalGet(ctx, "acme", "victim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.advanceRemoval(ctx, row, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AclBindingList(ctx, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, b := range rows {
		kept = kept || (b.SubjectID == "victim" && b.ACLName == "corp-deny")
	}
	if !kept {
		t.Error("victim's corp-deny binding is gone after the dormant removal's unbind; want it kept by the corp-* lock")
	}
}

// An identity provider's group sync takes a user out of the gate's groups
// with no operator behind it; the user may sway the provider. It keeps a
// membership whose group carries only deny lists, one of them locked, as
// the removal policy does, and still drops the rest.
func TestAuthGateSync_KeepsAMembershipWhoseDenyListIsLocked(t *testing.T) {
	srv, _, st := lockedFixture(t)
	ctx := context.Background()
	gate := config.AuthGate{Name: "okta", Identity: &config.AuthIdentityMap{SyncGroups: true, Trusted: true}}
	for _, g := range []store.GroupRow{
		{TenantID: "acme", Name: "bound", ExternalID: "okta:bound"},
		{TenantID: "acme", Name: "idp-plain", ExternalID: "okta:plain"},
	} {
		if _, err := st.GroupPut(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "idp-plain", MemberKind: store.GroupMemberUser, MemberID: "delegate"}); err != nil {
		t.Fatal(err)
	}
	_, removed, err := srv.syncGroups(ctx, gate, "acme", "delegate", nil)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := st.GroupsOfSubject(ctx, "acme", "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(groups, ",") != "bound" || strings.Join(removed, ",") != "idp-plain" {
		t.Errorf("delegate's groups after a sync asserting none: %v (removed %v), want bound kept and idp-plain removed", groups, removed)
	}
}
