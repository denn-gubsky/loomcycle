package storetest

import (
	"bytes"
	"context"
	"errors"
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

// The removal window counts only the gate's removals in the tenant.
func testSCIMRemovalsSinceCountsTheGatesRemovals(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, r := range []store.AuthRemovalRow{
		{TenantID: "acme", Subject: "a1", Gate: "okta", Reason: store.RemovalReasonRevoked, DisabledAt: pwgT0, UnbindDueAt: pwgT0.Add(time.Hour)},
		{TenantID: "acme", Subject: "a2", Gate: "okta", Reason: store.RemovalReasonRevoked, DisabledAt: pwgT0.Add(2 * time.Hour), UnbindDueAt: pwgT0.Add(3 * time.Hour)},
		{TenantID: "acme", Subject: "a3", Gate: "other", Reason: store.RemovalReasonRevoked, DisabledAt: pwgT0.Add(2 * time.Hour), UnbindDueAt: pwgT0.Add(3 * time.Hour)},
		{TenantID: "beta", Subject: "a4", Gate: "okta", Reason: store.RemovalReasonRevoked, DisabledAt: pwgT0.Add(2 * time.Hour), UnbindDueAt: pwgT0.Add(3 * time.Hour)},
	} {
		if ok, err := s.AuthRemovalStart(ctx, r); !ok || err != nil {
			t.Fatalf("removal %d: %v %v", i, ok, err)
		}
	}
	if n, err := s.SCIMRemovalsSince(ctx, "okta", "acme", pwgT0.Add(time.Hour)); n != 1 || err != nil {
		t.Fatalf("since +1h = %d, %v; want 1", n, err)
	}
	if n, _ := s.SCIMRemovalsSince(ctx, "okta", "acme", pwgT0); n != 2 {
		t.Fatalf("since t0 = %d; want 2", n)
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
