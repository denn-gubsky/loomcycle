package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P6 contract: passkeys (a counter that never goes backwards, flags
// that stick), single-use WebAuthn ceremonies, and SCIM resources confined to
// their gate and tenant.

func pkRow(tenant, subject string, id byte) store.UserPasskeyRow {
	return store.UserPasskeyRow{
		CredentialID: []byte{0xc0, id, 0x01, 0xff}, TenantID: tenant, Subject: subject,
		UserHandle: []byte("handle-" + subject), PublicKey: []byte{0xa5, 0x01, 0x02, id},
		AttestationType: "none", AttestationFormat: "none", Transports: []string{"usb", "internal"},
		Flags: 0x45, AAGUID: bytes.Repeat([]byte{id}, 16), SignCount: 5, Attachment: "cross-platform",
		Discoverable: true, Name: "key " + subject, CreatedAt: pwgT0,
	}
}

func testUserPasskeyRoundTripAndConflict(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := pkRow("acme", "alice", 1)
	if err := s.UserPasskeyCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserPasskeyGet(ctx, row.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, row.CreatedAt)
	}
	got.CreatedAt = row.CreatedAt
	if !reflect.DeepEqual(got, row) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, row)
	}
	// A credential id is registered once, whoever asks.
	dup := pkRow("other", "mallory", 1)
	var conflict *store.ErrConflict
	if err := s.UserPasskeyCreate(ctx, dup); !errors.As(err, &conflict) || conflict.Kind != "user_passkey" {
		t.Fatalf("duplicate credential id: %v", err)
	}
	if err := s.UserPasskeyCreate(ctx, pkRow("acme", "alice", 2)); err != nil {
		t.Fatal(err)
	}
	list, err := s.UserPasskeyList(ctx, "acme", "alice")
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	if l, _ := s.UserPasskeyList(ctx, "other", "alice"); len(l) != 0 {
		t.Fatal("another tenant's subject of the same name sees the passkeys")
	}
	if _, err := s.UserPasskeyGet(ctx, []byte("nope")); !pwgNotFound(err) {
		t.Fatalf("unknown id: %v", err)
	}
}

// The counter is a compare-and-set: it only moves forward, a flagged passkey
// never moves, and of two racing uses with the same counter only one wins.
func testUserPasskeyRecordUseOnlyMovesForward(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := pkRow("acme", "bob", 3)
	if err := s.UserPasskeyCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	at := pwgT0.Add(time.Minute)
	for _, c := range []struct {
		count uint32
		want  bool
	}{{6, true}, {6, false}, {4, false}, {9, true}} {
		ok, err := s.UserPasskeyRecordUse(ctx, row.CredentialID, c.count, 0x05, at)
		if err != nil || ok != c.want {
			t.Fatalf("use with count %d: %v, %v (want %v)", c.count, ok, err, c.want)
		}
	}
	got, _ := s.UserPasskeyGet(ctx, row.CredentialID)
	if got.SignCount != 9 || got.Flags != 0x05 || !got.LastUsedAt.Equal(at) {
		t.Fatalf("after uses: %+v", got)
	}
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.UserPasskeyRecordUse(ctx, row.CredentialID, 10, 0x05, at); err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d racing uses of one counter value won, want 1", wins)
	}
	if ok, err := s.UserPasskeyFlag(ctx, row.CredentialID, "sign_count_regressed", at); !ok || err != nil {
		t.Fatalf("flag: %v %v", ok, err)
	}
	if ok, _ := s.UserPasskeyFlag(ctx, row.CredentialID, "other", at); ok {
		t.Fatal("a second flag must keep the first reason")
	}
	if ok, _ := s.UserPasskeyRecordUse(ctx, row.CredentialID, 50, 0x05, at); ok {
		t.Fatal("a flagged passkey's counter moved")
	}
	got, _ = s.UserPasskeyGet(ctx, row.CredentialID)
	if got.FlaggedAt.IsZero() || got.FlagReason != "sign_count_regressed" {
		t.Fatalf("flag not kept: %+v", got)
	}
	// Authenticators without a counter report zero every time.
	zero := pkRow("acme", "bob", 4)
	zero.SignCount = 0
	if err := s.UserPasskeyCreate(ctx, zero); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if ok, err := s.UserPasskeyRecordUse(ctx, zero.CredentialID, 0, 0x05, at); !ok || err != nil {
			t.Fatalf("zero counter use %d: %v %v", i, ok, err)
		}
	}
	if ok, _ := s.UserPasskeyRecordUse(ctx, zero.CredentialID, 1, 0x05, at); !ok {
		t.Fatal("a counter starting from zero must be able to move")
	}
	if ok, _ := s.UserPasskeyRecordUse(ctx, zero.CredentialID, 0, 0x05, at); ok {
		t.Fatal("a counter that started must not fall back to zero")
	}
}

func testUserPasskeyDeleteIsConfinedToItsSubject(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, b := pkRow("acme", "carol", 5), pkRow("acme", "dave", 6)
	for _, r := range []store.UserPasskeyRow{a, b} {
		if err := s.UserPasskeyCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := s.UserPasskeyDelete(ctx, "acme", "carol", b.CredentialID); ok {
		t.Fatal("carol deleted dave's passkey")
	}
	if ok, _ := s.UserPasskeyRename(ctx, "acme", "carol", b.CredentialID, "stolen"); ok {
		t.Fatal("carol renamed dave's passkey")
	}
	if ok, err := s.UserPasskeyRename(ctx, "acme", "carol", a.CredentialID, "laptop"); !ok || err != nil {
		t.Fatalf("rename own: %v %v", ok, err)
	}
	if got, _ := s.UserPasskeyGet(ctx, a.CredentialID); got.Name != "laptop" {
		t.Fatalf("name = %q", got.Name)
	}
	if ok, err := s.UserPasskeyDelete(ctx, "acme", "carol", a.CredentialID); !ok || err != nil {
		t.Fatalf("delete own: %v %v", ok, err)
	}
	if n, err := s.UserPasskeyDeleteSubject(ctx, "acme", "dave"); n != 1 || err != nil {
		t.Fatalf("delete subject: %d %v", n, err)
	}
}

func testWebAuthnCeremonyIsTakenOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	row := store.WebAuthnCeremonyRow{CeremonyHash: "cer-1", Kind: store.CeremonyLogin, Gate: "keys", Binding: "login:keys",
		SessionData: `{"challenge":"abc"}`, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err := s.WebAuthnCeremonyCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	takes := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.WebAuthnCeremonyTake(ctx, "cer-1", now); err == nil && got.Binding == "login:keys" && got.SessionData == row.SessionData {
				mu.Lock()
				takes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if takes != 1 {
		t.Fatalf("a ceremony was taken %d times, want once", takes)
	}
	expired := row
	expired.CeremonyHash, expired.ExpiresAt = "cer-2", now.Add(-time.Second)
	if err := s.WebAuthnCeremonyCreate(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WebAuthnCeremonyTake(ctx, "cer-2", now); !pwgNotFound(err) {
		t.Fatalf("an expired ceremony: %v", err)
	}
	if n, err := s.WebAuthnCeremonyDeleteExpired(ctx, now); n != 1 || err != nil {
		t.Fatalf("delete expired: %d %v", n, err)
	}
}

func scimUser(tenant, id, name string) store.SCIMUserRow {
	return store.SCIMUserRow{Gate: "okta", TenantID: tenant, SCIMID: id, Subject: "okta:" + id,
		UserName: name, UserNameKey: strings.ToLower(name), ExternalID: "ext-" + id, Attributes: `{"displayName":"x"}`}
}

// A SCIM user resource lives in exactly one gate and tenant: another tenant
// (or gate) neither reads nor changes it, a live userName is unique per
// tenant (case-folded by the caller), and a deleted resource frees its name.
func testSCIMUserLifecycleIsTenantConfined(t *testing.T, s store.Store) {
	ctx := context.Background()
	u := scimUser("acme", "u1", "Alice@Acme.example")
	if err := s.SCIMUserCreate(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := s.SCIMUserGet(ctx, "okta", "acme", "u1")
	if err != nil || got.Subject != "okta:u1" || got.UserName != u.UserName || got.Version != 1 || got.Attributes != u.Attributes {
		t.Fatalf("get: %+v %v", got, err)
	}
	for _, other := range [][2]string{{"okta", "beta"}, {"other", "acme"}} {
		if _, err := s.SCIMUserGet(ctx, other[0], other[1], "u1"); !pwgNotFound(err) {
			t.Fatalf("%v reads acme's resource: %v", other, err)
		}
	}
	// The same name in another tenant is fine; in this one it is a conflict.
	if err := s.SCIMUserCreate(ctx, scimUser("beta", "u1", "alice@acme.example")); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	if err := s.SCIMUserCreate(ctx, scimUser("acme", "u2", "ALICE@acme.example")); !errors.As(err, &conflict) || conflict.Kind != "scim_user_name" {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := s.SCIMUserCreate(ctx, scimUser("acme", "u3", "carol")); err != nil {
		t.Fatal(err)
	}
	rows, total, err := s.SCIMUserList(ctx, "okta", "acme", store.SCIMFilter{}, 0, 10)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("list: %d/%d %v", len(rows), total, err)
	}
	rows, total, _ = s.SCIMUserList(ctx, "okta", "acme", store.SCIMFilter{UserNameKey: "carol"}, 0, 10)
	if total != 1 || len(rows) != 1 || rows[0].SCIMID != "u3" {
		t.Fatalf("filter: %+v", rows)
	}
	if rows, total, _ = s.SCIMUserList(ctx, "okta", "acme", store.SCIMFilter{}, 1, 10); total != 2 || len(rows) != 1 {
		t.Fatalf("offset: %d/%d", len(rows), total)
	}
	upd := got
	upd.UserName, upd.UserNameKey = "carol", "carol"
	if err := s.SCIMUserUpdate(ctx, upd); !errors.As(err, &conflict) || conflict.Kind != "scim_user_name" {
		t.Fatalf("rename into a taken name: %v", err)
	}
	upd.UserName, upd.UserNameKey = "alice2", "alice2"
	if err := s.SCIMUserUpdate(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SCIMUserGet(ctx, "okta", "acme", "u1"); got.Version != 2 || got.UserName != "alice2" {
		t.Fatalf("after update: %+v", got)
	}
	// beta has no u3: an update naming beta changes nothing in acme.
	cross := upd
	cross.TenantID, cross.SCIMID, cross.UserName, cross.UserNameKey = "beta", "u3", "hijack", "hijack"
	if err := s.SCIMUserUpdate(ctx, cross); !pwgNotFound(err) {
		t.Fatalf("cross-tenant update: %v", err)
	}
	if got, _ := s.SCIMUserGet(ctx, "okta", "acme", "u3"); got.UserName != "carol" {
		t.Fatalf("a cross-tenant update reached acme: %+v", got)
	}
	if ok, err := s.SCIMUserMarkDeleted(ctx, "okta", "beta", "u3", pwgT0); ok || err != nil {
		t.Fatalf("cross-tenant delete: %v %v", ok, err)
	}
	if ok, err := s.SCIMUserMarkDeleted(ctx, "okta", "acme", "u1", pwgT0); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.SCIMUserMarkDeleted(ctx, "okta", "acme", "u1", pwgT0); ok {
		t.Fatal("a second delete reported a change")
	}
	if _, err := s.SCIMUserGet(ctx, "okta", "acme", "u1"); !pwgNotFound(err) {
		t.Fatalf("a deleted resource still reads: %v", err)
	}
	// Its name is free again.
	if err := s.SCIMUserCreate(ctx, scimUser("acme", "u4", "alice2")); err != nil {
		t.Fatalf("reuse a deleted name: %v", err)
	}
	if err := s.SCIMUserDiscard(ctx, "okta", "acme", "u4"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SCIMUserGet(ctx, "okta", "acme", "u4"); !pwgNotFound(err) {
		t.Fatalf("a discarded resource still reads: %v", err)
	}
}

// Group resources and their memberships: the membership is what the
// provider asserts, confined to the tenant, removed with the group or with
// a deleted user, and listed per subject.
func testSCIMGroupLifecycle(t *testing.T, s store.Store) {
	ctx := context.Background()
	g := store.SCIMGroupRow{Gate: "okta", TenantID: "acme", SCIMID: "g1", DisplayName: "Engineering", DisplayKey: "engineering"}
	if err := s.SCIMGroupCreate(ctx, g); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	dup := g
	dup.SCIMID = "g2"
	if err := s.SCIMGroupCreate(ctx, dup); !errors.As(err, &conflict) || conflict.Kind != "scim_group_name" {
		t.Fatalf("duplicate display name: %v", err)
	}
	other := g
	other.TenantID = "beta"
	if err := s.SCIMGroupCreate(ctx, other); err != nil {
		t.Fatalf("same name in another tenant: %v", err)
	}
	if _, err := s.SCIMGroupGet(ctx, "okta", "beta", "g9"); !pwgNotFound(err) {
		t.Fatalf("unknown: %v", err)
	}
	for _, u := range []store.SCIMUserRow{scimUser("acme", "m1", "m1"), scimUser("acme", "m2", "m2")} {
		if err := s.SCIMUserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	added, err := s.SCIMGroupMembersAdd(ctx, "okta", "acme", "g1", []string{"okta:m2", "okta:m1", "okta:m1"})
	if err != nil || !reflect.DeepEqual(added, []string{"okta:m1", "okta:m2"}) {
		t.Fatalf("add: %v %v", added, err)
	}
	if added, _ := s.SCIMGroupMembersAdd(ctx, "okta", "acme", "g1", []string{"okta:m1"}); len(added) != 0 {
		t.Fatalf("a repeated add changed %v", added)
	}
	if m, _ := s.SCIMGroupMembers(ctx, "okta", "beta", "g1"); len(m) != 0 {
		t.Fatalf("beta's g1 sees acme's members: %v", m)
	}
	gs, err := s.SCIMGroupsOfSubject(ctx, "okta", "acme", "okta:m1")
	if err != nil || len(gs) != 1 || gs[0].DisplayName != "Engineering" {
		t.Fatalf("groups of m1: %+v %v", gs, err)
	}
	upd := g
	upd.DisplayName, upd.DisplayKey = "Eng", "eng"
	if err := s.SCIMGroupUpdate(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if rows, total, _ := s.SCIMGroupList(ctx, "okta", "acme", store.SCIMFilter{DisplayKey: "eng"}, 0, 10); total != 1 || rows[0].Version != 2 {
		t.Fatalf("filter after rename: %+v", rows)
	}
	removed, err := s.SCIMGroupMembersRemove(ctx, "okta", "acme", "g1", []string{"okta:m2", "okta:nobody"})
	if err != nil || !reflect.DeepEqual(removed, []string{"okta:m2"}) {
		t.Fatalf("remove: %v %v", removed, err)
	}
	// Deleting a user resource drops its memberships.
	if ok, _ := s.SCIMUserMarkDeleted(ctx, "okta", "acme", "m1", pwgT0); !ok {
		t.Fatal("delete m1")
	}
	if m, _ := s.SCIMGroupMembers(ctx, "okta", "acme", "g1"); len(m) != 0 {
		t.Fatalf("a deleted user is still a member: %v", m)
	}
	if _, err := s.SCIMGroupMembersAdd(ctx, "okta", "acme", "g1", []string{"okta:m2"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SCIMGroupDelete(ctx, "okta", "acme", "g1"); !ok || err != nil {
		t.Fatalf("delete group: %v %v", ok, err)
	}
	if gs, _ := s.SCIMGroupsOfSubject(ctx, "okta", "acme", "okta:m2"); len(gs) != 0 {
		t.Fatalf("memberships survived their group: %+v", gs)
	}
	if _, err := s.SCIMGroupGet(ctx, "okta", "beta", "g1"); err != nil {
		t.Fatalf("beta's group went with acme's: %v", err)
	}
}

// scimRemovalRow is a SCIM gate's removal of subject at now.
func scimRemovalRow(tenant, subject, gate string, at time.Time) store.AuthRemovalRow {
	return store.AuthRemovalRow{TenantID: tenant, Subject: subject, Gate: gate, Reason: store.RemovalReasonSCIM,
		DisabledAt: at, UnbindDueAt: at.Add(time.Hour), CreatedBy: "gate:" + gate}
}

func userStatus(t *testing.T, s store.Store, tenant, subject string) string {
	t.Helper()
	u, err := s.UserGet(context.Background(), tenant, subject)
	if err != nil {
		t.Fatalf("user %s: %v", subject, err)
	}
	return u.Status
}

// A SCIM removal is all or nothing under the gate's ceiling: concurrent
// removals never pass it together, a refused one leaves no row and no status
// change, and only the gate's own removals in the tenant count.
func testSCIMRemoveUserHoldsTheCeilingUnderConcurrency(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 12; i++ {
		if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: fmt.Sprintf("u%d", i), AccessMode: "isolated", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	// Outside the window, another gate's and another tenant's do not count.
	for _, r := range []store.AuthRemovalRow{scimRemovalRow("acme", "old", "okta", now.Add(-48*time.Hour)),
		scimRemovalRow("acme", "x1", "other", now), scimRemovalRow("beta", "x2", "okta", now)} {
		if ok, err := s.AuthRemovalStart(ctx, r); !ok || err != nil {
			t.Fatalf("seed: %v %v", ok, err)
		}
	}
	since := now.Add(-24 * time.Hour)
	var mu sync.Mutex
	var wg sync.WaitGroup
	removed, refused := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", fmt.Sprintf("u%d", i), "okta", now), since, 3)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && got:
				removed++
			case errors.Is(err, store.ErrSCIMRemovalCeiling):
				refused++
			default:
				t.Errorf("remove %d: %v %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	if removed != 3 || refused != 9 {
		t.Fatalf("removed %d and refused %d of 12 under a ceiling of 3", removed, refused)
	}
	disabled := 0
	for i := 0; i < 12; i++ {
		sub := fmt.Sprintf("u%d", i)
		st := userStatus(t, s, "acme", sub)
		_, rerr := s.AuthRemovalGet(ctx, "acme", sub)
		switch {
		case st == "disabled" && rerr == nil:
			disabled++
		case st == "active" && pwgNotFound(rerr):
		default:
			t.Fatalf("%s: status %s, removal %v — a removal must be all or nothing", sub, st, rerr)
		}
	}
	if disabled != 3 {
		t.Fatalf("%d users disabled, want 3", disabled)
	}
	if _, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", "nobody", "okta", now), since, 100); !pwgNotFound(err) {
		t.Fatalf("an unknown user: %v", err)
	}
}

// A user disabled by anyone else is not SCIM's to remove or re-enable, a
// pending user comes back pending, and an operator's removal takes a SCIM
// removal over.
func testSCIMRemoveAndReactivateKeepOwnership(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	since := now.Add(-24 * time.Hour)
	for _, u := range []store.UserRow{
		{TenantID: "acme", Subject: "op-off", AccessMode: "isolated", Status: "disabled"},
		{TenantID: "acme", Subject: "pend", AccessMode: "isolated", Status: store.UserStatusPending},
		{TenantID: "acme", Subject: "act", AccessMode: "isolated", Status: "active"},
	} {
		if err := s.UserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	// Disabled by an operator first: SCIM writes nothing and cannot re-enable.
	if got, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", "op-off", "okta", now), since, 10); got || err != nil {
		t.Fatalf("remove an operator-disabled user: %v %v", got, err)
	}
	if _, err := s.AuthRemovalGet(ctx, "acme", "op-off"); !pwgNotFound(err) {
		t.Fatalf("a SCIM row behind an operator's disable: %v", err)
	}
	if _, ok, err := s.SCIMReactivate(ctx, "okta", "acme", "op-off"); ok || err != nil {
		t.Fatalf("re-enable an operator-disabled user: %v %v", ok, err)
	}
	// Pending → removed → re-enabled is pending.
	if got, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", "pend", "okta", now), since, 10); !got || err != nil {
		t.Fatalf("remove pending: %v %v", got, err)
	}
	if rm, _ := s.AuthRemovalGet(ctx, "acme", "pend"); rm.Reason != store.RemovalReasonSCIMPending || userStatus(t, s, "acme", "pend") != "disabled" {
		t.Fatalf("pending removal: %+v, status %s", rm, userStatus(t, s, "acme", "pend"))
	}
	if ok, _ := s.SCIMReactivateClaim(ctx, "other-gate", "acme", "pend"); ok {
		t.Fatal("another gate claimed the SCIM gate's removal")
	}
	// A re-enable is a claim, then a finish: an unclaimed removal does not
	// finish, and the user stays disabled while it is claimed.
	if _, ok, _ := s.SCIMReactivate(ctx, "okta", "acme", "pend"); ok {
		t.Fatal("a re-enable finished without a claim")
	}
	for i := 0; i < 2; i++ { // a retried claim continues
		if ok, err := s.SCIMReactivateClaim(ctx, "okta", "acme", "pend"); !ok || err != nil {
			t.Fatalf("claim %d: %v %v", i, ok, err)
		}
	}
	if rm, _ := s.AuthRemovalGet(ctx, "acme", "pend"); rm.Reason != store.RemovalReasonSCIMReactivatingPending || userStatus(t, s, "acme", "pend") != "disabled" {
		t.Fatalf("claimed: %+v, status %s", rm, userStatus(t, s, "acme", "pend"))
	}
	if _, ok, _ := s.SCIMReactivate(ctx, "other-gate", "acme", "pend"); ok {
		t.Fatal("another gate finished the SCIM gate's claim")
	}
	if st, ok, err := s.SCIMReactivate(ctx, "okta", "acme", "pend"); !ok || err != nil || st != store.UserStatusPending {
		t.Fatalf("re-enable pending: %q %v %v", st, ok, err)
	}
	if userStatus(t, s, "acme", "pend") != store.UserStatusPending {
		t.Fatalf("status = %s, want pending", userStatus(t, s, "acme", "pend"))
	}
	if _, err := s.AuthRemovalGet(ctx, "acme", "pend"); !pwgNotFound(err) {
		t.Fatalf("the removal outlived the re-enable: %v", err)
	}
	// An operator's removal takes the SCIM removal over: SCIM cannot re-enable.
	if got, _ := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", "act", "okta", now), since, 10); !got {
		t.Fatal("remove active")
	}
	op := store.AuthRemovalRow{TenantID: "acme", Subject: "act", Reason: store.RemovalReasonOperator, DisabledAt: now.Add(time.Minute),
		UnbindDueAt: now.Add(2 * time.Hour), CreatedBy: "op"}
	// Even mid re-enable: the claim does not hold an operator off.
	if ok, err := s.SCIMReactivateClaim(ctx, "okta", "acme", "act"); !ok || err != nil {
		t.Fatalf("claim act: %v %v", ok, err)
	}
	if ok, err := s.AuthRemovalStart(ctx, op); !ok || err != nil {
		t.Fatalf("an operator's removal over SCIM's: %v %v", ok, err)
	}
	rm, _ := s.AuthRemovalGet(ctx, "acme", "act")
	if rm.Reason != store.RemovalReasonOperator || rm.Gate != "" || !rm.DisabledAt.Equal(now) {
		t.Fatalf("after the takeover: %+v (the reason and gate move, the timings stay)", rm)
	}
	if _, ok, _ := s.SCIMReactivate(ctx, "okta", "acme", "act"); ok {
		t.Fatal("SCIM re-enabled a user an operator's removal took over")
	}
	// A removal row behind a user who is still active is stale: SCIM
	// replaces it with a fresh row — its own timings, none of the stale
	// deadlines — so the sweeper does not unbind or erase on its next pass.
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "stale", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * 24 * time.Hour)
	stale := store.AuthRemovalRow{TenantID: "acme", Subject: "stale", Reason: store.RemovalReasonOperator, DisabledAt: old,
		UnbindDueAt: old.Add(24 * time.Hour), EraseDueAt: old.Add(48 * time.Hour), CreatedBy: "op"}
	if ok, err := s.AuthRemovalStart(ctx, stale); !ok || err != nil {
		t.Fatalf("seed a stale row: %v %v", ok, err)
	}
	fresh := scimRemovalRow("acme", "stale", "okta", now)
	if got, err := s.SCIMRemoveUser(ctx, fresh, since, 10); !got || err != nil {
		t.Fatalf("remove under a stale row: %v %v", got, err)
	}
	if st := userStatus(t, s, "acme", "stale"); st != "disabled" {
		t.Fatalf("under a stale row the user stayed %s", st)
	}
	rm, _ = s.AuthRemovalGet(ctx, "acme", "stale")
	if rm.Reason != store.RemovalReasonSCIM || rm.Gate != "okta" || !rm.DisabledAt.Equal(now) ||
		!rm.UnbindDueAt.Equal(fresh.UnbindDueAt) || !rm.EraseDueAt.IsZero() || !rm.UnboundAt.IsZero() {
		t.Fatalf("the stale row was not replaced by a fresh one: %+v", rm)
	}
	due, err := s.AuthRemovalDue(ctx, now.Add(time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.Subject == "stale" {
			t.Fatalf("the sweeper would act on the stale deadlines: %+v", d)
		}
	}
	// The ceiling holds on the stale path too, counted before any write.
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "stale2", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	stale.Subject = "stale2"
	if ok, err := s.AuthRemovalStart(ctx, stale); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	if _, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", "stale2", "okta", now), since, 1); !errors.Is(err, store.ErrSCIMRemovalCeiling) {
		t.Fatalf("a stale-row removal over the ceiling: %v", err)
	}
	if st := userStatus(t, s, "acme", "stale2"); st != "active" {
		t.Fatalf("a refused removal changed the user to %s", st)
	}
	if rm, _ := s.AuthRemovalGet(ctx, "acme", "stale2"); rm.Reason != store.RemovalReasonOperator {
		t.Fatalf("a refused removal changed the row: %+v", rm)
	}
	// A SCIM removal never takes an operator's over.
	if ok, _ := s.AuthRemovalStart(ctx, scimRemovalRow("acme", "act", "okta", now)); ok {
		t.Fatal("a SCIM row replaced an operator's")
	}
}

// An operator's disable disowns a SCIM removal (any SCIM reason, a claim
// included) and leaves every other removal alone.
func testSCIMRemovalDisownTakesOnlySCIMRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	since := now.Add(-24 * time.Hour)
	for _, sub := range []string{"s1", "s2"} {
		if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.SCIMRemoveUser(ctx, scimRemovalRow("acme", sub, "okta", now), since, 10); !got || err != nil {
			t.Fatalf("remove %s: %v %v", sub, got, err)
		}
	}
	if ok, err := s.SCIMReactivateClaim(ctx, "okta", "acme", "s2"); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	op := store.AuthRemovalRow{TenantID: "acme", Subject: "op", Reason: store.RemovalReasonOperator, DisabledAt: now,
		UnbindDueAt: now.Add(time.Hour), CreatedBy: "op"}
	if ok, err := s.AuthRemovalStart(ctx, op); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	for _, sub := range []string{"s1", "s2"} {
		if ok, err := s.SCIMRemovalDisown(ctx, "acme", sub); !ok || err != nil {
			t.Fatalf("disown %s: %v %v", sub, ok, err)
		}
		if _, err := s.AuthRemovalGet(ctx, "acme", sub); !pwgNotFound(err) {
			t.Fatalf("%s: the SCIM row outlived the disown: %v", sub, err)
		}
		if _, ok, _ := s.SCIMReactivate(ctx, "okta", "acme", sub); ok {
			t.Fatalf("%s: SCIM re-enabled a disowned user", sub)
		}
	}
	if ok, err := s.SCIMRemovalDisown(ctx, "acme", "op"); ok || err != nil {
		t.Fatalf("disowned an operator's removal: %v %v", ok, err)
	}
	if _, err := s.AuthRemovalGet(ctx, "acme", "op"); err != nil {
		t.Fatalf("the operator's removal went: %v", err)
	}
}

// The base of a percentage ceiling is the gate's linked users in the tenant
// who are not disabled and existed before the window; the creates in it are
// counted per gate and tenant, deleted ones too.
func testSCIMRemovalBaseAndCreatesCountTheWindow(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	before := now.Add(-24 * time.Hour)
	old := now.Add(-48 * time.Hour)
	for _, u := range []struct {
		tenant, sub, gate, status string
		at                        time.Time
	}{
		{"acme", "old-active", "okta", "active", old},
		{"acme", "old-pending", "okta", store.UserStatusPending, old},
		{"acme", "old-disabled", "okta", "disabled", old},
		{"acme", "old-removed-now", "okta", "disabled", old},
		{"acme", "old-removed-before", "okta", "disabled", old},
		{"acme", "new-active", "okta", "active", now},
		{"acme", "other-gate", "azure", "active", old},
		{"beta", "other-tenant", "okta", "active", old},
	} {
		if err := s.UserCreate(ctx, store.UserRow{TenantID: u.tenant, Subject: u.sub, AccessMode: "isolated", Status: u.status, CreatedAt: u.at}); err != nil {
			t.Fatal(err)
		}
		if err := s.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: u.tenant, Subject: u.sub, Gate: u.gate,
			ExternalSubject: "ext-" + u.sub, CreatedAt: u.at}); err != nil {
			t.Fatal(err)
		}
	}
	// Removed inside the window: still part of the base. Removed before it:
	// not.
	for _, r := range []store.AuthRemovalRow{scimRemovalRow("acme", "old-removed-now", "okta", now),
		scimRemovalRow("acme", "old-removed-before", "okta", old)} {
		if ok, err := s.AuthRemovalStart(ctx, r); !ok || err != nil {
			t.Fatalf("seed: %v %v", ok, err)
		}
	}
	if n, err := s.SCIMRemovalBase(ctx, "okta", "acme", before); n != 3 || err != nil {
		t.Fatalf("base = %d %v, want 3 (old-active, old-pending, old-removed-now)", n, err)
	}
	for i, at := range []time.Time{old, now, now, now} {
		r := scimUser("acme", fmt.Sprintf("c%d", i), fmt.Sprintf("c%d@acme.test", i))
		r.CreatedAt = at
		if err := s.SCIMUserCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SCIMUserCreate(ctx, scimUser("beta", "c9", "c9@beta.test")); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SCIMUserMarkDeleted(ctx, "okta", "acme", "c2", now); !ok || err != nil {
		t.Fatalf("mark deleted: %v %v", ok, err)
	}
	// A discarded create (its user was refused) made no user: not counted.
	if err := s.SCIMUserDiscard(ctx, "okta", "acme", "c3"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SCIMUsersCreatedSince(ctx, "okta", "acme", before); n != 2 || err != nil {
		t.Fatalf("created since = %d %v, want 2 (c1, and the deleted c2)", n, err)
	}
	got, err := s.SCIMUsersGetMany(ctx, "okta", "acme", []string{"c0", "c1", "c1", "c9", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["c0"].Subject != "okta:c0" || got["c1"].Subject != "okta:c1" {
		t.Fatalf("get many = %+v, want c0 and c1 only", got)
	}
}

// Two concurrent removals of a subject's two usable passkeys never take
// both, and a flagged passkey never stands in for a usable one.
func testUserPasskeyDeleteKeepingNeverTakesTheLastUsable(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, b, c := pkRow("acme", "erin", 7), pkRow("acme", "erin", 8), pkRow("acme", "erin", 9)
	for _, r := range []store.UserPasskeyRow{a, b, c} {
		if err := s.UserPasskeyCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UserPasskeyFlag(ctx, c.CredentialID, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	deleted, refused := 0, 0
	for _, id := range [][]byte{a.CredentialID, b.CredentialID} {
		wg.Add(1)
		go func(id []byte) {
			defer wg.Done()
			d, r, err := s.UserPasskeyDeleteKeeping(ctx, "acme", "erin", id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("delete: %v", err)
			case d:
				deleted++
			case r:
				refused++
			}
		}(id)
	}
	wg.Wait()
	if deleted != 1 || refused != 1 {
		t.Fatalf("deleted %d, refused %d of two usable passkeys removed together", deleted, refused)
	}
	if d, r, err := s.UserPasskeyDeleteKeeping(ctx, "acme", "erin", c.CredentialID); !d || r || err != nil {
		t.Fatalf("the flagged passkey: %v %v %v", d, r, err)
	}
	if d, r, err := s.UserPasskeyDeleteKeeping(ctx, "acme", "erin", []byte("nope")); d || r || err != nil {
		t.Fatalf("an unknown passkey: %v %v %v", d, r, err)
	}
	if d, r, _ := s.UserPasskeyDeleteKeeping(ctx, "acme", "frank", a.CredentialID); d || r {
		t.Fatal("another subject's call reached erin's passkey")
	}
}

// Deleting a user removes its passkeys and SCIM rows, and only its own.
func testAuthSubjectDeleteRemovesPasskeysAndSCIMRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, r := range []store.UserPasskeyRow{pkRow("acme", "okta:z1", 7), pkRow("acme", "okta:z2", 8)} {
		if err := s.UserPasskeyCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []store.SCIMUserRow{scimUser("acme", "z1", "z1"), scimUser("acme", "z2", "z2")} {
		if err := s.SCIMUserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	g := store.SCIMGroupRow{Gate: "okta", TenantID: "acme", SCIMID: "gz", DisplayName: "Z", DisplayKey: "z"}
	if err := s.SCIMGroupCreate(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SCIMGroupMembersAdd(ctx, "okta", "acme", "gz", []string{"okta:z1", "okta:z2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthSubjectDelete(ctx, "acme", "okta:z1"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.UserPasskeyList(ctx, "acme", "okta:z1"); len(l) != 0 {
		t.Fatalf("passkeys outlived their user: %d", len(l))
	}
	if _, err := s.SCIMUserGet(ctx, "okta", "acme", "z1"); !pwgNotFound(err) {
		t.Fatalf("the SCIM resource outlived its user: %v", err)
	}
	if m, _ := s.SCIMGroupMembers(ctx, "okta", "acme", "gz"); !reflect.DeepEqual(m, []string{"okta:z2"}) {
		t.Fatalf("members = %v, want only z2", m)
	}
	if l, _ := s.UserPasskeyList(ctx, "acme", "okta:z2"); len(l) != 1 {
		t.Fatal("another user's passkey went too")
	}
}
