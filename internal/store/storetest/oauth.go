package storetest

import (
	"context"
	"errors"
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
