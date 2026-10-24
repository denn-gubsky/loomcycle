package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P2 password-gate contract: identities, credentials and their
// throttle, TOTP and recovery codes, invites, resets, the per-key throttle and
// subject deletion, asserted against both backends. Every fixture value is
// obviously fake; no test needs a real hash or seal, only distinct strings.

// pwgT0 is a fixed, whole-second instant so both backends' timestamp precision
// (Postgres microseconds, SQLite nanoseconds) round-trips it exactly.
var pwgT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const pwgGate = "password"

func pwgIdentity(tenant, subject, login string) store.UserIdentityRow {
	return store.UserIdentityRow{TenantID: tenant, Subject: subject, Gate: pwgGate, ExternalSubject: login,
		Email: subject + "@example.test", CreatedAt: pwgT0}
}

func pwgCredential(tenant, subject, phc string) store.UserCredentialRow {
	return store.UserCredentialRow{TenantID: tenant, Subject: subject, Kind: store.CredentialKindPassword,
		PHC: phc, PepperKID: "kid-1", UpdatedAt: pwgT0}
}

func pwgUser(tenant, subject string) *store.UserRow {
	return &store.UserRow{TenantID: tenant, Subject: subject, DisplayName: subject, AccessMode: "tenant", Status: "active", CreatedAt: pwgT0, CreatedBy: "invite"}
}

func pwgInvite(id, code, tenant string) store.AuthInviteRow {
	return store.AuthInviteRow{InviteID: id, CodeHash: code, Gate: pwgGate, TenantID: tenant, AccessMode: "tenant",
		CreatedBy: "op", CreatedAt: pwgT0, ExpiresAt: pwgT0.Add(24 * time.Hour)}
}

// pwgRedemption builds a redemption creating subject with login in the
// invite's tenant, with a second factor.
func pwgRedemption(code, tenant, subject, login string, now time.Time) store.AuthInviteRedemption {
	return store.AuthInviteRedemption{
		CodeHash:   code,
		Now:        now,
		User:       pwgUser(tenant, subject),
		Identity:   pwgIdentity(tenant, subject, login),
		Credential: pwgCredential(tenant, subject, "phc-fake-"+subject),
		SecondFactor: &store.AuthSecondFactor{
			TOTP:           store.UserTOTPRow{TenantID: tenant, Subject: subject, Sealed: `{"sealed":"fake"}`, CreatedAt: now},
			RecoveryHashes: []string{"rc-fake-1", "rc-fake-2"},
		},
	}
}

func pwgNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

func pwgConflictKind(err error) string {
	var c *store.ErrConflict
	if errors.As(err, &c) {
		return c.Kind
	}
	return ""
}

func testUserIdentityCreateGetListRenameRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	pw := pwgIdentity("acme", "alice", "alice")
	pw.EmailVerified = true
	oidc := store.UserIdentityRow{TenantID: "acme", Subject: "alice", Gate: "oidc", Issuer: "https://idp.example.test",
		ExternalSubject: "sub-123", CreatedAt: pwgT0}
	for _, r := range []store.UserIdentityRow{pw, oidc} {
		if err := s.UserIdentityCreate(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.Gate, err)
		}
	}
	got, err := s.UserIdentityGet(ctx, pwgGate, "", "alice")
	if err != nil || !reflect.DeepEqual(got, pw) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, pw)
	}
	if _, err := s.UserIdentityGet(ctx, "oidc", "https://other.example.test", "sub-123"); !pwgNotFound(err) {
		t.Errorf("get under another issuer: err = %v, want not-found", err)
	}
	list, err := s.UserIdentityListSubject(ctx, "acme", "alice")
	if err != nil || len(list) != 2 || list[0].Gate != "oidc" || list[1].Gate != pwgGate {
		t.Fatalf("list = %+v, %v; want oidc then password", list, err)
	}
	if list, err := s.UserIdentityListSubject(ctx, "acme", "nobody"); err != nil || list == nil || len(list) != 0 {
		t.Errorf("list of an unknown subject = %#v, %v; want an empty slice", list, err)
	}

	if err := s.UserIdentityRename(ctx, "acme", "alice", pwgGate, "alice2"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := s.UserIdentityGet(ctx, pwgGate, "", "alice"); !pwgNotFound(err) {
		t.Errorf("old login after rename: err = %v, want not-found", err)
	}
	if got, err := s.UserIdentityGet(ctx, pwgGate, "", "alice2"); err != nil || got.Subject != "alice" {
		t.Errorf("new login after rename = %+v, %v", got, err)
	}
	if err := s.UserIdentityRename(ctx, "acme", "bob", pwgGate, "bob"); !pwgNotFound(err) {
		t.Errorf("rename of a subject with no identity: err = %v, want not-found", err)
	}

	at := pwgT0.Add(time.Hour)
	if err := s.UserIdentityTouchLogin(ctx, pwgGate, "", "alice2", at); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got, _ := s.UserIdentityGet(ctx, pwgGate, "", "alice2"); !got.LastLoginAt.Equal(at) {
		t.Errorf("last_login_at = %v, want %v", got.LastLoginAt, at)
	}
	if err := s.UserIdentityTouchLogin(ctx, pwgGate, "", "nobody", at); !pwgNotFound(err) {
		t.Errorf("touch of an unknown login: err = %v, want not-found", err)
	}
}

func testUserIdentityTakenLoginAndSecondIdentityAtGateConflict(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserIdentityCreate(ctx, pwgIdentity("acme", "alice", "alice")); err != nil {
		t.Fatal(err)
	}
	// The login is unique across tenants: a login form asks for no tenant.
	if err := s.UserIdentityCreate(ctx, pwgIdentity("other", "bob", "alice")); pwgConflictKind(err) != "user_identity" {
		t.Errorf("taken login in another tenant: err = %v, want user_identity conflict", err)
	}
	if err := s.UserIdentityCreate(ctx, pwgIdentity("acme", "alice", "alice-again")); pwgConflictKind(err) != "user_identity" {
		t.Errorf("second identity at one gate: err = %v, want user_identity conflict", err)
	}
	if got, err := s.UserIdentityGet(ctx, pwgGate, "", "alice"); err != nil || got.TenantID != "acme" {
		t.Errorf("the first identity = %+v, %v", got, err)
	}
	if _, err := s.UserIdentityGet(ctx, pwgGate, "", "alice-again"); !pwgNotFound(err) {
		t.Errorf("refused identity was written: err = %v", err)
	}
}

func testUserIdentityRenameToTakenNameConflictsAndKeepsOldName(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, r := range []store.UserIdentityRow{pwgIdentity("acme", "alice", "alice"), pwgIdentity("acme", "bob", "bob")} {
		if err := s.UserIdentityCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UserIdentityRename(ctx, "acme", "alice", pwgGate, "bob"); pwgConflictKind(err) != "user_identity" {
		t.Fatalf("rename onto a taken login: err = %v, want user_identity conflict", err)
	}
	if got, err := s.UserIdentityGet(ctx, pwgGate, "", "alice"); err != nil || got.Subject != "alice" {
		t.Errorf("alice's login after the refused rename = %+v, %v", got, err)
	}
	if got, err := s.UserIdentityGet(ctx, pwgGate, "", "bob"); err != nil || got.Subject != "bob" {
		t.Errorf("bob's login after the refused rename = %+v, %v", got, err)
	}
}

func testUserCredentialSetGetRehashIsACompareAndSet(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); !pwgNotFound(err) {
		t.Fatalf("get before set: err = %v, want not-found", err)
	}
	want := pwgCredential("acme", "alice", "phc-fake-1")
	if err := s.UserCredentialSet(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, want)
	}
	at := pwgT0.Add(time.Minute)
	if ok, err := s.UserCredentialRehash(ctx, "acme", "alice", store.CredentialKindPassword, "phc-fake-stale", "phc-fake-2", "kid-2", at); ok || err != nil {
		t.Errorf("rehash against a changed PHC = %v, %v; want false", ok, err)
	}
	if got, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); got.PHC != "phc-fake-1" {
		t.Errorf("refused rehash wrote PHC %q", got.PHC)
	}
	if ok, err := s.UserCredentialRehash(ctx, "acme", "alice", store.CredentialKindPassword, "phc-fake-1", "phc-fake-2", "kid-2", at); !ok || err != nil {
		t.Fatalf("rehash = %v, %v; want true", ok, err)
	}
	got, _ = s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword)
	if got.PHC != "phc-fake-2" || got.PepperKID != "kid-2" || !got.UpdatedAt.Equal(at) {
		t.Errorf("after rehash = %+v", got)
	}
	if ok, err := s.UserCredentialDelete(ctx, "acme", "alice", store.CredentialKindPassword); !ok || err != nil {
		t.Errorf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.UserCredentialDelete(ctx, "acme", "alice", store.CredentialKindPassword); ok {
		t.Error("second delete reported a removal")
	}
}

func testUserCredentialRecordFailureLocksPerCallbackNeverEarlier(t *testing.T, s store.Store) {
	ctx := context.Background()
	never := func(int) time.Duration { return 0 }
	if _, _, err := s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0, never); !pwgNotFound(err) {
		t.Fatalf("failure on a missing credential: err = %v, want not-found", err)
	}
	if err := s.UserCredentialSet(ctx, pwgCredential("acme", "alice", "phc-fake-1")); err != nil {
		t.Fatal(err)
	}
	lockAt3 := func(n int) time.Duration {
		if n >= 3 {
			return 10 * time.Minute
		}
		return 0
	}
	for i := 1; i <= 2; i++ {
		n, lock, err := s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0, lockAt3)
		if err != nil || n != i || !lock.IsZero() {
			t.Fatalf("failure %d = %d, %v, %v; want %d and no lock", i, n, lock, err, i)
		}
	}
	n, lock, err := s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0, lockAt3)
	if wantLock := pwgT0.Add(10 * time.Minute); err != nil || n != 3 || !lock.Equal(wantLock) {
		t.Fatalf("third failure = %d, %v, %v; want 3 locked until %v", n, lock, err, wantLock)
	}
	// A later failure whose callback asks for a shorter lock keeps the
	// existing, later one.
	short := func(int) time.Duration { return time.Minute }
	n, lock, err = s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0.Add(time.Minute), short)
	if wantLock := pwgT0.Add(10 * time.Minute); err != nil || n != 4 || !lock.Equal(wantLock) {
		t.Errorf("shorter lock = %d, %v, %v; want 4 and the lock kept at %v", n, lock, err, wantLock)
	}
	if got, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); got.FailedCount != 4 || !got.LockedUntil.Equal(pwgT0.Add(10*time.Minute)) {
		t.Errorf("stored throttle = %d / %v", got.FailedCount, got.LockedUntil)
	}

	if err := s.UserCredentialRecordSuccess(ctx, "acme", "alice", store.CredentialKindPassword); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); got.FailedCount != 0 || !got.LockedUntil.IsZero() {
		t.Errorf("after success = %d / %v; want cleared", got.FailedCount, got.LockedUntil)
	}

	// Set (a new password) also resets the throttle.
	for i := 0; i < 3; i++ {
		if _, _, err := s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0, lockAt3); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UserCredentialSet(ctx, pwgCredential("acme", "alice", "phc-fake-new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); got.FailedCount != 0 || !got.LockedUntil.IsZero() || got.PHC != "phc-fake-new" {
		t.Errorf("after set = %+v; want the new PHC and a cleared throttle", got)
	}
}

func testUserCredentialRecordFailureCountsConcurrentFailuresOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserCredentialSet(ctx, pwgCredential("acme", "alice", "phc-fake-1")); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, _, err := s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0,
				func(int) time.Duration { return 0 })
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			seen[n] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent failure: %v", err)
	}
	if len(seen) != workers {
		t.Errorf("counts seen = %v; want each of 1..%d exactly once", seen, workers)
	}
	if got, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); got.FailedCount != workers {
		t.Errorf("stored count = %d, want %d", got.FailedCount, workers)
	}
}

func testUserTOTPUseStepRefusesReplayAndConfirmsOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	if ok, err := s.UserTOTPUseStep(ctx, "acme", "alice", 10, pwgT0); ok || err != nil {
		t.Errorf("use step with no row = %v, %v; want false", ok, err)
	}
	if _, err := s.UserTOTPGet(ctx, "acme", "alice"); !pwgNotFound(err) {
		t.Fatalf("get before put: err = %v", err)
	}
	row := store.UserTOTPRow{TenantID: "acme", Subject: "alice", Sealed: `{"sealed":"fake"}`, CreatedAt: pwgT0}
	if err := s.UserTOTPPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserTOTPGet(ctx, "acme", "alice"); err != nil || !reflect.DeepEqual(got, row) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, row)
	}
	first := pwgT0.Add(time.Minute)
	if ok, err := s.UserTOTPUseStep(ctx, "acme", "alice", 100, first); !ok || err != nil {
		t.Fatalf("first step = %v, %v", ok, err)
	}
	for _, step := range []int64{100, 99} {
		if ok, _ := s.UserTOTPUseStep(ctx, "acme", "alice", step, pwgT0.Add(2*time.Minute)); ok {
			t.Errorf("step %d after 100 accepted; a replay must be refused", step)
		}
	}
	if ok, err := s.UserTOTPUseStep(ctx, "acme", "alice", 101, pwgT0.Add(3*time.Minute)); !ok || err != nil {
		t.Errorf("next step = %v, %v", ok, err)
	}
	got, _ := s.UserTOTPGet(ctx, "acme", "alice")
	if got.LastStep != 101 || !got.ConfirmedAt.Equal(first) {
		t.Errorf("after steps = last %d confirmed %v; want 101 and confirmed once at %v", got.LastStep, got.ConfirmedAt, first)
	}
	// Re-enrolling replaces the row: unconfirmed again, step reset.
	if err := s.UserTOTPPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserTOTPGet(ctx, "acme", "alice"); got.LastStep != 0 || !got.ConfirmedAt.IsZero() {
		t.Errorf("after re-put = %+v", got)
	}
}

func testUserRecoveryCodeConsumesExactlyOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserRecoveryCodesReplace(ctx, "acme", "alice", []string{"rc-a", "rc-b", "rc-c"}, pwgT0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UserRecoveryCodesRemaining(ctx, "acme", "alice"); n != 3 || err != nil {
		t.Fatalf("remaining = %d, %v", n, err)
	}
	if ok, err := s.UserRecoveryCodeConsume(ctx, "acme", "alice", "rc-unknown", pwgT0); ok || err != nil {
		t.Errorf("unknown code = %v, %v; want false", ok, err)
	}
	if ok, _ := s.UserRecoveryCodeConsume(ctx, "other", "alice", "rc-a", pwgT0); ok {
		t.Error("a code was consumed under another tenant")
	}
	// Concurrent logins presenting one code: exactly one wins.
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.UserRecoveryCodeConsume(ctx, "acme", "alice", "rc-a", pwgT0)
			if err != nil {
				t.Errorf("consume: %v", err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("concurrent consumes won %d times, want 1", wins)
	}
	if n, _ := s.UserRecoveryCodesRemaining(ctx, "acme", "alice"); n != 2 {
		t.Errorf("remaining after one use = %d, want 2", n)
	}
	if err := s.UserRecoveryCodesReplace(ctx, "acme", "alice", []string{"rc-x"}, pwgT0); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UserRecoveryCodesRemaining(ctx, "acme", "alice"); n != 1 {
		t.Errorf("remaining after replace = %d, want 1", n)
	}
	if ok, _ := s.UserRecoveryCodeConsume(ctx, "acme", "alice", "rc-b", pwgT0); ok {
		t.Error("a replaced code still works")
	}
	// TOTP delete takes the recovery codes with it.
	if err := s.UserTOTPPut(ctx, store.UserTOTPRow{TenantID: "acme", Subject: "alice", Sealed: "x", CreatedAt: pwgT0}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UserTOTPDelete(ctx, "acme", "alice"); !ok || err != nil {
		t.Fatalf("totp delete = %v, %v", ok, err)
	}
	if n, _ := s.UserRecoveryCodesRemaining(ctx, "acme", "alice"); n != 0 {
		t.Errorf("recovery codes after totp delete = %d, want 0", n)
	}
	if _, err := s.UserTOTPGet(ctx, "acme", "alice"); !pwgNotFound(err) {
		t.Errorf("totp after delete: err = %v", err)
	}
	if ok, _ := s.UserTOTPDelete(ctx, "acme", "alice"); ok {
		t.Error("second totp delete reported a removal")
	}
}

func testAuthInviteCreateGetListAndRevokeOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	a := pwgInvite("inv-a", "code-a", "acme")
	a.Groups = []string{"support", "ops"}
	a.Login, a.Email, a.ReturnTo = "alice", "alice@example.test", "/ui/run"
	b := pwgInvite("inv-b", "code-b", "acme")
	b.CreatedAt = pwgT0.Add(time.Minute)
	c := pwgInvite("inv-c", "code-c", "other")
	c.CreatedAt = pwgT0.Add(2 * time.Minute)
	d := pwgInvite("inv-d", "code-d", "acme")
	d.Gate = "another-gate"
	for _, r := range []store.AuthInviteRow{a, b, c, d} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.InviteID, err)
		}
	}
	dupID := pwgInvite("inv-a", "code-new", "acme")
	if err := s.AuthInviteCreate(ctx, dupID); pwgConflictKind(err) != "auth_invite" {
		t.Errorf("duplicate id: err = %v, want auth_invite conflict", err)
	}
	dupCode := pwgInvite("inv-new", "code-a", "acme")
	if err := s.AuthInviteCreate(ctx, dupCode); pwgConflictKind(err) != "auth_invite" {
		t.Errorf("duplicate code: err = %v, want auth_invite conflict", err)
	}

	got, err := s.AuthInviteGet(ctx, "inv-a")
	if err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, a)
	}
	if got, err := s.AuthInviteGetByCode(ctx, "code-a"); err != nil || got.InviteID != "inv-a" {
		t.Errorf("get by code = %+v, %v", got, err)
	}
	if _, err := s.AuthInviteGet(ctx, "inv-none"); !pwgNotFound(err) {
		t.Errorf("get miss: err = %v", err)
	}
	if _, err := s.AuthInviteGetByCode(ctx, "code-none"); !pwgNotFound(err) {
		t.Errorf("get by code miss: err = %v", err)
	}

	ids := func(rows []store.AuthInviteRow) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.InviteID)
		}
		return out
	}
	acme, err := s.AuthInviteList(ctx, pwgGate, "acme")
	if err != nil || !reflect.DeepEqual(ids(acme), []string{"inv-b", "inv-a"}) {
		t.Errorf("acme list = %v, %v; want newest first [inv-b inv-a]", ids(acme), err)
	}
	all, err := s.AuthInviteListAllTenants(ctx, pwgGate)
	if err != nil || !reflect.DeepEqual(ids(all), []string{"inv-c", "inv-b", "inv-a"}) {
		t.Errorf("all-tenant list = %v, %v; want [inv-c inv-b inv-a]", ids(all), err)
	}
	// "" is the shared tenant, not a wildcard: it lists none of the others'.
	if shared, err := s.AuthInviteList(ctx, pwgGate, ""); err != nil || len(shared) != 0 {
		t.Errorf("shared-tenant list = %v, %v; want none of the other tenants' invites", ids(shared), err)
	}
	if none, err := s.AuthInviteList(ctx, pwgGate, "nobody"); err != nil || none == nil || len(none) != 0 {
		t.Errorf("empty list = %#v, %v; want an empty slice", none, err)
	}

	at := pwgT0.Add(time.Hour)
	if ok, err := s.AuthInviteRevoke(ctx, "inv-a", at); !ok || err != nil {
		t.Fatalf("revoke = %v, %v", ok, err)
	}
	if ok, _ := s.AuthInviteRevoke(ctx, "inv-a", at); ok {
		t.Error("second revoke reported a change")
	}
	if ok, _ := s.AuthInviteRevoke(ctx, "inv-none", at); ok {
		t.Error("revoking an unknown invite reported a change")
	}
	if ok, _ := s.AuthInviteRevoke(ctx, "inv-b", pwgT0.Add(48*time.Hour)); ok {
		t.Error("revoking an expired invite reported a change")
	}
	got, _ = s.AuthInviteGet(ctx, "inv-a")
	if !got.RevokedAt.Equal(at) || got.Live(at) {
		t.Errorf("after revoke = revoked_at %v live %v", got.RevokedAt, got.Live(at))
	}
}

func testAuthInviteRedeemWritesUserIdentityCredentialAndSecondFactor(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.AuthInviteCreate(ctx, pwgInvite("inv-1", "code-1", "acme")); err != nil {
		t.Fatal(err)
	}
	now := pwgT0.Add(time.Hour)
	red := pwgRedemption("code-1", "acme", "alice", "alice", now)
	if err := s.AuthInviteRedeem(ctx, red); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	inv, _ := s.AuthInviteGet(ctx, "inv-1")
	if !inv.RedeemedAt.Equal(now) || inv.RedeemedSubject != "alice" || inv.Live(now) {
		t.Errorf("invite after redeem = %+v", inv)
	}
	if u, err := s.UserGet(ctx, "acme", "alice"); err != nil || u.AccessMode != "tenant" || u.Status != "active" {
		t.Errorf("user = %+v, %v", u, err)
	}
	if id, err := s.UserIdentityGet(ctx, pwgGate, "", "alice"); err != nil || id.Subject != "alice" || id.TenantID != "acme" {
		t.Errorf("identity = %+v, %v", id, err)
	}
	if c, err := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); err != nil || c.PHC != "phc-fake-alice" {
		t.Errorf("credential = %+v, %v", c, err)
	}
	if tp, err := s.UserTOTPGet(ctx, "acme", "alice"); err != nil || tp.Sealed != `{"sealed":"fake"}` {
		t.Errorf("totp = %+v, %v", tp, err)
	}
	if n, _ := s.UserRecoveryCodesRemaining(ctx, "acme", "alice"); n != 2 {
		t.Errorf("recovery codes = %d, want 2", n)
	}
	// A second redemption of the same code is a miss.
	again := pwgRedemption("code-1", "acme", "bob", "bob", now)
	if err := s.AuthInviteRedeem(ctx, again); !pwgNotFound(err) {
		t.Errorf("second redeem: err = %v, want not-found", err)
	}
	if _, err := s.UserGet(ctx, "acme", "bob"); !pwgNotFound(err) {
		t.Errorf("second redeem created a user: err = %v", err)
	}
}

// assertRedeemWroteNothing checks that a refused redemption of subject left no
// user, login, credential or second factor behind.
func assertRedeemWroteNothing(t *testing.T, s store.Store, tenant, subject, login string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.UserGet(ctx, tenant, subject); !pwgNotFound(err) {
		t.Errorf("user %s written: err = %v", subject, err)
	}
	if _, err := s.UserIdentityGet(ctx, pwgGate, "", login); !pwgNotFound(err) {
		t.Errorf("login %s written: err = %v", login, err)
	}
	if _, err := s.UserCredentialGet(ctx, tenant, subject, store.CredentialKindPassword); !pwgNotFound(err) {
		t.Errorf("credential of %s written: err = %v", subject, err)
	}
	if _, err := s.UserTOTPGet(ctx, tenant, subject); !pwgNotFound(err) {
		t.Errorf("totp of %s written: err = %v", subject, err)
	}
	if n, _ := s.UserRecoveryCodesRemaining(ctx, tenant, subject); n != 0 {
		t.Errorf("recovery codes of %s written: %d", subject, n)
	}
}

func testAuthInviteRedeemOfDeadInviteIsNotFoundAndWritesNothing(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	expired := pwgInvite("inv-exp", "code-exp", "acme")
	expired.ExpiresAt = now
	revoked := pwgInvite("inv-rev", "code-rev", "acme")
	redeemed := pwgInvite("inv-red", "code-red", "acme")
	redeemed.RedeemedAt, redeemed.RedeemedSubject = pwgT0.Add(time.Minute), "someone"
	for _, r := range []store.AuthInviteRow{expired, revoked, redeemed} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.AuthInviteRevoke(ctx, "inv-rev", pwgT0.Add(time.Minute)); !ok || err != nil {
		t.Fatalf("revoke = %v, %v", ok, err)
	}
	for i, code := range []string{"code-exp", "code-rev", "code-red", "code-unknown"} {
		subject := fmt.Sprintf("user-%d", i)
		if err := s.AuthInviteRedeem(ctx, pwgRedemption(code, "acme", subject, subject, now)); !pwgNotFound(err) {
			t.Errorf("redeem %s: err = %v, want not-found", code, err)
		}
		assertRedeemWroteNothing(t, s, "acme", subject, subject)
	}
	if got, _ := s.AuthInviteGet(ctx, "inv-red"); got.RedeemedSubject != "someone" {
		t.Errorf("a refused redeem rewrote redeemed_subject to %q", got.RedeemedSubject)
	}
}

func testAuthInviteRedeemTakenLoginConflictsAndLeavesInviteLive(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserIdentityCreate(ctx, pwgIdentity("other", "carol", "alice")); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthInviteCreate(ctx, pwgInvite("inv-1", "code-1", "acme")); err != nil {
		t.Fatal(err)
	}
	now := pwgT0.Add(time.Hour)
	if err := s.AuthInviteRedeem(ctx, pwgRedemption("code-1", "acme", "alice", "alice", now)); pwgConflictKind(err) != "user_identity" {
		t.Fatalf("redeem with a taken login: err = %v, want user_identity conflict", err)
	}
	inv, _ := s.AuthInviteGet(ctx, "inv-1")
	if !inv.Live(now) || inv.RedeemedSubject != "" {
		t.Errorf("invite after the refused redeem = %+v; want still live", inv)
	}
	if _, err := s.UserGet(ctx, "acme", "alice"); !pwgNotFound(err) {
		t.Errorf("the refused redeem created the user: err = %v", err)
	}
	if _, err := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); !pwgNotFound(err) {
		t.Errorf("the refused redeem wrote a credential: err = %v", err)
	}
	// The invite still works with a free login.
	if err := s.AuthInviteRedeem(ctx, pwgRedemption("code-1", "acme", "alice", "alice-free", now)); err != nil {
		t.Errorf("retry with a free login: %v", err)
	}
}

func testAuthInviteRedeemBootstrapRefusedOnceTenantHasUser(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	first := pwgInvite("inv-boot-1", "code-boot-1", "acme")
	first.Bootstrap = true
	second := pwgInvite("inv-boot-2", "code-boot-2", "acme")
	second.Bootstrap = true
	for _, r := range []store.AuthInviteRow{first, second} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AuthInviteRedeem(ctx, pwgRedemption("code-boot-1", "acme", "owner", "owner", now)); err != nil {
		t.Fatalf("first bootstrap redeem: %v", err)
	}
	if err := s.AuthInviteRedeem(ctx, pwgRedemption("code-boot-2", "acme", "intruder", "intruder", now)); pwgConflictKind(err) != "tenant_not_empty" {
		t.Fatalf("bootstrap into a tenant with a user: err = %v, want tenant_not_empty", err)
	}
	assertRedeemWroteNothing(t, s, "acme", "intruder", "intruder")
	if inv, _ := s.AuthInviteGet(ctx, "inv-boot-2"); !inv.Live(now) {
		t.Errorf("refused bootstrap invite consumed: %+v", inv)
	}
}

// A tenant that ever had a user stays bootstrapped after every user is
// deleted, so an owner bootstrap cannot be re-armed by emptying the tenant
// (the takeover the marker exists to close). Users created by UserCreate and
// by a redemption both mark it.
func testTenantBootstrappedSurvivesDeletingEveryUser(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	for _, tenant := range []string{"via-users", "via-invite"} {
		if got, err := s.TenantBootstrapped(ctx, tenant); err != nil || got {
			t.Fatalf("%s before any user: (%v, %v), want (false, nil)", tenant, got, err)
		}
	}
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "via-users", Subject: "u", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	boot := pwgInvite("inv-mark", "code-mark", "via-invite")
	boot.Bootstrap = true
	if err := s.AuthInviteCreate(ctx, boot); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthInviteRedeem(ctx, pwgRedemption("code-mark", "via-invite", "owner", "owner-mark", now)); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	for tenant, subject := range map[string]string{"via-users": "u", "via-invite": "owner"} {
		if _, err := s.UserDelete(ctx, tenant, subject); err != nil {
			t.Fatal(err)
		}
		if got, err := s.TenantBootstrapped(ctx, tenant); err != nil || !got {
			t.Errorf("%s after deleting its only user: (%v, %v), want (true, nil)", tenant, got, err)
		}
	}
	again := pwgInvite("inv-rearm", "code-rearm", "via-users")
	again.Bootstrap = true
	if err := s.AuthInviteCreate(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthInviteRedeem(ctx, pwgRedemption("code-rearm", "via-users", "intruder", "intruder-rearm", now)); pwgConflictKind(err) != "tenant_not_empty" {
		t.Fatalf("bootstrap of an emptied tenant: err = %v, want tenant_not_empty", err)
	}
	if got, _ := s.TenantBootstrapped(ctx, "never-had-users"); got {
		t.Error("a tenant with no user ever reads as bootstrapped")
	}
}

func testAuthInviteRedeemConcurrentBootstrapsAdmitOne(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	const n = 4
	for i := 0; i < n; i++ {
		inv := pwgInvite(fmt.Sprintf("inv-%d", i), fmt.Sprintf("code-%d", i), "acme")
		inv.Bootstrap = true
		if err := s.AuthInviteCreate(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub := fmt.Sprintf("owner-%d", i)
			results[i] = s.AuthInviteRedeem(ctx, pwgRedemption(fmt.Sprintf("code-%d", i), "acme", sub, sub, now))
		}(i)
	}
	wg.Wait()
	ok := 0
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case pwgConflictKind(err) != "tenant_not_empty":
			t.Errorf("redeem %d: err = %v, want nil or tenant_not_empty", i, err)
		}
	}
	if ok != 1 {
		t.Errorf("%d concurrent bootstrap redemptions succeeded, want exactly 1", ok)
	}
	if users, _ := s.UserList(ctx, "acme"); len(users) != 1 {
		t.Errorf("tenant has %d users, want 1", len(users))
	}
}

func testAuthInviteRedeemAttachToMissingSubjectIsNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	ghost := pwgInvite("inv-ghost", "code-ghost", "acme")
	ghost.Subject, ghost.AccessMode = "ghost", ""
	existing := pwgInvite("inv-real", "code-real", "acme")
	existing.Subject, existing.AccessMode = "dave", ""
	for _, r := range []store.AuthInviteRow{ghost, existing} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	attach := func(code, subject string) store.AuthInviteRedemption {
		return store.AuthInviteRedemption{CodeHash: code, Now: now,
			Identity: pwgIdentity("acme", subject, subject), Credential: pwgCredential("acme", subject, "phc-fake-"+subject)}
	}
	var nf *store.ErrNotFound
	if err := s.AuthInviteRedeem(ctx, attach("code-ghost", "ghost")); !errors.As(err, &nf) || nf.Kind != "user" {
		t.Fatalf("attach to a missing subject: err = %v, want user not-found", err)
	}
	assertRedeemWroteNothing(t, s, "acme", "ghost", "ghost")
	if inv, _ := s.AuthInviteGet(ctx, "inv-ghost"); !inv.Live(now) {
		t.Errorf("failed attach consumed the invite: %+v", inv)
	}
	// Attaching to a subject that exists writes the login and the password
	// and creates no user.
	if err := s.UserCreate(ctx, *pwgUser("acme", "dave")); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthInviteRedeem(ctx, attach("code-real", "dave")); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := s.UserCredentialGet(ctx, "acme", "dave", store.CredentialKindPassword); err != nil {
		t.Errorf("attached credential: %v", err)
	}
	if users, _ := s.UserList(ctx, "acme"); len(users) != 1 {
		t.Errorf("attach created a user: %d users", len(users))
	}
}

func testAuthInviteLiveBootstrapReportsOnlyLiveBootstrap(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	if ok, err := s.AuthInviteLiveBootstrap(ctx, "acme", now); ok || err != nil {
		t.Fatalf("no invites = %v, %v", ok, err)
	}
	plain := pwgInvite("inv-plain", "code-plain", "acme")
	expired := pwgInvite("inv-exp", "code-exp", "acme")
	expired.Bootstrap, expired.ExpiresAt = true, now
	elsewhere := pwgInvite("inv-else", "code-else", "other")
	elsewhere.Bootstrap = true
	for _, r := range []store.AuthInviteRow{plain, expired, elsewhere} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := s.AuthInviteLiveBootstrap(ctx, "acme", now); ok {
		t.Error("a plain, an expired and another tenant's bootstrap invite read as a live bootstrap")
	}
	live := pwgInvite("inv-live", "code-live", "acme")
	live.Bootstrap = true
	if err := s.AuthInviteCreate(ctx, live); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AuthInviteLiveBootstrap(ctx, "acme", now); !ok {
		t.Error("a live bootstrap invite was not reported")
	}
	if _, err := s.AuthInviteRevoke(ctx, "inv-live", now); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AuthInviteLiveBootstrap(ctx, "acme", now); ok {
		t.Error("a revoked bootstrap invite still reads as live")
	}
}

func pwgReset(id, code, tenant, subject string, created time.Time) store.AuthResetRow {
	return store.AuthResetRow{ResetID: id, CodeHash: code, Gate: pwgGate, TenantID: tenant, Subject: subject,
		CreatedBy: "op", CreatedAt: created, ExpiresAt: created.Add(time.Hour)}
}

func testAuthResetCreateSupersedesEarlierLiveReset(t *testing.T, s store.Store) {
	ctx := context.Background()
	first := pwgReset("rst-1", "rcode-1", "acme", "alice", pwgT0)
	other := pwgReset("rst-o", "rcode-o", "acme", "bob", pwgT0)
	for _, r := range []store.AuthResetRow{first, other} {
		if err := s.AuthResetCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.AuthResetGetByCode(ctx, "rcode-1")
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, first)
	}
	second := pwgReset("rst-2", "rcode-2", "acme", "alice", pwgT0.Add(time.Minute))
	if err := s.AuthResetCreate(ctx, second); err != nil {
		t.Fatal(err)
	}
	now := pwgT0.Add(2 * time.Minute)
	if got, _ := s.AuthResetGetByCode(ctx, "rcode-1"); got.Live(now) || !got.UsedAt.Equal(second.CreatedAt) {
		t.Errorf("first reset after a newer one = %+v; want used at %v", got, second.CreatedAt)
	}
	if got, _ := s.AuthResetGetByCode(ctx, "rcode-2"); !got.Live(now) {
		t.Errorf("newest reset = %+v; want live", got)
	}
	if got, _ := s.AuthResetGetByCode(ctx, "rcode-o"); !got.Live(now) {
		t.Errorf("another subject's reset was superseded: %+v", got)
	}
	if err := s.AuthResetCreate(ctx, pwgReset("rst-2", "rcode-new", "acme", "alice", now)); pwgConflictKind(err) != "auth_reset" {
		t.Errorf("duplicate reset id: err = %v, want auth_reset conflict", err)
	}
	if _, err := s.AuthResetGetByCode(ctx, "rcode-none"); !pwgNotFound(err) {
		t.Errorf("get miss: err = %v", err)
	}
}

func testAuthResetCompleteConsumesOnceAndReplacesCredential(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserCredentialSet(ctx, pwgCredential("acme", "alice", "phc-fake-old")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := s.UserCredentialRecordFailure(ctx, "acme", "alice", store.CredentialKindPassword, pwgT0,
			func(int) time.Duration { return time.Hour }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AuthResetCreate(ctx, pwgReset("rst-1", "rcode-1", "acme", "alice", pwgT0)); err != nil {
		t.Fatal(err)
	}
	now := pwgT0.Add(10 * time.Minute)
	c := store.AuthResetCompletion{CodeHash: "rcode-1", Now: now, Credential: pwgCredential("acme", "alice", "phc-fake-new"),
		SecondFactor: &store.AuthSecondFactor{
			TOTP:           store.UserTOTPRow{TenantID: "acme", Subject: "alice", Sealed: "sealed-fake", CreatedAt: now},
			RecoveryHashes: []string{"rc-1"},
		}}
	if err := s.AuthResetComplete(ctx, c); err != nil {
		t.Fatalf("complete: %v", err)
	}
	cred, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword)
	if cred.PHC != "phc-fake-new" || cred.FailedCount != 0 || !cred.LockedUntil.IsZero() {
		t.Errorf("credential after reset = %+v; want the new PHC with the throttle cleared", cred)
	}
	if tp, err := s.UserTOTPGet(ctx, "acme", "alice"); err != nil || tp.Sealed != "sealed-fake" {
		t.Errorf("totp after reset = %+v, %v", tp, err)
	}
	if got, _ := s.AuthResetGetByCode(ctx, "rcode-1"); !got.UsedAt.Equal(now) {
		t.Errorf("reset used_at = %v, want %v", got.UsedAt, now)
	}
	c.Credential.PHC = "phc-fake-again"
	if err := s.AuthResetComplete(ctx, c); !pwgNotFound(err) {
		t.Errorf("second complete: err = %v, want not-found", err)
	}
	if cred, _ := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); cred.PHC != "phc-fake-new" {
		t.Errorf("a refused complete wrote PHC %q", cred.PHC)
	}
	// An expired reset is a miss too.
	if err := s.AuthResetCreate(ctx, pwgReset("rst-2", "rcode-2", "acme", "alice", pwgT0)); err != nil {
		t.Fatal(err)
	}
	late := store.AuthResetCompletion{CodeHash: "rcode-2", Now: pwgT0.Add(2 * time.Hour), Credential: pwgCredential("acme", "alice", "phc-fake-late")}
	if err := s.AuthResetComplete(ctx, late); !pwgNotFound(err) {
		t.Errorf("expired complete: err = %v, want not-found", err)
	}
}

func testAuthThrottleWindowBlockAndExpiry(t *testing.T, s store.Store) {
	ctx := context.Background()
	const key = "login:alice"
	window := 15 * time.Minute
	if n, b, err := s.AuthThrottleGet(ctx, key, pwgT0); n != 0 || !b.IsZero() || err != nil {
		t.Fatalf("unknown key = %d, %v, %v", n, b, err)
	}
	blockAt3 := func(n int) time.Duration {
		if n >= 3 {
			return time.Hour
		}
		return 0
	}
	for i := 1; i <= 2; i++ {
		n, b, err := s.AuthThrottleRecordFailure(ctx, key, pwgT0.Add(time.Duration(i)*time.Minute), window, blockAt3)
		if n != i || !b.IsZero() || err != nil {
			t.Fatalf("failure %d = %d, %v, %v", i, n, b, err)
		}
	}
	if n, _, _ := s.AuthThrottleGet(ctx, key, pwgT0.Add(3*time.Minute)); n != 2 {
		t.Errorf("get inside the window = %d, want 2", n)
	}
	// After the window the count reads zero and the next failure starts a new
	// window at 1.
	afterWindow := pwgT0.Add(window + time.Minute)
	if n, b, _ := s.AuthThrottleGet(ctx, key, afterWindow); n != 0 || !b.IsZero() {
		t.Errorf("get after the window = %d, %v; want zero values", n, b)
	}
	n, _, err := s.AuthThrottleRecordFailure(ctx, key, afterWindow, window, blockAt3)
	if n != 1 || err != nil {
		t.Fatalf("first failure of a new window = %d, %v; want 1", n, err)
	}
	if _, _, err := s.AuthThrottleRecordFailure(ctx, key, afterWindow, window, blockAt3); err != nil {
		t.Fatal(err)
	}
	n, b, err := s.AuthThrottleRecordFailure(ctx, key, afterWindow, window, blockAt3)
	if wantBlock := afterWindow.Add(time.Hour); n != 3 || !b.Equal(wantBlock) || err != nil {
		t.Fatalf("third failure = %d, %v, %v; want 3 blocked until %v", n, b, err, wantBlock)
	}
	// A shorter block never moves the existing one earlier.
	n, b, _ = s.AuthThrottleRecordFailure(ctx, key, afterWindow.Add(time.Minute), window, func(int) time.Duration { return time.Minute })
	if wantBlock := afterWindow.Add(time.Hour); n != 4 || !b.Equal(wantBlock) {
		t.Errorf("shorter block = %d, %v; want 4 and the block kept at %v", n, b, wantBlock)
	}
	// The block outlasts the window: it still reads as blocked after the
	// window closed, with the failure count zero.
	if n, b, _ := s.AuthThrottleGet(ctx, key, afterWindow.Add(window+time.Minute)); n != 0 || !b.Equal(afterWindow.Add(time.Hour)) {
		t.Errorf("get after the window inside the block = %d, %v", n, b)
	}

	if err := s.AuthThrottleClear(ctx, key); err != nil {
		t.Fatal(err)
	}
	if n, b, _ := s.AuthThrottleGet(ctx, key, afterWindow); n != 0 || !b.IsZero() {
		t.Errorf("after clear = %d, %v", n, b)
	}

	// DeleteExpired removes a key only when both its window and its block ended.
	if _, _, err := s.AuthThrottleRecordFailure(ctx, "ip:a", pwgT0, window, func(int) time.Duration { return 0 }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthThrottleRecordFailure(ctx, "ip:b", pwgT0, window, func(int) time.Duration { return 2 * time.Hour }); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AuthThrottleDeleteExpired(ctx, pwgT0.Add(time.Hour)); n != 1 || err != nil {
		t.Errorf("delete expired = %d, %v; want 1 (ip:b is still blocked)", n, err)
	}
	if _, b, _ := s.AuthThrottleGet(ctx, "ip:b", pwgT0.Add(time.Hour)); b.IsZero() {
		t.Error("a blocked key was deleted")
	}
	if n, _ := s.AuthThrottleDeleteExpired(ctx, pwgT0.Add(3*time.Hour)); n != 1 {
		t.Errorf("delete after the block = %d, want 1", n)
	}
}

func testAuthInviteAndResetDeleteExpiredKeepLiveRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	cut := pwgT0.Add(48 * time.Hour)
	live := pwgInvite("inv-live", "code-live", "acme")
	live.ExpiresAt = cut.Add(time.Hour)
	expired := pwgInvite("inv-exp", "code-exp", "acme") // expires pwgT0+24h
	redeemed := pwgInvite("inv-red", "code-red", "acme")
	redeemed.ExpiresAt, redeemed.RedeemedAt, redeemed.RedeemedSubject = cut.Add(time.Hour), pwgT0.Add(time.Hour), "x"
	revoked := pwgInvite("inv-rev", "code-rev", "acme")
	revoked.ExpiresAt, revoked.RevokedAt = cut.Add(time.Hour), pwgT0.Add(time.Hour)
	for _, r := range []store.AuthInviteRow{live, expired, redeemed, revoked} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.AuthInviteDeleteExpired(ctx, cut); n != 3 || err != nil {
		t.Errorf("invite delete expired = %d, %v; want 3", n, err)
	}
	if _, err := s.AuthInviteGet(ctx, "inv-live"); err != nil {
		t.Errorf("live invite deleted: %v", err)
	}

	liveReset := pwgReset("rst-live", "rc-live", "acme", "alice", cut)
	oldReset := pwgReset("rst-old", "rc-old", "acme", "bob", pwgT0)
	for _, r := range []store.AuthResetRow{liveReset, oldReset} {
		if err := s.AuthResetCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.AuthResetDeleteExpired(ctx, cut); n != 1 || err != nil {
		t.Errorf("reset delete expired = %d, %v; want 1", n, err)
	}
	if _, err := s.AuthResetGetByCode(ctx, "rc-live"); err != nil {
		t.Errorf("live reset deleted: %v", err)
	}
}

func testAuthSubjectDeleteRemovesOnlyThatSubjectsRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	type who struct{ tenant, subject, login string }
	victim := who{"acme", "alice", "alice"}
	sameTenant := who{"acme", "bob", "bob"}
	otherTenant := who{"other", "alice", "alice-other"}
	for i, w := range []who{victim, sameTenant, otherTenant} {
		if err := s.UserIdentityCreate(ctx, pwgIdentity(w.tenant, w.subject, w.login)); err != nil {
			t.Fatal(err)
		}
		if err := s.UserCredentialSet(ctx, pwgCredential(w.tenant, w.subject, "phc-fake")); err != nil {
			t.Fatal(err)
		}
		if err := s.UserTOTPPut(ctx, store.UserTOTPRow{TenantID: w.tenant, Subject: w.subject, Sealed: "s", CreatedAt: pwgT0}); err != nil {
			t.Fatal(err)
		}
		if err := s.UserRecoveryCodesReplace(ctx, w.tenant, w.subject, []string{"rc"}, pwgT0); err != nil {
			t.Fatal(err)
		}
		inv := pwgInvite(fmt.Sprintf("inv-%d", i), fmt.Sprintf("code-%d", i), w.tenant)
		inv.Subject, inv.AccessMode = w.subject, ""
		if err := s.AuthInviteCreate(ctx, inv); err != nil {
			t.Fatal(err)
		}
		if err := s.AuthResetCreate(ctx, pwgReset(fmt.Sprintf("rst-%d", i), fmt.Sprintf("rcode-%d", i), w.tenant, w.subject, pwgT0)); err != nil {
			t.Fatal(err)
		}
	}
	// An invite the victim redeemed (it created them) goes too.
	redeemedBy := pwgInvite("inv-redeemed", "code-redeemed", "acme")
	redeemedBy.RedeemedAt, redeemedBy.RedeemedSubject = pwgT0, "alice"
	if err := s.AuthInviteCreate(ctx, redeemedBy); err != nil {
		t.Fatal(err)
	}

	if err := s.AuthSubjectDelete(ctx, "acme", "alice"); err != nil {
		t.Fatalf("subject delete: %v", err)
	}
	if _, err := s.UserIdentityGet(ctx, pwgGate, "", "alice"); !pwgNotFound(err) {
		t.Errorf("victim identity survived: %v", err)
	}
	if _, err := s.UserCredentialGet(ctx, "acme", "alice", store.CredentialKindPassword); !pwgNotFound(err) {
		t.Errorf("victim credential survived: %v", err)
	}
	if _, err := s.UserTOTPGet(ctx, "acme", "alice"); !pwgNotFound(err) {
		t.Errorf("victim totp survived: %v", err)
	}
	if n, _ := s.UserRecoveryCodesRemaining(ctx, "acme", "alice"); n != 0 {
		t.Errorf("victim recovery codes survived: %d", n)
	}
	for _, id := range []string{"inv-0", "inv-redeemed"} {
		if _, err := s.AuthInviteGet(ctx, id); !pwgNotFound(err) {
			t.Errorf("victim invite %s survived: %v", id, err)
		}
	}
	if _, err := s.AuthResetGetByCode(ctx, "rcode-0"); !pwgNotFound(err) {
		t.Errorf("victim reset survived: %v", err)
	}

	for j, w := range []who{sameTenant, otherTenant} {
		i := j + 1 // their fixtures were created as 1 and 2
		if _, err := s.UserIdentityGet(ctx, pwgGate, "", w.login); err != nil {
			t.Errorf("%s/%s identity removed: %v", w.tenant, w.subject, err)
		}
		if _, err := s.UserCredentialGet(ctx, w.tenant, w.subject, store.CredentialKindPassword); err != nil {
			t.Errorf("%s/%s credential removed: %v", w.tenant, w.subject, err)
		}
		if _, err := s.UserTOTPGet(ctx, w.tenant, w.subject); err != nil {
			t.Errorf("%s/%s totp removed: %v", w.tenant, w.subject, err)
		}
		if n, _ := s.UserRecoveryCodesRemaining(ctx, w.tenant, w.subject); n != 1 {
			t.Errorf("%s/%s recovery codes = %d, want 1", w.tenant, w.subject, n)
		}
		if _, err := s.AuthInviteGet(ctx, fmt.Sprintf("inv-%d", i)); err != nil {
			t.Errorf("%s/%s invite removed: %v", w.tenant, w.subject, err)
		}
		if _, err := s.AuthResetGetByCode(ctx, fmt.Sprintf("rcode-%d", i)); err != nil {
			t.Errorf("%s/%s reset removed: %v", w.tenant, w.subject, err)
		}
	}
}
