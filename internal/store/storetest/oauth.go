package storetest

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FL contract: app sign-in codes, refresh families and refresh tokens.

func oauthCode(hash string, expires time.Time) store.OAuthCodeRow {
	return store.OAuthCodeRow{CodeHash: hash, ClientID: "board", RedirectURI: "https://board.example/cb",
		CodeChallenge: "chal", TenantID: "acme", Subject: "alice", Gate: "pw", AMR: []string{"pwd", "otp"},
		SessionHash: "sess-1", ScopeCap: []string{"runs:read"}, RestrictionsJSON: `{"retention":"none"}`,
		CreatedAt: pwgT0, ExpiresAt: expires}
}

func oauthFamily(id, client string) store.OAuthFamilyRow {
	return store.OAuthFamilyRow{FamilyID: id, ClientID: client, TenantID: "acme", Subject: "alice", Gate: "pw",
		AMR: []string{"pwd"}, ScopeCap: []string{"runs:read", "runs:create"}, CreatedAt: pwgT0,
		IdleExpiresAt: pwgT0.Add(7 * 24 * time.Hour), AbsoluteExpiresAt: pwgT0.Add(30 * 24 * time.Hour)}
}

func testOAuthCodeConsumesOnceAndKeepsTheFirstFamily(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := oauthCode("c-1", pwgT0.Add(time.Minute))
	if err := s.OAuthCodeCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	if err := s.OAuthCodeCreate(ctx, row); !errors.As(err, &conflict) {
		t.Fatalf("duplicate code: %v", err)
	}
	now := pwgT0.Add(10 * time.Second)
	got, first, err := s.OAuthCodeConsume(ctx, "c-1", "fam-1", now)
	if err != nil || !first {
		t.Fatalf("first consume: %v %v", first, err)
	}
	if got.ClientID != "board" || got.RedirectURI != row.RedirectURI || got.CodeChallenge != "chal" || got.TenantID != "acme" ||
		got.Subject != "alice" || got.Gate != "pw" || len(got.AMR) != 2 || got.SessionHash != "sess-1" ||
		len(got.ScopeCap) != 1 || got.RestrictionsJSON != row.RestrictionsJSON || !got.CreatedAt.Equal(pwgT0) ||
		!got.ExpiresAt.Equal(row.ExpiresAt) || !got.ConsumedAt.Equal(now) || got.FamilyID != "fam-1" {
		t.Fatalf("round trip = %+v", got)
	}
	again, first, err := s.OAuthCodeConsume(ctx, "c-1", "fam-2", now.Add(time.Second))
	if err != nil || first || again.FamilyID != "fam-1" {
		t.Fatalf("second consume = %+v first=%v err=%v; want the first family, not first", again, first, err)
	}
	var nf *store.ErrNotFound
	if _, _, err := s.OAuthCodeConsume(ctx, "never", "fam-3", now); !errors.As(err, &nf) {
		t.Fatalf("unknown code: %v", err)
	}
}

func testOAuthCodeConcurrentConsumeHasOneWinner(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.OAuthCodeCreate(ctx, oauthCode("c-race", pwgT0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, first, err := s.OAuthCodeConsume(ctx, "c-race", "fam", pwgT0.Add(time.Second)); err == nil && first {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d exchanges consumed one code", wins.Load())
	}
}

func testOAuthCodeInvalidateClientTouchesOnlyItsUnusedCodes(t *testing.T, s store.Store) {
	ctx := context.Background()
	other := oauthCode("c-other", pwgT0.Add(time.Minute))
	other.ClientID = "other"
	for _, r := range []store.OAuthCodeRow{oauthCode("c-a", pwgT0.Add(time.Minute)), oauthCode("c-b", pwgT0.Add(time.Minute)), other} {
		if err := s.OAuthCodeCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.OAuthCodeConsume(ctx, "c-a", "fam-a", pwgT0); err != nil {
		t.Fatal(err)
	}
	n, err := s.OAuthCodeInvalidateClient(ctx, "board", pwgT0.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("invalidated %d, %v; want the one unused code", n, err)
	}
	if _, first, err := s.OAuthCodeConsume(ctx, "c-b", "fam-b", pwgT0.Add(2*time.Second)); err != nil || first {
		t.Fatalf("invalidated code consumed: first=%v %v", first, err)
	}
	if _, first, err := s.OAuthCodeConsume(ctx, "c-other", "fam-o", pwgT0.Add(2*time.Second)); err != nil || !first {
		t.Fatalf("another client's code: first=%v %v", first, err)
	}
}

func testOAuthFamilyRoundTripsTouchesAndRevokesOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	f := oauthFamily("fam-1", "board")
	f.RestrictionsJSON = `{"retention":"none"}`
	if err := s.OAuthFamilyCreate(ctx, f); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	if err := s.OAuthFamilyCreate(ctx, f); !errors.As(err, &conflict) {
		t.Fatalf("duplicate family: %v", err)
	}
	got, err := s.OAuthFamilyGet(ctx, "fam-1")
	if err != nil || got.ClientID != "board" || got.TenantID != "acme" || got.Subject != "alice" || got.Gate != "pw" ||
		len(got.AMR) != 1 || len(got.ScopeCap) != 2 || got.RestrictionsJSON != f.RestrictionsJSON ||
		!got.LastUsedAt.Equal(pwgT0) || !got.IdleExpiresAt.Equal(f.IdleExpiresAt) ||
		!got.AbsoluteExpiresAt.Equal(f.AbsoluteExpiresAt) || !got.RevokedAt.IsZero() {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	touch := pwgT0.Add(time.Hour)
	if err := s.OAuthFamilyTouch(ctx, "fam-1", touch, touch.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.OAuthFamilyGet(ctx, "fam-1"); !got.LastUsedAt.Equal(touch) || !got.IdleExpiresAt.Equal(touch.Add(7*24*time.Hour)) {
		t.Fatalf("touch = %+v", got)
	}
	if ok, err := s.OAuthFamilyRevoke(ctx, "fam-1", "logout", touch); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ok, err := s.OAuthFamilyRevoke(ctx, "fam-1", "again", touch.Add(time.Second)); err != nil || ok {
		t.Fatalf("second revoke reported: %v %v", ok, err)
	}
	got, _ = s.OAuthFamilyGet(ctx, "fam-1")
	if !got.RevokedAt.Equal(touch) || got.RevokeReason != "logout" {
		t.Fatalf("revoked = %+v", got)
	}
	// A revoked family is not touched back to life.
	if err := s.OAuthFamilyTouch(ctx, "fam-1", touch.Add(time.Hour), touch.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.OAuthFamilyGet(ctx, "fam-1"); !got.LastUsedAt.Equal(touch) {
		t.Fatalf("revoked family touched: %+v", got)
	}
}

// A family revoked before it exists (a code replayed while its first exchange
// still runs) leaves a tombstone, and the late create fails.
func testOAuthFamilyRevokedBeforeCreateCannotBeCreated(t *testing.T, s store.Store) {
	ctx := context.Background()
	if ok, err := s.OAuthFamilyRevoke(ctx, "fam-early", "code_reuse", pwgT0); err != nil || !ok {
		t.Fatalf("tombstone revoke: %v %v", ok, err)
	}
	var conflict *store.ErrConflict
	if err := s.OAuthFamilyCreate(ctx, oauthFamily("fam-early", "board")); !errors.As(err, &conflict) {
		t.Fatalf("create over a tombstone: %v", err)
	}
	got, err := s.OAuthFamilyGet(ctx, "fam-early")
	if err != nil || got.RevokedAt.IsZero() || got.RevokeReason != "code_reuse" {
		t.Fatalf("tombstone = %+v, %v", got, err)
	}
}

func testOAuthFamilyRevokeClientRevokesOnlyItsLiveFamilies(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, f := range []store.OAuthFamilyRow{oauthFamily("f-a", "board"), oauthFamily("f-b", "board"), oauthFamily("f-o", "other")} {
		if err := s.OAuthFamilyCreate(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.OAuthFamilyRevoke(ctx, "f-b", "logout", pwgT0); err != nil {
		t.Fatal(err)
	}
	ids, err := s.OAuthFamilyRevokeClient(ctx, "board", "client_retired", pwgT0.Add(time.Minute))
	if err != nil || len(ids) != 1 || ids[0] != "f-a" {
		t.Fatalf("revoked %v, %v; want only the live f-a", ids, err)
	}
	if got, _ := s.OAuthFamilyGet(ctx, "f-o"); !got.RevokedAt.IsZero() {
		t.Fatal("another client's family was revoked")
	}
	if got, _ := s.OAuthFamilyGet(ctx, "f-b"); got.RevokeReason != "logout" {
		t.Fatalf("an already revoked family was re-revoked: %+v", got)
	}
}

func testOAuthRefreshUseIsOnceAndReportsReuse(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: "r-1", FamilyID: "fam-1", CreatedAt: pwgT0}); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	if err := s.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: "r-1", FamilyID: "fam-1"}); !errors.As(err, &conflict) {
		t.Fatalf("duplicate refresh: %v", err)
	}
	now := pwgT0.Add(time.Hour)
	got, first, err := s.OAuthRefreshUse(ctx, "r-1", now)
	if err != nil || !first || got.FamilyID != "fam-1" || !got.UsedAt.Equal(now) || !got.CreatedAt.Equal(pwgT0) {
		t.Fatalf("first use = %+v first=%v %v", got, first, err)
	}
	if got, first, err := s.OAuthRefreshUse(ctx, "r-1", now.Add(time.Minute)); err != nil || first || got.FamilyID != "fam-1" {
		t.Fatalf("reuse = %+v first=%v %v; want the row, not first", got, first, err)
	}
	var nf *store.ErrNotFound
	if _, _, err := s.OAuthRefreshUse(ctx, "never", now); !errors.As(err, &nf) {
		t.Fatalf("unknown refresh: %v", err)
	}
	// A read leaves the token as it was.
	if err := s.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: "r-2", FamilyID: "fam-1", CreatedAt: pwgT0}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OAuthRefreshGet(ctx, "r-2"); err != nil || got.FamilyID != "fam-1" || !got.UsedAt.IsZero() {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, first, err := s.OAuthRefreshUse(ctx, "r-2", now); err != nil || !first {
		t.Fatalf("use after get: first=%v %v", first, err)
	}
	if _, err := s.OAuthRefreshGet(ctx, "never"); !errors.As(err, &nf) {
		t.Fatalf("unknown refresh get: %v", err)
	}
}

func testOAuthTokenRetireRetiresOnlyThatSignIn(t *testing.T, s store.Store) {
	ctx := context.Background()
	mint := func(id, client, fam string) {
		t.Helper()
		if _, err := s.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: id, Name: "u-app-" + client, TenantID: "acme",
			Subject: "alice", TokenHash: "h-" + id, AllowedScopes: []string{"runs:read"}, CreatedAt: pwgT0,
			ExpiresAt: pwgT0.Add(time.Hour), IssuedByGate: "pw", OAuthClient: client, OAuthFamily: fam}); err != nil {
			t.Fatal(err)
		}
	}
	mint("ot-a1", "board", "fam-a")
	mint("ot-a2", "board", "fam-a")
	mint("ot-b", "board", "fam-b")
	mint("ot-o", "other", "fam-o")
	if _, err := s.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: "ot-plain", Name: "plain", TenantID: "acme",
		Subject: "alice", TokenHash: "h-plain", AllowedScopes: []string{"runs:read"}, CreatedAt: pwgT0}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OperatorTokenDefGet(ctx, "ot-a1"); err != nil || got.OAuthClient != "board" || got.OAuthFamily != "fam-a" {
		t.Fatalf("oauth tags round trip = %+v, %v", got, err)
	}
	at := pwgT0.Add(time.Minute)
	if n, err := s.OAuthTokenRetire(ctx, "board", "fam-a", at); err != nil || n != 2 {
		t.Fatalf("family retire = %d, %v", n, err)
	}
	if got, _ := s.OperatorTokenDefGet(ctx, "ot-b"); !got.RetiredAt.IsZero() {
		t.Fatal("another family's token was retired")
	}
	if n, err := s.OAuthTokenRetire(ctx, "board", "", at); err != nil || n != 1 {
		t.Fatalf("client retire = %d, %v; want the one live token left", n, err)
	}
	for _, id := range []string{"ot-o", "ot-plain"} {
		if got, _ := s.OperatorTokenDefGet(ctx, id); !got.RetiredAt.IsZero() {
			t.Fatalf("%s retired by another client's retire", id)
		}
	}
}

func testOAuthDeleteExpiredKeepsWhatCanStillAuthenticate(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, c := range []store.OAuthCodeRow{oauthCode("c-old", pwgT0.Add(time.Minute)), oauthCode("c-live", pwgT0.Add(48*time.Hour))} {
		if err := s.OAuthCodeCreate(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	old := oauthFamily("f-old", "board")
	old.AbsoluteExpiresAt = pwgT0.Add(time.Hour)
	for _, f := range []store.OAuthFamilyRow{old, oauthFamily("f-rev", "board"), oauthFamily("f-live", "board")} {
		if err := s.OAuthFamilyCreate(ctx, f); err != nil {
			t.Fatal(err)
		}
		if err := s.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: "r-" + f.FamilyID, FamilyID: f.FamilyID, CreatedAt: pwgT0}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.OAuthFamilyRevoke(ctx, "f-rev", "logout", pwgT0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OAuthDeleteExpired(ctx, pwgT0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var nf *store.ErrNotFound
	for _, id := range []string{"f-old", "f-rev"} {
		if _, err := s.OAuthFamilyGet(ctx, id); !errors.As(err, &nf) {
			t.Fatalf("%s survived the sweep: %v", id, err)
		}
		if _, _, err := s.OAuthRefreshUse(ctx, "r-"+id, pwgT0); !errors.As(err, &nf) {
			t.Fatalf("%s's refresh token survived the sweep: %v", id, err)
		}
	}
	if _, err := s.OAuthFamilyGet(ctx, "f-live"); err != nil {
		t.Fatalf("live family swept: %v", err)
	}
	if _, _, err := s.OAuthCodeConsume(ctx, "c-old", "x", pwgT0); !errors.As(err, &nf) {
		t.Fatalf("expired code survived: %v", err)
	}
	if _, _, err := s.OAuthCodeConsume(ctx, "c-live", "x", pwgT0); err != nil {
		t.Fatalf("live code swept: %v", err)
	}
}

func testOAuthSubjectDeleteRemovesItsSignIns(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.OAuthCodeCreate(ctx, oauthCode("c-del", pwgT0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	keep := oauthFamily("f-bob", "board")
	keep.Subject = "bob"
	for _, f := range []store.OAuthFamilyRow{oauthFamily("f-del", "board"), keep} {
		if err := s.OAuthFamilyCreate(ctx, f); err != nil {
			t.Fatal(err)
		}
		if err := s.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: "r-" + f.FamilyID, FamilyID: f.FamilyID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AuthSubjectDelete(ctx, "acme", "alice"); err != nil {
		t.Fatal(err)
	}
	var nf *store.ErrNotFound
	if _, err := s.OAuthFamilyGet(ctx, "f-del"); !errors.As(err, &nf) {
		t.Fatalf("deleted subject's family survived: %v", err)
	}
	if _, _, err := s.OAuthRefreshUse(ctx, "r-f-del", pwgT0); !errors.As(err, &nf) {
		t.Fatalf("deleted subject's refresh token survived: %v", err)
	}
	if _, _, err := s.OAuthCodeConsume(ctx, "c-del", "x", pwgT0); !errors.As(err, &nf) {
		t.Fatalf("deleted subject's code survived: %v", err)
	}
	if _, err := s.OAuthFamilyGet(ctx, "f-bob"); err != nil {
		t.Fatalf("another subject's family went: %v", err)
	}
}

// OAuthFamilyRevokeSubject ends that subject's live families only, and
// reports them.
func testOAuthFamilyRevokeSubjectRevokesOnlyItsLiveFamilies(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, tenant, subject string) {
		f := oauthFamily(id, "board")
		f.TenantID, f.Subject = tenant, subject
		if err := s.OAuthFamilyCreate(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	mk("f-a1", "acme", "alice")
	mk("f-a2", "acme", "alice")
	mk("f-done", "acme", "alice")
	mk("f-bob", "acme", "bob")
	mk("f-other", "other", "alice")
	if _, err := s.OAuthFamilyRevoke(ctx, "f-done", "earlier", pwgT0); err != nil {
		t.Fatal(err)
	}
	ids, err := s.OAuthFamilyRevokeSubject(ctx, "acme", "alice", "user_disabled", pwgT0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"f-a1", "f-a2"}) {
		t.Fatalf("revoked %v, want the subject's live families", ids)
	}
	for id, want := range map[string]string{"f-a1": "user_disabled", "f-a2": "user_disabled", "f-done": "earlier", "f-bob": "", "f-other": ""} {
		f, err := s.OAuthFamilyGet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if f.RevokeReason != want || f.RevokedAt.IsZero() != (want == "") {
			t.Errorf("%s = revoked %v reason %q, want reason %q", id, f.RevokedAt, f.RevokeReason, want)
		}
	}
}

// OAuthFamilyList returns the subject's live families only — not another
// subject's or tenant's, not a revoked or expired one — most recently used
// first.
func testOAuthFamilyListReturnsOnlyTheSubjectsLiveSignIns(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0.Add(time.Hour)
	mk := func(id, tenant, subject string, lastUsed time.Time, edit func(*store.OAuthFamilyRow)) {
		t.Helper()
		f := oauthFamily(id, "board")
		f.TenantID, f.Subject, f.LastUsedAt = tenant, subject, lastUsed
		if edit != nil {
			edit(&f)
		}
		if err := s.OAuthFamilyCreate(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	mk("f-old", "acme", "alice", pwgT0, nil)
	mk("f-new", "acme", "alice", pwgT0.Add(30*time.Minute), nil)
	mk("f-revoked", "acme", "alice", pwgT0, nil)
	mk("f-idle", "acme", "alice", pwgT0, func(f *store.OAuthFamilyRow) { f.IdleExpiresAt = now.Add(-time.Second) })
	mk("f-absolute", "acme", "alice", pwgT0, func(f *store.OAuthFamilyRow) { f.AbsoluteExpiresAt = now })
	mk("f-bob", "acme", "bob", pwgT0, nil)
	mk("f-other", "other", "alice", pwgT0, nil)
	if _, err := s.OAuthFamilyRevoke(ctx, "f-revoked", "user_signed_out", pwgT0); err != nil {
		t.Fatal(err)
	}
	got, err := s.OAuthFamilyList(ctx, "acme", "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range got {
		ids = append(ids, f.FamilyID)
	}
	if !reflect.DeepEqual(ids, []string{"f-new", "f-old"}) {
		t.Fatalf("listed %v, want the subject's live families, most recently used first", ids)
	}
	f := got[0]
	if f.ClientID != "board" || f.Gate != "pw" || !f.CreatedAt.Equal(pwgT0) || !f.LastUsedAt.Equal(pwgT0.Add(30*time.Minute)) ||
		!reflect.DeepEqual(f.AMR, []string{"pwd"}) || !f.AbsoluteExpiresAt.Equal(pwgT0.Add(30*24*time.Hour)) {
		t.Fatalf("listed row = %+v", f)
	}
	if none, err := s.OAuthFamilyList(ctx, "acme", "nobody", now); err != nil || len(none) != 0 {
		t.Fatalf("a subject with no sign-ins listed %v, %v", none, err)
	}
}

// OAuthTokenRetireEndedSignIns retires the subject's live app tokens whose
// sign-in is revoked (whatever the reason) or gone, and nothing else. It is
// what lets a disable retried after a partial failure finish: the families
// are already revoked, so the revoke returns none, and the tokens still go.
func testOAuthTokenRetireEndedSignInsFinishesAPartialDisable(t *testing.T, s store.Store) {
	ctx := context.Background()
	fam := func(id, subject string) {
		t.Helper()
		f := oauthFamily(id, "board")
		f.Subject = subject
		if err := s.OAuthFamilyCreate(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	mint := func(id, subject, client, family string) {
		t.Helper()
		if _, err := s.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: id, Name: "app-" + id, TenantID: "acme",
			Subject: subject, TokenHash: "h-" + id, AllowedScopes: []string{"runs:read"}, CreatedAt: pwgT0,
			ExpiresAt: pwgT0.Add(time.Hour), IssuedByGate: "pw", OAuthClient: client, OAuthFamily: family}); err != nil {
			t.Fatal(err)
		}
	}
	fam("f-live", "alice")
	fam("f-done", "alice")
	fam("f-bob", "bob")
	mint("t-live", "alice", "board", "f-live")
	mint("t-done", "alice", "board", "f-done")
	mint("t-gone", "alice", "board", "f-missing")
	mint("t-bob", "bob", "board", "f-bob")
	mint("t-plain", "alice", "", "")
	// The partial failure: the first disable revoked the families and
	// stopped before retiring their tokens.
	if _, err := s.OAuthFamilyRevokeSubject(ctx, "acme", "alice", "user_disabled", pwgT0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OAuthFamilyRevoke(ctx, "f-bob", "client_retired", pwgT0); err != nil {
		t.Fatal(err)
	}
	// The retry: nothing is left to revoke, and the tokens still go.
	if ids, err := s.OAuthFamilyRevokeSubject(ctx, "acme", "alice", "user_disabled", pwgT0.Add(time.Minute)); err != nil || len(ids) != 0 {
		t.Fatalf("second revoke = %v, %v; want none", ids, err)
	}
	at := pwgT0.Add(time.Minute)
	if n, err := s.OAuthTokenRetireEndedSignIns(ctx, "acme", "alice", at); err != nil || n != 3 {
		t.Fatalf("retire ended sign-ins = %d, %v; want alice's three app tokens", n, err)
	}
	for id, retired := range map[string]bool{"t-live": true, "t-done": true, "t-gone": true, "t-bob": false, "t-plain": false} {
		got, err := s.OperatorTokenDefGet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.RetiredAt.IsZero() == retired {
			t.Errorf("%s retired_at = %v, want retired %v", id, got.RetiredAt, retired)
		}
	}
	if n, err := s.OAuthTokenRetireEndedSignIns(ctx, "acme", "alice", at.Add(time.Minute)); err != nil || n != 0 {
		t.Fatalf("a repeat retired %d, %v; want 0", n, err)
	}
	// A live sign-in's token is left.
	fam("f-new", "alice")
	mint("t-new", "alice", "board", "f-new")
	if n, err := s.OAuthTokenRetireEndedSignIns(ctx, "acme", "alice", at.Add(2*time.Minute)); err != nil || n != 0 {
		t.Fatalf("retire with a live sign-in = %d, %v; want 0", n, err)
	}
}

// A reuse marks its family once: the first reuse of a live family revokes
// it, the first reuse of a family ended for another reason keeps that reason
// and adds the reuse, an unknown family gets a tombstone — each reports
// "first" — and every later reuse of the same family reports a repeat.
func testOAuthFamilyRevokeForReuseReportsTheFirstReuseOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"fam-live", "fam-ended"} {
		if err := s.OAuthFamilyCreate(ctx, oauthFamily(id, "board")); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.OAuthFamilyRevoke(ctx, "fam-ended", "revoked_by_app", pwgT0); err != nil || !ok {
		t.Fatalf("app revoke = %v, %v", ok, err)
	}
	for _, tc := range []struct{ id, reason, wantReason string }{
		{"fam-live", "refresh_reuse", "refresh_reuse"},
		{"fam-ended", "refresh_reuse", "revoked_by_app;refresh_reuse"},
		{"fam-unknown", "code_reuse", "code_reuse"},
	} {
		first, err := s.OAuthFamilyRevokeForReuse(ctx, tc.id, tc.reason, pwgT0)
		if err != nil || !first {
			t.Fatalf("%s first reuse = %v, %v; want first", tc.id, first, err)
		}
		for range 2 {
			if again, err := s.OAuthFamilyRevokeForReuse(ctx, tc.id, tc.reason, pwgT0); err != nil || again {
				t.Fatalf("%s repeat = %v, %v; want a repeat", tc.id, again, err)
			}
		}
		got, err := s.OAuthFamilyGet(ctx, tc.id)
		if err != nil || got.RevokedAt.IsZero() || got.RevokeReason != tc.wantReason {
			t.Errorf("%s = revoked %v reason %q, %v; want reason %q", tc.id, got.RevokedAt, got.RevokeReason, err, tc.wantReason)
		}
	}
}
