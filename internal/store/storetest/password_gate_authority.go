package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P2 password gate: the issuer kind on invite/reset links, the
// revocation of a manager's links when their target's authority is raised,
// and the bootstrap markers a snapshot carries. Siblings of
// password_gate.go; same fixtures.

// IssuedByManager survives the write and every read of both link kinds.
func testAuthInviteAndResetRoundTripIssuedByManager(t *testing.T, s store.Store) {
	ctx := context.Background()
	inv := pwgInvite("inv-mgr", "code-mgr", "acme")
	inv.IssuedByManager = true
	plain := pwgInvite("inv-op", "code-op", "acme")
	for _, r := range []store.AuthInviteRow{inv, plain} {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.AuthInviteGet(ctx, "inv-mgr"); err != nil || !reflect.DeepEqual(got, inv) {
		t.Errorf("get = %+v, %v; want %+v", got, err, inv)
	}
	if got, err := s.AuthInviteGetByCode(ctx, "code-mgr"); err != nil || !got.IssuedByManager {
		t.Errorf("get by code = %+v, %v; want issued by a manager", got, err)
	}
	if got, err := s.AuthInviteGet(ctx, "inv-op"); err != nil || got.IssuedByManager {
		t.Errorf("operator invite = %+v, %v; want not issued by a manager", got, err)
	}
	list, err := s.AuthInviteList(ctx, pwgGate, "acme")
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, r := range list {
		if r.IssuedByManager != (r.InviteID == "inv-mgr") {
			t.Errorf("listed %s issued_by_manager = %v", r.InviteID, r.IssuedByManager)
		}
	}
	rst := pwgReset("rst-mgr", "rcode-mgr", "acme", "alice", pwgT0)
	rst.IssuedByManager = true
	if err := s.AuthResetCreate(ctx, rst); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AuthResetGetByCode(ctx, "rcode-mgr"); err != nil || !reflect.DeepEqual(got, rst) {
		t.Errorf("reset = %+v, %v; want %+v", got, err, rst)
	}
	if err := s.AuthResetCreate(ctx, pwgReset("rst-op", "rcode-op", "acme", "bob", pwgT0)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AuthResetGetByCode(ctx, "rcode-op"); err != nil || got.IssuedByManager {
		t.Errorf("operator reset = %+v, %v; want not issued by a manager", got, err)
	}
}

// pwgAttachInvite is an invite that attaches a password to an existing
// subject, live from created for an hour.
func pwgAttachInvite(id, code, tenant, subject string, created time.Time, byManager bool) store.AuthInviteRow {
	return store.AuthInviteRow{InviteID: id, CodeHash: code, Gate: pwgGate, TenantID: tenant, Subject: subject,
		IssuedByManager: byManager, CreatedBy: "mgr", CreatedAt: created, ExpiresAt: created.Add(time.Hour)}
}

// AuthRevokeManagerIssued closes only the live, manager-issued links of
// exactly (tenant, subject): an operator's link to the same subject, another
// subject's, the same subject's in another tenant, and dead links all stay
// as they were.
func testAuthRevokeManagerIssuedClosesOnlyThatSubjectsManagerLinks(t *testing.T, s store.Store) {
	ctx := context.Background()
	at := pwgT0.Add(10 * time.Minute)
	invites := []store.AuthInviteRow{
		pwgAttachInvite("inv-mgr", "c-mgr", "acme", "alice", pwgT0, true),
		pwgAttachInvite("inv-op", "c-op", "acme", "alice", pwgT0, false),
		pwgAttachInvite("inv-bob", "c-bob", "acme", "bob", pwgT0, true),
		pwgAttachInvite("inv-other", "c-other", "other", "alice", pwgT0, true),
		// Expired before `at`: not live, so not touched.
		pwgAttachInvite("inv-old", "c-old", "acme", "alice", pwgT0.Add(-2*time.Hour), true),
	}
	for _, r := range invites {
		if err := s.AuthInviteCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// One live reset per subject is the reset contract, so alice's operator
	// reset is checked first and her manager reset after it.
	opReset := pwgReset("rst-op", "r-op", "acme", "alice", pwgT0)
	bobReset := pwgReset("rst-bob", "r-bob", "acme", "bob", pwgT0)
	bobReset.IssuedByManager = true
	for _, r := range []store.AuthResetRow{opReset, bobReset} {
		if err := s.AuthResetCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.AuthRevokeManagerIssued(ctx, "acme", "alice", at); err != nil || n != 1 {
		t.Fatalf("revoke = (%d, %v), want (1, nil): only inv-mgr", n, err)
	}
	if got, _ := s.AuthInviteGet(ctx, "inv-mgr"); got.Live(at) || !got.RevokedAt.Equal(at) {
		t.Errorf("manager invite after revoke = %+v; want revoked at %v", got, at)
	}
	for _, id := range []string{"inv-op", "inv-bob", "inv-other"} {
		if got, _ := s.AuthInviteGet(ctx, id); !got.Live(at) {
			t.Errorf("%s = %+v; want still live", id, got)
		}
	}
	if got, _ := s.AuthInviteGet(ctx, "inv-old"); !got.RevokedAt.IsZero() {
		t.Errorf("an expired invite was stamped revoked: %+v", got)
	}
	if got, _ := s.AuthResetGetByCode(ctx, "r-op"); !got.Live(at) {
		t.Errorf("operator reset = %+v; want still live", got)
	}

	mgrReset := pwgReset("rst-mgr", "r-mgr", "acme", "alice", pwgT0.Add(time.Minute))
	mgrReset.IssuedByManager = true
	if err := s.AuthResetCreate(ctx, mgrReset); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AuthRevokeManagerIssued(ctx, "acme", "alice", at); err != nil || n != 1 {
		t.Fatalf("second revoke = (%d, %v), want (1, nil): only rst-mgr", n, err)
	}
	if got, _ := s.AuthResetGetByCode(ctx, "r-mgr"); got.Live(at) || !got.UsedAt.Equal(at) {
		t.Errorf("manager reset after revoke = %+v; want used at %v", got, at)
	}
	if got, _ := s.AuthResetGetByCode(ctx, "r-bob"); !got.Live(at) {
		t.Errorf("another subject's manager reset = %+v; want still live", got)
	}
	if n, err := s.AuthRevokeManagerIssued(ctx, "acme", "alice", at); err != nil || n != 0 {
		t.Errorf("repeat revoke = (%d, %v), want (0, nil)", n, err)
	}
}

// pwgSeedManagerLinks gives subject a live manager-issued reset and invite
// (live at the real clock: the store hooks revoke at time.Now) and returns a
// check reporting whether each is still live.
func pwgSeedManagerLinks(t *testing.T, s store.Store, tenant, subject string) func() (resetLive, inviteLive bool) {
	t.Helper()
	ctx := context.Background()
	created := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	rst := pwgReset("rst-"+subject, "r-"+subject, tenant, subject, created)
	rst.IssuedByManager = true
	if err := s.AuthResetCreate(ctx, rst); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthInviteCreate(ctx, pwgAttachInvite("inv-"+subject, "c-"+subject, tenant, subject, created, true)); err != nil {
		t.Fatal(err)
	}
	return func() (bool, bool) {
		t.Helper()
		now := time.Now().UTC()
		r, err := s.AuthResetGetByCode(ctx, "r-"+subject)
		if err != nil {
			t.Fatal(err)
		}
		i, err := s.AuthInviteGet(ctx, "inv-"+subject)
		if err != nil {
			t.Fatal(err)
		}
		return r.Live(now), i.Live(now)
	}
}

// Every write that raises a user's authority closes that user's
// manager-issued links, whichever transport called the store.
func testAuthorityRaisingWritesRevokeManagerIssuedLinks(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "admins"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "promoted", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	tenantMode := "tenant"
	cases := []struct {
		name, subject string
		setup         func() error
		write         func() error
	}{
		{name: "GroupMemberAddUser", subject: "member", write: func() error {
			return s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "admins", MemberKind: store.GroupMemberUser, MemberID: "member"})
		}},
		{name: "AclBindingPutUser", subject: "bound", write: func() error {
			_, err := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "admin", SubjectKind: "user", SubjectID: "bound"})
			return err
		}},
		{name: "AclBindingPutToken", subject: "tok-owner",
			// The token is minted before the links are seeded, so its own
			// create-time revoke cannot be what closes them.
			setup: func() error {
				_, err := s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("def-tok", "tok", "acme", "tok-owner", "hash-tok", nil))
				return err
			},
			write: func() error {
				_, err := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "admin", SubjectKind: "token", SubjectID: "def-tok"})
				return err
			}},
		// A snapshot restore that adds the same authority is the same write.
		{name: "SnapshotRestoreGroupMemberUser", subject: "restored-member", write: func() error {
			ok, err := s.SnapshotRestoreGroupMember(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "admins", MemberKind: store.GroupMemberUser, MemberID: "restored-member"})
			if err == nil && !ok {
				err = errors.New("the membership was not inserted")
			}
			return err
		}},
		{name: "SnapshotRestoreAclBindingUser", subject: "restored-bound", write: func() error {
			ok, err := s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{BindingID: "aclb_restored", TenantID: "acme", ACLName: "admin", SubjectKind: "user", SubjectID: "restored-bound"})
			if err == nil && !ok {
				err = errors.New("the binding was not inserted")
			}
			return err
		}},
		{name: "UserUpdateToTenant", subject: "promoted", write: func() error {
			return s.UserUpdate(ctx, "acme", "promoted", store.UserPatch{AccessMode: &tenantMode})
		}},
		{name: "OperatorTokenDefCreate", subject: "minted", write: func() error {
			_, err := s.OperatorTokenDefCreate(ctx, mkOperatorTokenDef("def-minted", "minted", "acme", "minted", "hash-minted", nil))
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != nil {
				if err := c.setup(); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			live := pwgSeedManagerLinks(t, s, "acme", c.subject)
			if r, i := live(); !r || !i {
				t.Fatalf("seeded links not live: reset %v, invite %v", r, i)
			}
			if err := c.write(); err != nil {
				t.Fatalf("write: %v", err)
			}
			if r, i := live(); r || i {
				t.Errorf("after %s: reset live %v, invite live %v; want both closed", c.name, r, i)
			}
		})
	}
	// A binding to a token def that does not exist grants no one anything:
	// it is stored and revokes nothing.
	if _, err := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "admin", SubjectKind: "token", SubjectID: "def-missing"}); err != nil {
		t.Errorf("binding to a missing token def: %v", err)
	}
}

// Writes that raise no user's authority leave its manager-issued links live.
// The member group shares the seeded user's name, so a hook that ignored
// member_kind would revoke here.
func testNonRaisingWritesKeepManagerIssuedLinks(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, g := range []string{"parent", "dora"} {
		if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "erin", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	doraLive := pwgSeedManagerLinks(t, s, "acme", "dora")
	if err := s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "parent", MemberKind: store.GroupMemberGroup, MemberID: "dora"}); err != nil {
		t.Fatal(err)
	}
	if r, i := doraLive(); !r || !i {
		t.Errorf("after adding group dora: reset live %v, invite live %v; want both live", r, i)
	}
	erinLive := pwgSeedManagerLinks(t, s, "acme", "erin")
	name, isolated := "Erin", "isolated"
	if err := s.UserUpdate(ctx, "acme", "erin", store.UserPatch{DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserUpdate(ctx, "acme", "erin", store.UserPatch{AccessMode: &isolated}); err != nil {
		t.Fatal(err)
	}
	if r, i := erinLive(); !r || !i {
		t.Errorf("after a display-name and an isolated update: reset live %v, invite live %v; want both live", r, i)
	}

	// Re-restoring a membership and a binding the store already holds, and
	// restoring a binding that is retired, grant nothing new.
	if err := s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "parent", MemberKind: store.GroupMemberUser, MemberID: "fay"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_fay", TenantID: "acme", ACLName: "admin", SubjectKind: "user", SubjectID: "fay"}); err != nil {
		t.Fatal(err)
	}
	fayLive := pwgSeedManagerLinks(t, s, "acme", "fay")
	if ok, err := s.SnapshotRestoreGroupMember(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "parent", MemberKind: store.GroupMemberUser, MemberID: "fay"}); err != nil || ok {
		t.Errorf("re-restore membership = (%v, %v); want (false, nil)", ok, err)
	}
	if ok, err := s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{BindingID: "aclb_fay", TenantID: "acme", ACLName: "admin", SubjectKind: "user", SubjectID: "fay"}); err != nil || ok {
		t.Errorf("re-restore binding = (%v, %v); want (false, nil)", ok, err)
	}
	if ok, err := s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{BindingID: "aclb_fay_retired", TenantID: "acme", ACLName: "admin", SubjectKind: "user", SubjectID: "fay",
		RetiredAt: time.Now().UTC()}); err != nil || !ok {
		t.Errorf("restore retired binding = (%v, %v); want (true, nil)", ok, err)
	}
	if r, i := fayLive(); !r || !i {
		t.Errorf("after re-restoring held rows and a retired binding: reset live %v, invite live %v; want both live", r, i)
	}
}

// TenantBootstrapMark is insert-or-ignore (an existing marker keeps its time
// and route) and TenantBootstrapList returns every marker by tenant.
func testTenantBootstrapMarkIsInsertOrIgnoreAndListsByTenant(t *testing.T, s store.Store) {
	ctx := context.Background()
	if got, err := s.TenantBootstrapList(ctx); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v, %v; want an empty slice", got, err)
	}
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "zeta", Subject: "u", AccessMode: "isolated", Status: "active", CreatedAt: pwgT0}); err != nil {
		t.Fatal(err)
	}
	first := store.TenantBootstrapRow{TenantID: "acme", At: pwgT0.Add(time.Hour), Via: "snapshot"}
	if err := s.TenantBootstrapMark(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.TenantBootstrapMark(ctx, store.TenantBootstrapRow{TenantID: "acme", At: pwgT0.Add(2 * time.Hour), Via: "later"}); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if err := s.TenantBootstrapMark(ctx, store.TenantBootstrapRow{TenantID: "zeta", At: pwgT0.Add(3 * time.Hour), Via: "later"}); err != nil {
		t.Fatalf("mark over a users marker: %v", err)
	}
	want := []store.TenantBootstrapRow{first, {TenantID: "zeta", At: pwgT0, Via: "users"}}
	if got, err := s.TenantBootstrapList(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("list = %+v, %v; want %+v", got, err, want)
	}
	if ok, err := s.TenantBootstrapped(ctx, "acme"); err != nil || !ok {
		t.Errorf("acme bootstrapped = (%v, %v); a marker alone must count", ok, err)
	}
	// A marker of unknown time is stamped when it is written.
	before := time.Now().Add(-time.Second)
	if err := s.TenantBootstrapMark(ctx, store.TenantBootstrapRow{TenantID: "beta", Via: "backfill"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.TenantBootstrapList(ctx)
	if err != nil || len(got) != 3 || got[1].TenantID != "beta" || got[1].At.Before(before) {
		t.Errorf("list after a zero-time mark = %+v, %v; want beta second, stamped after %v", got, err, before)
	}
}
