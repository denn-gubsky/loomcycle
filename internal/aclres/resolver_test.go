package aclres

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "acl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func member(subject string) auth.Principal {
	return auth.Principal{TenantID: "acme", Subject: subject, Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, TokenDefID: "def-" + subject}
}

func decide(e *authz.Effective, action, typ, name string) authz.Outcome {
	return e.Decide(authz.Request{Subject: e.Subject, Action: action, Resource: authz.Resource{Type: typ, Name: name, TenantID: e.Subject.TenantID}, Context: authz.Context{Now: time.Now()}}).Outcome
}

func TestClass(t *testing.T) {
	for name, c := range map[string]struct {
		p    auth.Principal
		want string
	}{
		"legacy":   {auth.Principal{Legacy: true, Scopes: []string{auth.ScopeAdmin}}, "admin"},
		"admin":    {auth.Principal{Scopes: []string{auth.ScopeAdmin}}, "admin"},
		"tenant":   {auth.Principal{Scopes: []string{auth.ScopeTenant}}, "tenant"},
		"isolated": {auth.Principal{Scopes: []string{auth.ScopeUser}}, "isolated"},
		"member":   {member("alice"), "member"},
	} {
		if got := Class(c.p); got != c.want {
			t.Errorf("%s: Class = %q, want %q", name, got, c.want)
		}
	}
}

func TestResolve_BaselineByClassAndStoredBindings(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	r := New(st, authz.ModeCompat, 0)
	// A member with no rows: the member baseline alone.
	p, err := r.Resolve(ctx, member("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective == nil || decide(p.Effective, "run", "agent", "chat/x") != authz.Allowed || decide(p.Effective, "mint", "principal", "user:x") != authz.NoMatch {
		t.Fatalf("member baseline: %+v", p.Effective)
	}
	// An isolated user: the shared keyspace is an explicit deny.
	iso, _ := r.Resolve(ctx, auth.Principal{TenantID: "acme", Subject: "sandboxed", Scopes: []string{auth.ScopeUser}})
	if decide(iso.Effective, "read", "memory", "tenant") != authz.Denied {
		t.Error("isolated baseline should deny the tenant keyspace")
	}
	// A run-plane token carries the deny list on top of its class.
	rp, _ := r.Resolve(ctx, auth.Principal{TenantID: "acme", Subject: "ci", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"ci"}}})
	if decide(rp.Effective, "author", "def", "agent/x") != authz.Denied || decide(rp.Effective, "use", "gateway", "chat") != authz.Denied {
		t.Error("run-plane baseline should deny authoring and the gateway")
	}
	// Rows: a group ACL that denies Bash for support, alice in support via a
	// nested group; a per-user ACL that denies an agent.
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-admin-agents", Rules: json.RawMessage(`[{"effect":"deny","actions":["run"],"resources":["agent:admin/**"]}]`)}); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"support", "support-l2"} {
		if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "group", MemberID: "support-l2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support-l2", MemberKind: "user", MemberID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "group", SubjectID: "support"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-admin-agents", SubjectKind: "user", SubjectID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "retired-or-missing", SubjectKind: "tenant"}); err != nil {
		t.Fatal(err)
	}
	p, err = r.Resolve(ctx, member("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Groups; len(got) != 2 || got[0] != "support" || got[1] != "support-l2" {
		t.Errorf("groups = %v", got)
	}
	if decide(p.Effective, "invoke", "tool", "Bash") != authz.Denied || decide(p.Effective, "invoke", "tool", "Read") != authz.Allowed {
		t.Error("the group deny should hold over the member baseline's allow")
	}
	if decide(p.Effective, "run", "agent", "admin/users") != authz.Denied || decide(p.Effective, "run", "agent", "chat/x") != authz.Allowed {
		t.Error("the per-user deny should hold")
	}
	if m := p.Effective.Missing(); len(m) != 1 || m[0] != "retired-or-missing" {
		t.Errorf("missing = %v", m)
	}
	// Bob is in no group: the group deny does not reach him.
	bob, _ := r.Resolve(ctx, member("bob"))
	if decide(bob.Effective, "invoke", "tool", "Bash") != authz.Allowed || len(bob.Groups) != 0 {
		t.Error("bob should not carry support's deny")
	}
	// Another tenant's same-named subject sees none of it.
	other, _ := r.Resolve(ctx, auth.Principal{TenantID: "other", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}})
	if decide(other.Effective, "invoke", "tool", "Bash") != authz.Allowed || len(other.Groups) != 0 {
		t.Error("tenant closure")
	}
	// A principal with no subject is left alone.
	if np, err := r.Resolve(ctx, auth.Principal{TenantID: "acme"}); err != nil || np.Effective != nil {
		t.Errorf("no subject: %+v, %v", np.Effective, err)
	}
}

func TestResolve_CacheFollowsTheTenantVersion(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	r := New(st, authz.ModeCompat, 5*time.Second)
	r.now = func() time.Time { return clock }

	first, _ := r.Resolve(ctx, member("alice"))
	second, _ := r.Resolve(ctx, member("alice"))
	if first.Effective != second.Effective {
		t.Fatal("a repeat within the ttl should return the cached set")
	}
	// A write bumps the version; within the ttl the replica still trusts
	// its cached version...
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "x", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "x", SubjectKind: "tenant"}); err != nil {
		t.Fatal(err)
	}
	third, _ := r.Resolve(ctx, member("alice"))
	if third.Effective != first.Effective {
		t.Error("within the ttl the cached set should still be served")
	}
	// ...until the ttl passes, when the version is re-read and the set rebuilt.
	clock = clock.Add(6 * time.Second)
	fourth, _ := r.Resolve(ctx, member("alice"))
	if fourth.Effective == first.Effective || decide(fourth.Effective, "invoke", "tool", "Bash") != authz.Denied {
		t.Error("after the ttl the new binding should apply")
	}
	// An invalidation (the bus) drops it at once.
	if _, err := st.AclBindingRetire(ctx, "acme", mustFirstBinding(t, st)); err != nil {
		t.Fatal(err)
	}
	fifth, _ := r.Resolve(ctx, member("alice"))
	if fifth.Effective != fourth.Effective {
		t.Error("before an invalidation the cached set is served")
	}
	r.Invalidate("acme")
	sixth, _ := r.Resolve(ctx, member("alice"))
	if sixth.Effective == fourth.Effective || decide(sixth.Effective, "invoke", "tool", "Bash") != authz.Allowed {
		t.Error("after Invalidate the retired binding should be gone")
	}
	// Another tenant's invalidation leaves this one cached.
	r.Invalidate("other")
	if seventh, _ := r.Resolve(ctx, member("alice")); seventh.Effective != sixth.Effective {
		t.Error("another tenant's invalidation dropped this tenant's set")
	}
}

func mustFirstBinding(t *testing.T, st store.Store) string {
	t.Helper()
	rows, err := st.AclBindingList(context.Background(), "acme", false)
	if err != nil || len(rows) == 0 {
		t.Fatalf("bindings: %v %v", rows, err)
	}
	return rows[0].BindingID
}

func TestResolve_StoreFaultReturnsTheErrorAndLeavesThePrincipal(t *testing.T) {
	st := newStore(t)
	_ = st.Close() // every read now fails
	r := New(st, authz.ModeEnforce, 0)
	p, err := r.Resolve(context.Background(), member("alice"))
	if err == nil || p.Effective != nil || p.Groups != nil {
		t.Errorf("want an error and an untouched principal, got %+v, %v", p, err)
	}
}
