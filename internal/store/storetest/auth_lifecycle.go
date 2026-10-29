package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P4: the external gates' replay cache and the removal stage machine,
// asserted against both backends.

// A jti is used once per gate; another gate's equal jti is its own; a row
// becomes reusable only after its expiry; and of many concurrent uses exactly
// one wins (the cross-replica property the cache exists for).
func testAuthAssertionUseOnceIsOncePerGateAndAtomic(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	exp := now.Add(5 * time.Minute)
	if ok, err := s.AuthAssertionUseOnce(ctx, "tg", "j1", exp, now); err != nil || !ok {
		t.Fatalf("first use = %v, %v; want true", ok, err)
	}
	if ok, err := s.AuthAssertionUseOnce(ctx, "tg", "j1", exp, now); err != nil || ok {
		t.Fatalf("second use = %v, %v; want false", ok, err)
	}
	if ok, err := s.AuthAssertionUseOnce(ctx, "other", "j1", exp, now); err != nil || !ok {
		t.Fatalf("same jti at another gate = %v, %v; want true", ok, err)
	}
	if ok, err := s.AuthAssertionUseOnce(ctx, "tg", "j1", exp.Add(time.Hour), exp.Add(time.Second)); err != nil || !ok {
		t.Fatalf("reuse after expiry = %v, %v; want true", ok, err)
	}
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.AuthAssertionUseOnce(ctx, "tg", "race", exp, now)
			if err != nil {
				t.Errorf("concurrent use: %v", err)
				return
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
		t.Fatalf("%d concurrent uses of one jti won, want exactly 1", wins)
	}
	n, err := s.AuthAssertionDeleteExpired(ctx, exp.Add(time.Minute))
	if err != nil || n < 2 {
		t.Fatalf("sweep = %d, %v; want the expired rows gone", n, err)
	}
	if ok, err := s.AuthAssertionUseOnce(ctx, "tg", "j1", exp.Add(time.Hour), now); err != nil || ok {
		t.Fatalf("a live row was swept: %v %v", ok, err)
	}
}

func mkRemoval(subject string, at time.Time) store.AuthRemovalRow {
	return store.AuthRemovalRow{TenantID: "acme", Subject: subject, Gate: "tg", Reason: store.RemovalReasonRevoked,
		DisabledAt: at, UnbindDueAt: at.Add(7 * 24 * time.Hour), CreatedBy: "gate:tg"}
}

// A removal starts once (a repeat never postpones it), comes due in stage
// order, and the erase can never be marked before the unbind.
func testAuthRemovalStagesNeverSkip(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, sub := range []string{"alice", "bob", "carol"} {
		if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-30 * 24 * time.Hour)
	r := mkRemoval("alice", t0)
	r.EraseDueAt = t0.Add(30 * 24 * time.Hour)
	if ok, err := s.AuthRemovalStart(ctx, r); err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	later := mkRemoval("alice", time.Now())
	if ok, err := s.AuthRemovalStart(ctx, later); err != nil || ok {
		t.Fatalf("a second start = %v, %v; want the first kept", ok, err)
	}
	got, err := s.AuthRemovalGet(ctx, "acme", "alice")
	if err != nil || !sameInstant(got.DisabledAt, t0) || !sameInstant(got.EraseDueAt, r.EraseDueAt) || got.Gate != "tg" || got.Reason != "revoked" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if ok, err := s.AuthRemovalMarkErased(ctx, "acme", "alice", time.Now()); err != nil || ok {
		t.Fatalf("erase before unbind = %v, %v; want refused", ok, err)
	}
	// Only the unbind is due, although the erase time has passed too.
	due, err := s.AuthRemovalDue(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 || !due[0].UnboundAt.IsZero() {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if ok, err := s.AuthRemovalMarkUnbound(ctx, "acme", "alice", time.Now()); err != nil || !ok {
		t.Fatalf("unbind = %v, %v", ok, err)
	}
	if ok, err := s.AuthRemovalMarkUnbound(ctx, "acme", "alice", time.Now()); err != nil || ok {
		t.Fatalf("a second unbind = %v, %v; want false", ok, err)
	}
	due, _ = s.AuthRemovalDue(ctx, time.Now(), 10)
	if len(due) != 1 || due[0].UnboundAt.IsZero() {
		t.Fatalf("after unbind, due = %+v; want the erase", due)
	}
	if ok, err := s.AuthRemovalMarkErased(ctx, "acme", "alice", time.Now()); err != nil || !ok {
		t.Fatalf("erase = %v, %v", ok, err)
	}
	if due, _ = s.AuthRemovalDue(ctx, time.Now(), 10); len(due) != 0 {
		t.Fatalf("an erased removal is still due: %+v", due)
	}
	// Erase never: once unbound, nothing more is ever due.
	never := mkRemoval("bob", t0)
	if _, err := s.AuthRemovalStart(ctx, never); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthRemovalMarkUnbound(ctx, "acme", "bob", time.Now()); err != nil {
		t.Fatal(err)
	}
	if due, _ = s.AuthRemovalDue(ctx, time.Now().Add(100*365*24*time.Hour), 10); len(due) != 0 {
		t.Fatalf("an erase_after: never removal came due: %+v", due)
	}
	// Out-of-order due times are refused.
	bad := mkRemoval("carol", t0)
	bad.EraseDueAt = t0.Add(time.Hour)
	if _, err := s.AuthRemovalStart(ctx, bad); err == nil {
		t.Fatal("an erase due before the unbind was accepted")
	}
	if list, _ := s.AuthRemovalList(ctx, "acme"); len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list, _ := s.AuthRemovalList(ctx, ""); len(list) != 0 {
		t.Fatalf("the shared tenant's list read another tenant's rows: %+v", list)
	}
	if all, _ := s.AuthRemovalListAllTenants(ctx); len(all) != 2 {
		t.Fatalf("all = %+v", all)
	}
	if ok, err := s.AuthRemovalDelete(ctx, "acme", "bob"); err != nil || !ok {
		t.Fatalf("delete = %v %v", ok, err)
	}
	if _, err := s.AuthRemovalGet(ctx, "acme", "bob"); !isUISessionNotFound(err) {
		t.Fatalf("get after delete: %v", err)
	}
}

// Dormancy looks at the newest login of any of a user's identities, skips
// users with no identity, disabled users and users already being removed.
func testAuthDormantCandidatesUseTheNewestLoginOfAnyGate(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := time.Now().Add(-200 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	for _, u := range []struct{ sub, status string }{
		{"stale", "active"}, {"fresh-elsewhere", "active"}, {"no-identity", "active"}, {"off", "disabled"}, {"removing", "active"},
	} {
		if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: u.sub, AccessMode: "isolated", Status: u.status}); err != nil {
			t.Fatal(err)
		}
	}
	ident := func(sub, gate string, login time.Time) {
		t.Helper()
		if err := s.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: "acme", Subject: sub, Gate: gate, ExternalSubject: sub + "@" + gate, CreatedAt: old}); err != nil {
			t.Fatal(err)
		}
		if !login.IsZero() {
			if err := s.UserIdentityTouchLogin(ctx, gate, "", sub+"@"+gate, login); err != nil {
				t.Fatal(err)
			}
		}
	}
	ident("stale", "tg", old)
	ident("fresh-elsewhere", "tg", old)
	ident("fresh-elsewhere", "local", recent)
	ident("off", "tg", time.Time{})
	ident("removing", "tg", time.Time{})
	if _, err := s.AuthRemovalStart(ctx, mkRemoval("removing", time.Now())); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuthDormantCandidates(ctx, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Subject != "stale" || got[0].TenantID != "acme" {
		t.Fatalf("dormant = %+v, want only stale", got)
	}
	ids, err := s.UserIdentityListGate(ctx, "tg")
	if err != nil || len(ids) != 4 {
		t.Fatalf("identities at tg = %d %v, want 4", len(ids), err)
	}
	if err := s.AuthSubjectDelete(ctx, "acme", "removing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthRemovalGet(ctx, "acme", "removing"); !isUISessionNotFound(err) {
		t.Fatalf("a deleted user's removal survived: %v", err)
	}
}

// UISessionRevokeSubject ends exactly that subject's live sessions.
func testUISessionRevokeSubjectEndsOnlyThatSubjectsSessions(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	a1, a2, b := mkUISession("a1", now), mkUISession("a2", now), mkUISession("b", now)
	b.Subject = "bob"
	other := mkUISession("o", now)
	other.TenantID = "other"
	for _, r := range []store.UISessionRow{a1, a2, b, other} {
		if err := s.UISessionCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UISessionRevoke(ctx, "a2", now); err != nil {
		t.Fatal(err)
	}
	n, err := s.UISessionRevokeSubject(ctx, "acme", "alice", now)
	if err != nil || n != 1 {
		t.Fatalf("revoke subject = %d, %v; want the one live session", n, err)
	}
	for h, want := range map[string]bool{"a1": true, "a2": true, "b": false, "o": false} {
		r, err := s.UISessionGet(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if got := !r.RevokedAt.IsZero(); got != want {
			t.Errorf("session %s revoked = %v, want %v", h, got, want)
		}
	}
}
