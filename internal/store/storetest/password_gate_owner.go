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
