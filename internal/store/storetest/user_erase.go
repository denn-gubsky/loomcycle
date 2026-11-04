package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FY Part F: an erasure ends the account. UserErase removes every
// sign-in row of a subject and keeps its users row as a PII-free tombstone
// that no store write re-enables, renames or deletes.

// seedSignIn writes one row of every sign-in family for (tenant, subject),
// its users row and a removal record. n tells the rows apart across subjects.
func seedSignIn(t *testing.T, s store.Store, tenant, subject string, n byte) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id := fmt.Sprintf("%s-%s", tenant, subject)
	u := pwgUser(tenant, subject)
	u.DisplayName = "Person " + subject
	if err := s.UserCreate(ctx, *u); err != nil {
		t.Fatal(err)
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s for %s: %v", what, id, err)
		}
	}
	must("identity", s.UserIdentityCreate(ctx, pwgIdentity(tenant, subject, "login-"+id)))
	must("password", s.UserCredentialSet(ctx, pwgCredential(tenant, subject, "phc-fake")))
	must("totp", s.UserTOTPPut(ctx, store.UserTOTPRow{TenantID: tenant, Subject: subject, Sealed: "s", CreatedAt: pwgT0}))
	must("recovery codes", s.UserRecoveryCodesReplace(ctx, tenant, subject, []string{"rc-" + id}, pwgT0))
	must("passkey", s.UserPasskeyCreate(ctx, pkRow(tenant, subject, n)))
	must("ceremony", s.WebAuthnCeremonyCreate(ctx, store.WebAuthnCeremonyRow{CeremonyHash: "cer-" + id,
		Kind: store.CeremonyLogin, Gate: "keys", TenantID: tenant, Subject: subject, Binding: "b",
		SessionData: `{}`, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}))
	inv := pwgInvite("inv-"+id, "code-"+id, tenant)
	inv.Subject, inv.AccessMode = subject, ""
	must("invite", s.AuthInviteCreate(ctx, inv))
	must("reset", s.AuthResetCreate(ctx, pwgReset("rst-"+id, "rcode-"+id, tenant, subject, pwgT0)))
	ls := loginState("st-"+id, now.Add(10*time.Minute))
	ls.LinkTenant, ls.LinkSubject = tenant, subject
	must("link login", s.AuthLoginStateCreate(ctx, ls))
	sess := mkUISession("sess-"+id, now)
	sess.TenantID, sess.Subject = tenant, subject
	must("session", s.UISessionCreate(ctx, sess))
	must("device login", s.DeviceAuthCreate(ctx, mkDeviceAuth("dev-"+id, "uc-"+id, now)))
	if ok, err := s.DeviceAuthDecide(ctx, "uc-"+id, pinnedApproval(tenant, subject, now, now.Add(time.Hour))); err != nil || !ok {
		t.Fatalf("seed device decision for %s: %v %v", id, ok, err)
	}
	code := oauthCode("oc-"+id, now.Add(time.Minute))
	code.TenantID, code.Subject = tenant, subject
	must("app code", s.OAuthCodeCreate(ctx, code))
	fam := oauthFamily("fam-"+id, "board")
	fam.TenantID, fam.Subject = tenant, subject
	must("app family", s.OAuthFamilyCreate(ctx, fam))
	must("app refresh token", s.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: "rt-" + id, FamilyID: fam.FamilyID}))
	scim := scimUser(tenant, "scim-"+id, "user-"+id)
	scim.Subject = subject
	must("scim user", s.SCIMUserCreate(ctx, scim, time.Time{}, noCeiling))
	g := store.SCIMGroupRow{Gate: "okta", TenantID: tenant, SCIMID: "g-" + id, DisplayName: "G " + id, DisplayKey: "g-" + id}
	must("scim group", s.SCIMGroupCreate(ctx, g))
	if _, err := s.SCIMGroupMembersAdd(ctx, "okta", tenant, g.SCIMID, []string{subject}); err != nil {
		t.Fatalf("seed scim membership for %s: %v", id, err)
	}
	rm := mkRemoval(subject, now)
	rm.TenantID = tenant
	if ok, err := s.AuthRemovalStart(ctx, rm); err != nil || !ok {
		t.Fatalf("seed removal for %s: %v %v", id, ok, err)
	}
}

// oneOfEach is the count seedSignIn leaves in every family.
func oneOfEach() map[string]int {
	out := map[string]int{}
	for _, f := range store.AuthSubjectFamilies() {
		out[f] = 1
	}
	return out
}

func authCounts(t *testing.T, s store.Store, tenant, subject string) map[string]int {
	t.Helper()
	got, err := s.AuthSubjectCount(context.Background(), tenant, subject)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Every family is counted and removed, the users row stays as a tombstone
// with its removal record, nobody else's rows move, and a second erasure
// changes nothing.
func testUserEraseEndsTheAccountAndOnlyThatSubjects(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedSignIn(t, s, "acme", "alice", 1)
	seedSignIn(t, s, "acme", "bob", 2)
	seedSignIn(t, s, "other", "alice", 3)

	// The census: every family the store reports is one seedSignIn wrote,
	// so a family added to the list without a seed fails here.
	before := authCounts(t, s, "acme", "alice")
	if !reflect.DeepEqual(before, oneOfEach()) {
		t.Fatalf("count before = %v, want one of each of %v", before, store.AuthSubjectFamilies())
	}
	at := time.Now().UTC().Truncate(time.Second)
	removed, err := s.UserErase(ctx, "acme", "alice", at)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, before) {
		t.Fatalf("erase removed %v, want what the count promised %v", removed, before)
	}
	after := authCounts(t, s, "acme", "alice")
	for f, n := range after {
		if n != 0 {
			t.Errorf("after erase %s = %d, want 0", f, n)
		}
	}
	u, err := s.UserGet(ctx, "acme", "alice")
	if err != nil {
		t.Fatalf("the tombstone is gone: %v", err)
	}
	if u.DisplayName != "" || u.Status != "disabled" || !u.ErasedAt.Equal(at) || u.AccessMode != "tenant" {
		t.Fatalf("tombstone = %+v, want no display name, disabled, erased_at %v, access_mode kept", u, at)
	}
	if _, err := s.AuthRemovalGet(ctx, "acme", "alice"); err != nil {
		t.Errorf("the removal record went with the erase: %v", err)
	}
	if _, err := s.OAuthFamilyGet(ctx, "fam-acme-alice"); !pwgNotFound(err) {
		t.Errorf("the app sign-in survived: %v", err)
	}
	for _, w := range [][2]string{{"acme", "bob"}, {"other", "alice"}} {
		if got := authCounts(t, s, w[0], w[1]); !reflect.DeepEqual(got, oneOfEach()) {
			t.Errorf("%s/%s counts after another subject's erase = %v", w[0], w[1], got)
		}
		if u, err := s.UserGet(ctx, w[0], w[1]); err != nil || !u.ErasedAt.IsZero() || u.DisplayName == "" {
			t.Errorf("%s/%s user after another subject's erase = %+v, %v", w[0], w[1], u, err)
		}
	}

	again, err := s.UserErase(ctx, "acme", "alice", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for f, n := range again {
		if n != 0 {
			t.Errorf("second erase %s = %d, want 0", f, n)
		}
	}
	if u, _ := s.UserGet(ctx, "acme", "alice"); !u.ErasedAt.Equal(at) {
		t.Errorf("a second erase moved erased_at to %v", u.ErasedAt)
	}
	if list, err := s.UserList(ctx, "acme"); err != nil || len(list) != 2 || !list[0].ErasedAt.Equal(at) || !list[1].ErasedAt.IsZero() {
		t.Errorf("user list = %+v, %v; want alice erased at %v and bob not", list, err, at)
	}
}

// UserErase of a subject with no users row removes its sign-in rows and
// reports no account.
func testUserEraseWithoutAUserRowRemovesTheRest(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserIdentityCreate(ctx, pwgIdentity("acme", "ghost", "ghost")); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserErase(ctx, "acme", "ghost", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got[store.AuthFamilyIdentities] != 1 || got[store.AuthFamilyAccount] != 0 {
		t.Fatalf("erase = %v, want one identity and no account", got)
	}
	if _, err := s.UserGet(ctx, "acme", "ghost"); !pwgNotFound(err) {
		t.Fatalf("erase made a users row: %v", err)
	}
}

// No write re-enables, renames, deletes or gives a login to a tombstone.
func testUserErasedTombstoneRefusesEveryWayBack(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.UserCreate(ctx, *pwgUser("acme", "alice")); err != nil {
		t.Fatal(err)
	}
	// A SCIM removal whose re-enable was claimed before the erasure: every
	// re-enabling write below would go through on a plain disabled user.
	if ok, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", "alice", "okta", now), time.Time{}, noCeiling); err != nil || !ok {
		t.Fatalf("scim removal: %v %v", ok, err)
	}
	if ok, err := s.SCIMReactivateClaim(ctx, "okta", "acme", "alice", now, time.Time{}, noCeiling); err != nil || !ok {
		t.Fatalf("scim claim: %v %v", ok, err)
	}
	if _, err := s.UserErase(ctx, "acme", "alice", now); err != nil {
		t.Fatal(err)
	}
	active, name := "active", "Back Again"
	for _, p := range []store.UserPatch{{Status: &active}, {DisplayName: &name}} {
		if err := s.UserUpdate(ctx, "acme", "alice", p); !errors.Is(err, store.ErrUserErased) {
			t.Errorf("UserUpdate %+v = %v, want ErrUserErased", p, err)
		}
	}
	var nf *store.ErrNotFound
	if err := s.UserUpdate(ctx, "acme", "nobody", store.UserPatch{Status: &active}); !errors.As(err, &nf) {
		t.Errorf("UserUpdate of a missing user = %v, want not found", err)
	}
	if removed, err := s.UserDelete(ctx, "acme", "alice"); removed || !errors.Is(err, store.ErrUserErased) {
		t.Errorf("UserDelete = %v, %v; want false, ErrUserErased", removed, err)
	}
	if ok, err := s.SCIMReactivateClaim(ctx, "okta", "acme", "alice", now, time.Time{}, noCeiling); ok || err != nil {
		t.Errorf("SCIMReactivateClaim = %v, %v; want no change", ok, err)
	}
	if _, ok, err := s.SCIMReactivate(ctx, "okta", "acme", "alice"); ok || err != nil {
		t.Errorf("SCIMReactivate = %v, %v; want no change", ok, err)
	}
	rm, err := s.AuthRemovalGet(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AuthRemovalReactivate(ctx, rm); ok || err != nil {
		t.Errorf("AuthRemovalReactivate = %v, %v; want no change", ok, err)
	}
	inv := pwgInvite("inv-back", "code-back", "acme")
	inv.Subject, inv.AccessMode = "alice", ""
	if err := s.AuthInviteCreate(ctx, inv); err != nil {
		t.Fatal(err)
	}
	err = s.AuthInviteRedeem(ctx, store.AuthInviteRedemption{CodeHash: "code-back", Now: pwgT0.Add(time.Hour),
		Identity: pwgIdentity("acme", "alice", "alice-again"), Credential: pwgCredential("acme", "alice", "phc-again")})
	if !errors.As(err, &nf) {
		t.Errorf("invite attach to the tombstone = %v, want user not found", err)
	}
	if ids, _ := s.UserIdentityListSubject(ctx, "acme", "alice"); len(ids) != 0 {
		t.Errorf("the tombstone got a login: %+v", ids)
	}
	if u := userStatus(t, s, "acme", "alice"); u != "disabled" {
		t.Errorf("tombstone status = %s, want disabled", u)
	}
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", AccessMode: "tenant", Status: "active"}); err == nil {
		t.Error("a new user took the erased subject id")
	}
}
