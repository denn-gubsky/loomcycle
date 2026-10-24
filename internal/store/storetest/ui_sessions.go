package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA §5 Web UI session contract, asserted against both backends.

func mkUISession(hash string, now time.Time) store.UISessionRow {
	return store.UISessionRow{
		SessionHash: hash, TenantID: "acme", Subject: "alice", Gate: "token", AMR: []string{"bearer"},
		CredKind: "token", CredRef: "def-1", CredMAC: "mac", UAHash: "ua", IPHash: "ip",
		CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour),
	}
}

func isUISessionNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

// sameInstant compares times at the coarsest precision either backend keeps
// (postgres stores microseconds).
func sameInstant(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

func testUISessionCreateGetRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	want := mkUISession("h1", now)
	if err := s.UISessionCreate(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.UISessionGet(ctx, "h1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TenantID != "acme" || got.Subject != "alice" || got.Gate != "token" || !reflect.DeepEqual(got.AMR, []string{"bearer"}) ||
		got.CredKind != "token" || got.CredRef != "def-1" || got.CredMAC != "mac" || got.UAHash != "ua" || got.IPHash != "ip" {
		t.Errorf("round trip = %+v", got)
	}
	if !sameInstant(got.CreatedAt, now) || !sameInstant(got.IdleExpiresAt, want.IdleExpiresAt) ||
		!sameInstant(got.AbsoluteExpiresAt, want.AbsoluteExpiresAt) || !got.RevokedAt.IsZero() {
		t.Errorf("times = %+v", got)
	}
	if _, err := s.UISessionGet(ctx, "missing"); !isUISessionNotFound(err) {
		t.Errorf("missing get err = %v, want ErrNotFound", err)
	}
	if err := s.UISessionCreate(ctx, store.UISessionRow{SessionHash: "h2"}); err == nil {
		t.Error("a row without tenant/subject/gate/credential/expiry was accepted")
	}
}

func testUISessionRevokeIsOnceAndTouchSkipsRevoked(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	if err := s.UISessionCreate(ctx, mkUISession("h1", now)); err != nil {
		t.Fatalf("create: %v", err)
	}
	later := now.Add(time.Minute)
	if err := s.UISessionTouch(ctx, "h1", later, later.Add(time.Hour)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := s.UISessionGet(ctx, "h1")
	if !sameInstant(got.LastSeenAt, later) || !sameInstant(got.IdleExpiresAt, later.Add(time.Hour)) {
		t.Errorf("touch did not move last_seen/idle: %+v", got)
	}
	changed, err := s.UISessionRevoke(ctx, "h1", later)
	if err != nil || !changed {
		t.Fatalf("first revoke = %v, %v; want true", changed, err)
	}
	if changed, err := s.UISessionRevoke(ctx, "h1", later.Add(time.Minute)); err != nil || changed {
		t.Errorf("second revoke = %v, %v; want false (already revoked)", changed, err)
	}
	if changed, err := s.UISessionRevoke(ctx, "missing", later); err != nil || changed {
		t.Errorf("revoke of a missing row = %v, %v; want false, nil", changed, err)
	}
	got, _ = s.UISessionGet(ctx, "h1")
	if !sameInstant(got.RevokedAt, later) {
		t.Errorf("revoked_at = %v, want %v (the first revoke's time)", got.RevokedAt, later)
	}
	if err := s.UISessionTouch(ctx, "h1", later.Add(time.Hour), later.Add(5*time.Hour)); err != nil {
		t.Fatalf("touch on revoked: %v", err)
	}
	if again, _ := s.UISessionGet(ctx, "h1"); !sameInstant(again.LastSeenAt, later) {
		t.Errorf("a revoked session was touched: last_seen = %v", again.LastSeenAt)
	}
}

func testUISessionDeleteExpiredKeepsOnlyLiveRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	live := mkUISession("live", now)
	idle := mkUISession("idle", now)
	idle.IdleExpiresAt = now.Add(-time.Second)
	abs := mkUISession("absolute", now)
	abs.AbsoluteExpiresAt = now.Add(-time.Second)
	revoked := mkUISession("revoked", now)
	for _, r := range []store.UISessionRow{live, idle, abs, revoked} {
		if err := s.UISessionCreate(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.SessionHash, err)
		}
	}
	if _, err := s.UISessionRevoke(ctx, "revoked", now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	n, err := s.UISessionDeleteExpired(ctx, now)
	if err != nil || n != 3 {
		t.Fatalf("delete expired = %d, %v; want 3", n, err)
	}
	if _, err := s.UISessionGet(ctx, "live"); err != nil {
		t.Errorf("the live session went: %v", err)
	}
	for _, h := range []string{"idle", "absolute", "revoked"} {
		if _, err := s.UISessionGet(ctx, h); !isUISessionNotFound(err) {
			t.Errorf("%s session kept: err = %v", h, err)
		}
	}
}
