package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// delegateBearer is a second tenant operator of acme whose authority an ACL
// list narrows: the "delegated writer" the list-write checks exist for.
const delegateBearer = "tok-delegate"

func addDelegateOperator(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_deleg", Name: "u-def_deleg", TenantID: "acme", Subject: "delegate",
		TokenHash: auth.HashToken(testTokenPepper, delegateBearer), AllowedScopes: []string{auth.ScopeTenant},
	}); err != nil {
		t.Fatal(err)
	}
}

// A list write asks author (create/update/put) or retire of the named list,
// not the route's read: the op is in the body, so the route gate cannot tell
// a list from a create. An operator whose ACL denies authoring lists could
// write them before.
func TestACLDef_WritesNeedAuthorAndRetireNotRead(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) { aclDefWritesNeedAuthor(t, mode) })
	}
}

func aclDefWritesNeedAuthor(t *testing.T, mode authz.Mode) {
	ts, st := aclMgmtFixture(t, mode)
	addDelegateOperator(t, st)
	call := func(bearer, body string) (int, string) {
		return bearerCall(t, ts, bearer, "POST", "/v1/_acldef", body)
	}
	rules := `[{"effect":"deny","actions":["author","retire"],"resources":["def:acl/*"]},{"effect":"allow","actions":["read"],"resources":["def:acl/*"]}]`
	if code, body := call(operatorBearer, `{"op":"create","name":"acl-readers","rules":`+rules+`}`); code != 201 {
		t.Fatalf("create the readers list: %d %s", code, body)
	}
	other := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	if code, body := call(operatorBearer, `{"op":"create","name":"no-shell","rules":`+other+`}`); code != 201 {
		t.Fatalf("create a list: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"acl-readers","subject_kind":"user","subject_id":"delegate"}`); code != 201 {
		t.Fatalf("bind the delegate: %d %s", code, body)
	}

	for _, tc := range []struct{ body, action string }{
		{`{"op":"create","name":"wider","rules":` + other + `}`, "author"},
		{`{"op":"put","name":"no-shell","rules":` + other + `}`, "author"},
		{`{"op":"update","name":"no-shell","rules":` + other + `}`, "author"},
		{`{"op":"retire","name":"no-shell"}`, "retire"},
		// An op the handler does not know is asked author, not let through:
		// the check fails closed for a write op added later.
		{`{"op":"fork","name":"no-shell"}`, "author"},
	} {
		code, body := call(delegateBearer, tc.body)
		if code != 403 || !strings.Contains(body, `"acl_denied"`) || !strings.Contains(body, `"action":"`+tc.action+`"`) {
			t.Errorf("%s as a delegate whose ACL denies it: %d %s, want 403 acl_denied on %s", tc.body, code, body, tc.action)
		}
	}
	for _, body := range []string{`{"op":"get","name":"no-shell"}`, `{"op":"list"}`, `{"op":"validate","rules":` + other + `}`, `{"op":"catalog"}`} {
		if code, out := call(delegateBearer, body); code != 200 {
			t.Errorf("%s as a delegate who may read lists: %d %s, want 200", body, code, out)
		}
	}
	// The list was not changed by the refused writes.
	if code, body := call(operatorBearer, `{"op":"get","name":"no-shell"}`); code != 200 || !strings.Contains(body, `"version":1`) {
		t.Errorf("no-shell after refused writes: %d %s", code, body)
	}
}

// A tenant operator holds every ACL, group and binding authority in its
// tenant (RFC EY §3.8), so a list bound to it, even tenant-wide, is its to
// change: the operator who wrote "deny Bash for everyone" must be able to edit
// it. What holds an operator back is a deny on authoring the list, which the
// per-op check honours.
func TestACLDef_AnOperatorChangesListsThatBindIt_UnlessDeniedAuthoring(t *testing.T) {
	ts, st := aclMgmtFixture(t, authz.ModeCompat)
	addDelegateOperator(t, st)
	call := func(bearer, body string) (int, string) {
		return bearerCall(t, ts, bearer, "POST", "/v1/_acldef", body)
	}
	bind := func(acl, kind, id string) {
		t.Helper()
		body := `{"acl":"` + acl + `","subject_kind":"` + kind + `"`
		if id != "" {
			body += `,"subject_id":"` + id + `"`
		}
		if code, out := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", body+`}`); code != 201 {
			t.Fatalf("bind %s to %s %s: %d %s", acl, kind, id, code, out)
		}
	}
	noShell := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	open := `[{"effect":"allow","actions":["invoke"],"resources":["tool:Bash"]}]`
	for _, name := range []string{"everyone", "delegate-set"} {
		if code, body := call(operatorBearer, `{"op":"create","name":"`+name+`","rules":`+noShell+`}`); code != 201 {
			t.Fatalf("create %s: %d %s", name, code, body)
		}
	}
	bind("everyone", "tenant", "")
	if code, body := call(operatorBearer, `{"op":"put","name":"everyone","rules":`+open+`}`); code != 201 {
		t.Errorf("an operator edits the tenant-wide list it is bound by: %d %s, want 201", code, body)
	}
	// The delegate is bound to its own list, and to a deny on authoring it.
	lock := `[{"effect":"deny","actions":["author","retire"],"resources":["def:acl/delegate-set"]}]`
	if code, body := call(operatorBearer, `{"op":"create","name":"lock","rules":`+lock+`}`); code != 201 {
		t.Fatalf("create lock: %d %s", code, body)
	}
	bind("delegate-set", "user", "delegate")
	bind("lock", "user", "delegate")
	for _, body := range []string{`{"op":"put","name":"delegate-set","rules":` + open + `}`, `{"op":"retire","name":"delegate-set"}`} {
		if code, out := call(delegateBearer, body); code != 403 || !strings.Contains(out, `"acl_denied"`) {
			t.Errorf("the delegate lifts its own list against a deny: %s → %d %s, want 403 acl_denied", body, code, out)
		}
	}
}

// A writer that holds only what an ACL delegates to it (an auth gate's
// token, RFC EY §3.8) may not change a list that shapes its own set: one
// bound to its subject, its token, a group it is in or its tenant, nor create
// a name already bound to it. Today the management route is operator-only, so
// the guard is exercised directly; it is the one the route will run.
func TestACLListNotSelfBound_RefusesADelegatedWritersOwnLists(t *testing.T) {
	deny := authz.ACL{Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}}}
	acls := map[string]authz.ACLVersion{}
	for _, n := range []string{"by-user", "by-group", "by-token", "by-tenant", "other-user", "unrelated"} {
		a := deny
		a.Name = n
		acls[n] = authz.ACLVersion{ACL: a, Version: 1}
	}
	subject := authz.Subject{TenantID: "acme", ID: "gate", Groups: []string{"gates"}, TokenDefID: "def_gate"}
	eff, err := authz.Compile(authz.Input{Subject: subject, ACLs: acls, Now: time.Now(), Bindings: []authz.Binding{
		{ACL: "by-user", SubjectKind: authz.SubjectUser, SubjectID: "gate"},
		{ACL: "by-group", SubjectKind: authz.SubjectGroup, SubjectID: "gates"},
		{ACL: "by-token", SubjectKind: authz.SubjectToken, SubjectID: "def_gate"},
		{ACL: "by-tenant", SubjectKind: authz.SubjectTenant},
		{ACL: "other-user", SubjectKind: authz.SubjectUser, SubjectID: "someone-else"},
		{ACL: "not-yet", SubjectKind: authz.SubjectUser, SubjectID: "gate"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	gate := auth.Principal{TenantID: "acme", Subject: "gate", Groups: []string{"gates"}, TokenDefID: "def_gate",
		Scopes: []string{auth.ScopeRunsCreate}, Effective: eff}
	for _, name := range []string{"by-user", "by-group", "by-token", "by-tenant", "not-yet"} {
		if err := aclListNotSelfBound(gate, name); err == nil {
			t.Errorf("a delegated writer changes %q, which binds it: allowed, want refused", name)
		}
	}
	for _, name := range []string{"other-user", "unrelated", "brand-new"} {
		if err := aclListNotSelfBound(gate, name); err != nil {
			t.Errorf("a delegated writer changes %q, which does not bind it: %v", name, err)
		}
	}
	for _, holder := range []auth.Principal{
		{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}, Effective: eff},
		{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}, Effective: eff},
		{Legacy: true, Effective: eff},
	} {
		if err := aclListNotSelfBound(holder, "by-tenant"); err != nil {
			t.Errorf("%+v holds the tenant's authority and may change by-tenant: %v", holder.Scopes, err)
		}
	}
}
