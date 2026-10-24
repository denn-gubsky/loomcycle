package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// floorFixture is a tenant with the floor groups (everyone, new-users) and a
// staff group, and one fresh floor member per test case.
func floorFixture(t *testing.T) *gateFixture {
	t.Helper()
	f := newGateFixture(t)
	for _, g := range []string{"everyone", "new-users", "staff"} {
		if _, err := f.st.GroupPut(context.Background(), store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *gateFixture) floorMember(t *testing.T, subject string) {
	t.Helper()
	f.addUser(t, "acme", subject, "isolated")
	for _, g := range []string{"everyone", "new-users"} {
		f.joinGroup(t, g, store.GroupMemberUser, subject)
	}
}

func (f *gateFixture) joinGroup(t *testing.T, group, kind, id string) {
	t.Helper()
	if err := f.st.GroupMemberAdd(context.Background(), store.GroupMemberRow{TenantID: "acme", GroupName: group, MemberKind: kind, MemberID: id}); err != nil {
		t.Fatal(err)
	}
}

func (f *gateFixture) bind(t *testing.T, kind, id string) {
	t.Helper()
	if _, err := f.st.AclBindingPut(context.Background(), store.AclBindingRow{TenantID: "acme", ACLName: "ops", SubjectKind: kind, SubjectID: id}); err != nil {
		t.Fatal(err)
	}
}

func (f *gateFixture) above(t *testing.T, subject string) bool {
	t.Helper()
	got, _, err := f.srv.aboveFloor(context.Background(), "acme", subject, nil)
	if err != nil {
		t.Fatalf("aboveFloor(%s): %v", subject, err)
	}
	return got
}

// A fresh floor member — isolated, in the floor groups, maybe holding an
// isolated user token, with a binding on a floor group and a tenant-wide one —
// is AT the floor; each authority source alone puts a user above it.
func TestAboveFloor_FloorMemberIsAtTheFloorAndEachAuthoritySourceRaisesIt(t *testing.T) {
	f := floorFixture(t)
	f.floorMember(t, "fresh")
	seedToken(t, f.st, "fresh-user-token", "acme", "fresh", []string{auth.ScopeUser}, time.Time{})
	f.bind(t, string(authz.SubjectGroup), "new-users")
	f.bind(t, string(authz.SubjectTenant), "")
	if f.above(t, "fresh") {
		t.Fatal("a fresh floor member reads as above the floor")
	}

	cases := []struct {
		name, subject string
		raise         func(subject string)
	}{
		{"access_mode tenant", "u-mode", func(s string) {
			mode := "tenant"
			if err := f.st.UserUpdate(context.Background(), "acme", s, store.UserPatch{AccessMode: &mode}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a non-floor group", "u-group", func(s string) { f.joinGroup(t, "staff", store.GroupMemberUser, s) }},
		{"a user-kind ACL binding", "u-binding", func(s string) { f.bind(t, string(authz.SubjectUser), s) }},
		{"a token beyond the floor", "u-token", func(s string) {
			seedToken(t, f.st, s+"-tenant-token", "acme", s, []string{auth.ScopeTenant}, time.Time{})
		}},
		{"a token-kind ACL binding", "u-tokbind", func(s string) {
			seedToken(t, f.st, s+"-bound-token", "acme", s, []string{auth.ScopeUser}, time.Time{})
			f.bind(t, string(authz.SubjectToken), "def_"+s)
		}},
	}
	for _, c := range cases {
		f.floorMember(t, c.subject)
		if f.above(t, c.subject) {
			t.Fatalf("%s: above the floor before it was raised", c.name)
		}
		c.raise(c.subject)
		if !f.above(t, c.subject) {
			t.Errorf("%s alone does not put a user above the floor", c.name)
		}
	}
}

// Nesting: a floor group placed inside a privileged group lifts its members,
// since the engine closes over nesting.
func TestAboveFloor_CountsAGroupReachedOnlyThroughNesting(t *testing.T) {
	f := floorFixture(t)
	f.floorMember(t, "nested")
	if f.above(t, "nested") {
		t.Fatal("above before nesting")
	}
	f.joinGroup(t, "staff", store.GroupMemberGroup, "new-users")
	if !f.above(t, "nested") {
		t.Fatal("membership of staff through new-users is not counted")
	}
}

// Config-declared groups count exactly as the resolver counts them.
func TestAboveFloor_CountsAConfigDeclaredGroupMembership(t *testing.T) {
	f := floorFixture(t)
	f.floorMember(t, "declared")
	res := aclres.New(f.st, authz.ModeCompat, 0)
	static := &authz.Static{Groups: []authz.StaticGroup{{Tenant: "acme", Name: "cfg-admins", Members: []string{"declared"}}}}
	f.srv.SetACLResolver(res)
	// After SetACLResolver, which wires the static source from config.
	res.SetStatic(func() *authz.Static { return static })
	if !f.above(t, "declared") {
		t.Fatal("a config-declared group membership is not counted")
	}
}

// Drift guard: whatever the resolver applies to a subject beyond the tenant's
// and the floor groups' bindings, the predicate counts.
func TestAboveFloor_AgreesWithTheResolversAppliedBindings(t *testing.T) {
	f := floorFixture(t)
	res := aclres.New(f.st, authz.ModeCompat, 0)
	f.srv.SetACLResolver(res)
	f.floorMember(t, "plain")
	f.floorMember(t, "nested")
	f.joinGroup(t, "staff", store.GroupMemberGroup, "new-users")
	f.bind(t, string(authz.SubjectGroup), "staff")
	for _, subject := range []string{"plain", "nested"} {
		p, err := res.Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: subject, Scopes: []string{auth.ScopeUser}})
		if err != nil {
			t.Fatal(err)
		}
		extra := false
		for _, b := range p.Effective.Applied() {
			if !authz.IsBuiltin(b.ACL) && b.SubjectKind != authz.SubjectTenant && !(b.SubjectKind == authz.SubjectGroup && (b.SubjectID == "everyone" || b.SubjectID == "new-users")) {
				extra = true
			}
		}
		if got := f.above(t, subject); got != extra {
			t.Errorf("%s: aboveFloor = %v, the resolver applies extra bindings = %v", subject, got, extra)
		}
	}
}

// failingGroupsStore makes the resolver's group read fail.
type failingGroupsStore struct{ store.Store }

func (failingGroupsStore) GroupsOfSubject(context.Context, string, string) ([]string, error) {
	return nil, errors.New("store down")
}

// A store error reads as above the floor: the website's credential is
// refused, recovery included, rather than let through on a guess.
func TestAboveFloor_AStoreErrorRefusesTheManager(t *testing.T) {
	f := floorFixture(t)
	f.floorMember(t, "flaky")
	f.addLogin(t, "acme", "flaky", "cloud", "flaky@acme.test")
	f.srv.store = failingGroupsStore{f.st}
	if got, _, err := f.srv.aboveFloor(context.Background(), "acme", "flaky", nil); !got || err == nil {
		t.Fatalf("aboveFloor = (%v, %v), want (true, error)", got, err)
	}
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/cloud/resets", `{"tenant":"acme","subject":"flaky"}`); code != http.StatusForbidden {
		t.Fatalf("reset on a store error, recovery gate: %d %v, want 403", code, b)
	}
}
