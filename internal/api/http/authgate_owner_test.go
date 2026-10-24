package http

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ownerBindings lists the subject's live user-kind bindings to the built-in
// tenant-operator list.
func (f *gateFixture) ownerBindings(t *testing.T, tenant, subject string) []store.AclBindingRow {
	t.Helper()
	rows, err := f.st.AclBindingList(context.Background(), tenant, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AclBindingRow
	for _, r := range rows {
		if r.SubjectKind == string(authz.SubjectUser) && r.SubjectID == subject && r.ACLName == authz.BaselineTenantOperator {
			out = append(out, r)
		}
	}
	return out
}

// Owner answer to Q6: a tenant's bootstrapped owner signs in as its tenant
// operator. Redeeming the owner invite binds the new user to the built-in
// tenant-operator list in its tenant (audited auth_owner_bound), and the
// password session then reaches the operator plane of that tenant — and only
// of that tenant.
func TestPasswordGate_ABootstrapOwnerSignsInAsItsTenantsOperator(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bSite, "cloud", `{"tenant":"newco","access_mode":"tenant"}`)
	owner := f.redeemed(t, "cloud", "owner@newco.test", inv)
	_, subject := owner.me()
	if got := f.ownerBindings(t, "newco", subject); len(got) != 1 {
		t.Fatalf("owner bindings = %+v, want one to %s", got, authz.BaselineTenantOperator)
	}
	if !slices.Contains(f.audit.actions(), "auth_owner_bound") {
		t.Fatalf("binding not audited: %v", f.audit.actions())
	}
	// The operator plane of its own tenant: create a user, list the roster,
	// issue an invite, read the groups.
	if code, body := owner.do("POST", "/v1/_users", `{"subject":"colleague","access_mode":"isolated"}`); code != http.StatusCreated || body["tenant_id"] != "newco" {
		t.Fatalf("owner creates a user: %d %v", code, body)
	}
	if code, body := owner.do("GET", "/v1/_users", ""); code != http.StatusOK {
		t.Fatalf("owner lists users: %d %v", code, body)
	}
	if code, body := owner.do("POST", "/v1/_authgate/cloud/invites", `{}`); code != http.StatusCreated {
		t.Fatalf("owner invites: %d %v", code, body)
	}
	if code, body := owner.do("GET", "/v1/_groups", ""); code != http.StatusOK {
		t.Fatalf("owner reads groups: %d %v", code, body)
	}
	// Confined to its tenant: another tenant's name is refused or ignored.
	if code, body := owner.do("POST", "/v1/_authgate/cloud/invites", `{"tenant":"acme"}`); code != http.StatusForbidden {
		t.Fatalf("owner invites into another tenant: %d %v", code, body)
	}
	if code, body := owner.do("POST", "/v1/_users?tenant=acme", `{"subject":"sneak"}`); code != http.StatusCreated || body["tenant_id"] != "newco" {
		t.Fatalf("owner's ?tenant= is not ignored: %d %v", code, body)
	}
	if _, err := f.st.UserGet(context.Background(), "acme", "sneak"); err == nil {
		t.Fatal("the owner created a user in another tenant")
	}
}

// Member invites, attach invites and recovery never bind: a member's
// password session stays a member's.
func TestPasswordGate_OnlyTheBootstrapPathBindsAnOwner(t *testing.T) {
	f := newGateFixture(t)
	member := f.redeemed(t, "cloud", "member@acme.test", f.inviteCode(t, bAcmeOp, "cloud", `{}`))
	_, mSubject := member.me()
	if code, body := member.do("POST", "/v1/_users", `{"subject":"x"}`); code != http.StatusForbidden {
		t.Fatalf("a member creates users: %d %v", code, body)
	}
	if code, _ := member.do("GET", "/v1/_groups", ""); code != http.StatusForbidden {
		t.Fatalf("a member reads groups: %d", code)
	}
	f.addUser(t, "acme", "legacy-owner", "tenant")
	attach := f.redeemed(t, "cloud", "legacy-owner@acme.test", f.inviteCode(t, bSite, "cloud", `{"tenant":"acme","subject":"legacy-owner"}`))
	_, aSubject := attach.me()
	for _, s := range []string{mSubject, aSubject} {
		if got := f.ownerBindings(t, "acme", s); len(got) != 0 {
			t.Errorf("%s got an owner binding: %+v", s, got)
		}
	}
	// The attached user is access_mode tenant but unbound: a member session.
	if code, _ := attach.do("POST", "/v1/_users", `{"subject":"y"}`); code != http.StatusForbidden {
		t.Fatalf("an unbound tenant-mode user creates users: %d", code)
	}
}

// The /v1/_users bootstrap create of an auth:manage credential binds the
// owner too, in one transaction with the user (the store refuses the second
// owner inside it).
func TestUsersCRUD_AuthManagerBootstrapCreateBindsTheOwner(t *testing.T) {
	f := newGateFixture(t)
	if code, body := f.call(t, bSite, "POST", "/v1/_users?tenant=freshco", `{"subject":"boss","access_mode":"tenant"}`); code != http.StatusCreated {
		t.Fatalf("bootstrap create: %d %v", code, body)
	}
	if got := f.ownerBindings(t, "freshco", "boss"); len(got) != 1 {
		t.Fatalf("owner bindings = %+v", got)
	}
	err := f.st.UserCreateOwner(context.Background(), store.UserRow{TenantID: "freshco", Subject: "boss2", AccessMode: "tenant", Status: "active"},
		store.AclBindingRow{TenantID: "freshco", ACLName: authz.BaselineTenantOperator, SubjectKind: "user", SubjectID: "boss2"})
	var conflict *store.ErrConflict
	if !errors.As(err, &conflict) || conflict.Kind != "tenant_not_empty" {
		t.Fatalf("a second owner: %v", err)
	}
	if _, gerr := f.st.UserGet(context.Background(), "freshco", "boss2"); gerr == nil || len(f.ownerBindings(t, "freshco", "boss2")) != 0 {
		t.Fatal("a refused owner create left a user or a binding")
	}
}

// The binding is the grant: retiring it in the Access console takes the
// operator plane away on the owner's next request, and the owner, now above
// the floor, is out of the website's reach except by recovery.
func TestPasswordGate_RetiringTheOwnerBindingEndsOperatorPower(t *testing.T) {
	f := newGateFixture(t)
	owner := f.redeemed(t, "cloud", "boss@newco.test", f.inviteCode(t, bSite, "cloud", `{"tenant":"newco","access_mode":"tenant"}`))
	_, subject := owner.me()
	if code, body := f.call(t, bSite, "DELETE", "/v1/_users/"+subject+"?tenant=newco", ""); code != http.StatusForbidden || body["code"] != "target_beyond_floor" {
		t.Fatalf("the website deletes the owner: %d %v", code, body)
	}
	b := f.ownerBindings(t, "newco", subject)
	if len(b) != 1 {
		t.Fatalf("bindings = %+v", b)
	}
	if ok, err := f.st.AclBindingRetire(context.Background(), "newco", b[0].BindingID); !ok || err != nil {
		t.Fatalf("retire: %v %v", ok, err)
	}
	if code, _ := owner.do("POST", "/v1/_users", `{"subject":"after"}`); code != http.StatusForbidden {
		t.Fatalf("operator power survived the retired binding: %d", code)
	}
}

// A deleted owner's subject given to a new access_mode tenant user carries no
// operator power: deleting the user retired its binding.
func TestPasswordGate_ARecreatedSubjectDoesNotInheritTheOwnerBinding(t *testing.T) {
	f := newGateFixture(t)
	owner := f.redeemed(t, "cloud", "first@newco.test", f.inviteCode(t, bSite, "cloud", `{"tenant":"newco","access_mode":"tenant"}`))
	_, subject := owner.me()
	if code, _ := f.call(t, bAdmin, "DELETE", "/v1/_users/"+subject+"?tenant=newco", ""); code != http.StatusNoContent {
		t.Fatalf("admin delete: %d", code)
	}
	if len(f.ownerBindings(t, "newco", subject)) != 0 {
		t.Fatal("the deleted owner's binding is still live")
	}
	if code, b := f.call(t, bAdmin, "POST", "/v1/_users?tenant=newco", `{"subject":"`+subject+`","access_mode":"tenant"}`); code != http.StatusCreated {
		t.Fatalf("recreate: %d %v", code, b)
	}
	again := f.redeemed(t, "cloud", "second@newco.test", f.inviteCode(t, bAdmin, "cloud", `{"tenant":"newco","subject":"`+subject+`"}`))
	if code, _ := again.do("POST", "/v1/_users", `{"subject":"x"}`); code != http.StatusForbidden {
		t.Fatalf("the recreated subject has operator power: %d", code)
	}
}

// Belt and braces: an owner binding older than the user row it names never
// counts, however it got there.
func TestPasswordGate_AnOwnerBindingOlderThanItsUserGrantsNothing(t *testing.T) {
	f := newGateFixture(t)
	if _, err := f.st.AclBindingPut(context.Background(), ownerBinding("acme", "late", "test")); err != nil {
		t.Fatal(err)
	}
	f.addUser(t, "acme", "late", "tenant")
	b := f.redeemed(t, "cloud", "late@acme.test", f.inviteCode(t, bAdmin, "cloud", `{"tenant":"acme","subject":"late"}`))
	if code, _ := b.do("POST", "/v1/_users", `{"subject":"y"}`); code != http.StatusForbidden {
		t.Fatalf("a binding older than its user granted operator power: %d", code)
	}
}
