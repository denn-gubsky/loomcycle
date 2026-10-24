package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P2, owner answer to Q6: a tenant's bootstrapped owner is bound to the
// built-in tenant-operator list in the same transaction as its user row and
// login, so no owner is ever half-made.

const pwgOwnerACL = "builtin:tenant-operator"

func pwgOwnerBinding(tenant, subject string) store.AclBindingRow {
	return store.AclBindingRow{TenantID: tenant, ACLName: pwgOwnerACL, SubjectKind: "user", SubjectID: subject, CreatedBy: "bootstrap"}
}

func pwgOwnerBound(t *testing.T, s store.Store, tenant, subject string) int {
	t.Helper()
	rows, err := s.AclBindingList(context.Background(), tenant, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.SubjectKind == "user" && r.SubjectID == subject && r.ACLName == pwgOwnerACL {
			n++
		}
	}
	return n
}

func testUserCreateOwnerWritesUserBindingAndMarkerOrNothing(t *testing.T, s store.Store) {
	ctx := context.Background()
	v0, _ := s.AclVersion(ctx, "fresh")
	if err := s.UserCreateOwner(ctx, store.UserRow{TenantID: "fresh", Subject: "boss", AccessMode: "tenant", Status: "active"}, pwgOwnerBinding("fresh", "boss")); err != nil {
		t.Fatalf("owner create: %v", err)
	}
	if _, err := s.UserGet(ctx, "fresh", "boss"); err != nil {
		t.Fatalf("user: %v", err)
	}
	if n := pwgOwnerBound(t, s, "fresh", "boss"); n != 1 {
		t.Fatalf("owner bindings = %d, want 1", n)
	}
	if v1, _ := s.AclVersion(ctx, "fresh"); v1 <= v0 {
		t.Fatalf("acl_version did not move (%d → %d): cached sets would miss the binding", v0, v1)
	}
	if ok, _ := s.TenantBootstrapped(ctx, "fresh"); !ok {
		t.Fatal("no bootstrap marker")
	}
	err := s.UserCreateOwner(ctx, store.UserRow{TenantID: "fresh", Subject: "boss2", AccessMode: "tenant", Status: "active"}, pwgOwnerBinding("fresh", "boss2"))
	if pwgConflictKind(err) != "tenant_not_empty" {
		t.Fatalf("second owner: err = %v, want tenant_not_empty", err)
	}
	if _, err := s.UserGet(ctx, "fresh", "boss2"); !pwgNotFound(err) || pwgOwnerBound(t, s, "fresh", "boss2") != 0 {
		t.Fatal("a refused owner create wrote a user or a binding")
	}
}

func testAuthInviteRedeemWritesTheOwnerBindingOrNothing(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	boot := pwgInvite("inv-own", "code-own", "ownco")
	boot.Bootstrap = true
	if err := s.AuthInviteCreate(ctx, boot); err != nil {
		t.Fatal(err)
	}
	// A taken login fails the redemption: neither the user nor the binding.
	if err := s.UserIdentityCreate(ctx, pwgIdentity("elsewhere", "someone", "taken-login")); err != nil {
		t.Fatal(err)
	}
	red := pwgRedemption("code-own", "ownco", "owner", "taken-login", now)
	b := pwgOwnerBinding("ownco", "owner")
	red.OwnerBinding = &b
	if err := s.AuthInviteRedeem(ctx, red); pwgConflictKind(err) != "user_identity" {
		t.Fatalf("taken login: %v", err)
	}
	if pwgOwnerBound(t, s, "ownco", "owner") != 0 {
		t.Fatal("a failed redemption left the owner binding")
	}
	red = pwgRedemption("code-own", "ownco", "owner", "owner-login", now)
	red.OwnerBinding = &b
	if err := s.AuthInviteRedeem(ctx, red); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if pwgOwnerBound(t, s, "ownco", "owner") != 1 {
		t.Fatal("the redemption did not bind the owner")
	}
}

// Deleting a user unbinds it (RFC FA §3.7c): its user-kind bindings are
// retired and its group memberships removed in the same transaction, so a
// later user given the same subject inherits none of its authority.
func testUserDeleteRetiresTheUsersBindingsAndMemberships(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, sub := range []string{"gone", "stays"} {
		if err := s.UserCreate(ctx, store.UserRow{TenantID: "ut", Subject: sub, AccessMode: "tenant", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "ut", Name: "ops"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"gone", "stays"} {
		if err := s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "ut", GroupName: "ops", MemberKind: store.GroupMemberUser, MemberID: m}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []store.AclBindingRow{pwgOwnerBinding("ut", "gone"), pwgOwnerBinding("ut", "stays"),
		{TenantID: "ut", ACLName: "ops-list", SubjectKind: "group", SubjectID: "gone"}} {
		if _, err := s.AclBindingPut(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	v0, _ := s.AclVersion(ctx, "ut")
	if removed, err := s.UserDelete(ctx, "ut", "gone"); err != nil || !removed {
		t.Fatalf("delete = %v, %v", removed, err)
	}
	if pwgOwnerBound(t, s, "ut", "gone") != 0 {
		t.Fatal("the deleted user's binding is still live")
	}
	all, err := s.AclBindingList(ctx, "ut", true)
	if err != nil {
		t.Fatal(err)
	}
	retired, groupKind := false, false
	for _, r := range all {
		if r.SubjectKind == "user" && r.SubjectID == "gone" && !r.RetiredAt.IsZero() {
			retired = true
		}
		if r.SubjectKind == "group" && r.SubjectID == "gone" && r.RetiredAt.IsZero() {
			groupKind = true
		}
	}
	if !retired {
		t.Fatal("the binding row is not kept as retired")
	}
	if !groupKind {
		t.Fatal("a group-kind binding whose id equals the subject was touched")
	}
	if pwgOwnerBound(t, s, "ut", "stays") != 1 {
		t.Fatal("another user's binding was retired")
	}
	groups, _ := s.GroupsOfSubject(ctx, "ut", "gone")
	if len(groups) != 0 {
		t.Fatalf("the deleted user is still in %v", groups)
	}
	if g, _ := s.GroupsOfSubject(ctx, "ut", "stays"); len(g) != 1 {
		t.Fatalf("another user's membership went: %v", g)
	}
	if v1, _ := s.AclVersion(ctx, "ut"); v1 <= v0 {
		t.Fatal("acl_version did not move")
	}
	// The same subject again starts with nothing.
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "ut", Subject: "gone", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.AclBindingsFor(ctx, "ut", "gone", nil, ""); len(rows) != 0 {
		t.Fatalf("a recreated subject inherits %+v", rows)
	}
	if removed, _ := s.UserDelete(ctx, "ut", "nobody"); removed {
		t.Fatal("deleting a missing user reported a removal")
	}
}
