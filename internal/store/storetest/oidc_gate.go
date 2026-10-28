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

// RFC FA P1 contract: started redirect logins, identity unlink, the
// insert-only group create, a session's auth_time and a token's issuing gate.

func loginState(hash string, expires time.Time) store.AuthLoginStateRow {
	return store.AuthLoginStateRow{StateHash: hash, Gate: "google", BindingHash: "bind-" + hash, Purpose: store.LoginPurposeLink,
		ReturnTo: "/ui/account", Reauth: true, LinkTenant: "acme", LinkSubject: "alice", LinkSessionHash: "sess-1",
		CreatedAt: pwgT0, ExpiresAt: expires}
}

func testAuthLoginStateConsumesOnceAndRoundTrips(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := loginState("st-1", pwgT0.Add(10*time.Minute))
	if err := s.AuthLoginStateCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	if err := s.AuthLoginStateCreate(ctx, row); !errors.As(err, &conflict) {
		t.Fatalf("duplicate state: %v", err)
	}
	now := pwgT0.Add(time.Minute)
	got, err := s.AuthLoginStateConsume(ctx, "st-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gate != "google" || got.BindingHash != "bind-st-1" || got.Purpose != store.LoginPurposeLink || !got.Reauth ||
		got.ReturnTo != "/ui/account" || got.LinkTenant != "acme" || got.LinkSubject != "alice" || got.LinkSessionHash != "sess-1" ||
		!got.CreatedAt.Equal(pwgT0) || !got.ExpiresAt.Equal(row.ExpiresAt) || !got.ConsumedAt.Equal(now) {
		t.Fatalf("round trip = %+v", got)
	}
	var nf *store.ErrNotFound
	if _, err := s.AuthLoginStateConsume(ctx, "st-1", now); !errors.As(err, &nf) {
		t.Fatalf("replayed state: %v", err)
	}
	if _, err := s.AuthLoginStateConsume(ctx, "never", now); !errors.As(err, &nf) {
		t.Fatalf("unknown state: %v", err)
	}
}

func testAuthLoginStateExpiredIsRefusedAndSwept(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.AuthLoginStateCreate(ctx, loginState("st-old", pwgT0.Add(10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthLoginStateCreate(ctx, loginState("st-live", pwgT0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	var nf *store.ErrNotFound
	if _, err := s.AuthLoginStateConsume(ctx, "st-old", pwgT0.Add(10*time.Minute)); !errors.As(err, &nf) {
		t.Fatalf("expired state consumed: %v", err)
	}
	n, err := s.AuthLoginStateDeleteExpired(ctx, pwgT0.Add(30*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	if _, err := s.AuthLoginStateConsume(ctx, "st-live", pwgT0.Add(20*time.Minute)); err != nil {
		t.Fatalf("live state after the sweep: %v", err)
	}
}

// Of concurrent callbacks carrying the same state, exactly one finishes.
func testAuthLoginStateConcurrentConsumeHasOneWinner(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.AuthLoginStateCreate(ctx, loginState("st-race", pwgT0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AuthLoginStateConsume(ctx, "st-race", pwgT0.Add(time.Minute)); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d callbacks consumed one state", wins.Load())
	}
}

func testUserIdentityDeleteRemovesOnlyThatGate(t *testing.T, s store.Store) {
	ctx := context.Background()
	a := pwgIdentity("acme", "alice", "alice")
	b := a
	b.Gate, b.Issuer, b.ExternalSubject = "google", "https://accounts.google.com", "1234"
	for _, r := range []store.UserIdentityRow{a, b} {
		if err := s.UserIdentityCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.UserIdentityDelete(ctx, "other", "alice", "google"); err != nil || ok {
		t.Fatalf("cross-tenant delete: %v %v", ok, err)
	}
	if ok, err := s.UserIdentityDelete(ctx, "acme", "alice", "google"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	ids, err := s.UserIdentityListSubject(ctx, "acme", "alice")
	if err != nil || len(ids) != 1 || ids[0].Gate != pwgGate {
		t.Fatalf("left %+v, %v", ids, err)
	}
	if ok, _ := s.UserIdentityDelete(ctx, "acme", "alice", "google"); ok {
		t.Fatal("second delete reported a row")
	}
}

// GroupCreate never touches an existing group: a sync cannot take over a
// local group by naming it.
func testGroupCreateNeverOverwrites(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "admins", Description: "local"}); err != nil {
		t.Fatal(err)
	}
	created, err := s.GroupCreate(ctx, store.GroupRow{TenantID: "acme", Name: "admins", ExternalID: "google:admins"})
	if err != nil || created {
		t.Fatalf("create over a local group: %v %v", created, err)
	}
	g, err := s.GroupGet(ctx, "acme", "admins")
	if err != nil || g.ExternalID != "" || g.Description != "local" {
		t.Fatalf("local group changed: %+v %v", g, err)
	}
	created, err = s.GroupCreate(ctx, store.GroupRow{TenantID: "acme", Name: "staff", ExternalID: "google:staff"})
	if err != nil || !created {
		t.Fatalf("new group: %v %v", created, err)
	}
	if g, err := s.GroupGet(ctx, "acme", "staff"); err != nil || g.ExternalID != "google:staff" {
		t.Fatalf("new group read back: %+v %v", g, err)
	}
}

func testUISessionAuthTimeRoundTrips(t *testing.T, s store.Store) {
	ctx := context.Background()
	base := store.UISessionRow{TenantID: "acme", Subject: "alice", Gate: "google", CredKind: "identity", CredMAC: "mac",
		CreatedAt: pwgT0, LastSeenAt: pwgT0, IdleExpiresAt: pwgT0.Add(time.Hour), AbsoluteExpiresAt: pwgT0.Add(2 * time.Hour)}
	with, without := base, base
	with.SessionHash, with.AuthTime = "s-auth", pwgT0.Add(-time.Minute)
	without.SessionHash = "s-noauth"
	for _, r := range []store.UISessionRow{with, without} {
		if err := s.UISessionCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.UISessionGet(ctx, "s-auth"); err != nil || !got.AuthTime.Equal(with.AuthTime) {
		t.Fatalf("auth_time = %v, %v", got.AuthTime, err)
	}
	if got, err := s.UISessionGet(ctx, "s-noauth"); err != nil || !got.AuthTime.IsZero() {
		t.Fatalf("unknown auth_time = %v, %v", got.AuthTime, err)
	}
}

func testOperatorTokenIssuedByGateRoundTrips(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := store.OperatorTokenDefRow{DefID: "ot-gate", Name: "u-gate-google", TenantID: "acme", Subject: "google:1",
		TokenHash: "h-ot-gate", AllowedScopes: []string{"runs:read"}, CreatedAt: pwgT0, IssuedByGate: "google"}
	if _, err := s.OperatorTokenDefCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	plain := row
	plain.DefID, plain.TokenHash, plain.IssuedByGate = "ot-plain", "h-ot-plain", ""
	if _, err := s.OperatorTokenDefCreate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OperatorTokenDefGet(ctx, "ot-gate"); err != nil || got.IssuedByGate != "google" {
		t.Fatalf("issued_by_gate = %q, %v", got.IssuedByGate, err)
	}
	if got, err := s.OperatorTokenDefGetByTokenHash(ctx, "h-ot-plain"); err != nil || got.IssuedByGate != "" {
		t.Fatalf("plain token issued_by_gate = %q, %v", got.IssuedByGate, err)
	}
}

// RFC EY §8: an inherit token reads back inherit on both lookups, and a row
// written without the field (every token minted before it existed) reads back
// frozen ("").
func testOperatorTokenAuthorityRoundTripsAndDefaultsFrozen(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := store.OperatorTokenDefRow{DefID: "ot-inherit", Name: "u-alice", TenantID: "acme", Subject: "alice",
		TokenHash: "h-ot-inherit", AllowedScopes: []string{"substrate:user"}, CreatedAt: pwgT0, Authority: "inherit"}
	if _, err := s.OperatorTokenDefCreate(ctx, row); err != nil {
		t.Fatal(err)
	}
	frozen := row
	frozen.DefID, frozen.TokenHash, frozen.Authority = "ot-frozen", "h-ot-frozen", ""
	if _, err := s.OperatorTokenDefCreate(ctx, frozen); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OperatorTokenDefGet(ctx, "ot-inherit"); err != nil || got.Authority != "inherit" {
		t.Fatalf("Get authority = %q, %v; want inherit", got.Authority, err)
	}
	if got, err := s.OperatorTokenDefGetByTokenHash(ctx, "h-ot-inherit"); err != nil || got.Authority != "inherit" {
		t.Fatalf("GetByTokenHash authority = %q, %v; want inherit", got.Authority, err)
	}
	if got, err := s.OperatorTokenDefGetByTokenHash(ctx, "h-ot-frozen"); err != nil || got.Authority != "" {
		t.Fatalf("frozen token authority = %q, %v; want \"\"", got.Authority, err)
	}
	rows, err := s.OperatorTokenDefListBySubject(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, r := range rows {
		seen[r.DefID] = r.Authority
	}
	if seen["ot-inherit"] != "inherit" || seen["ot-frozen"] != "" || len(seen) != 2 {
		t.Fatalf("listing authority = %v", seen)
	}
}

func testUserStatusPendingIsStored(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:9", AccessMode: "isolated", Status: store.UserStatusPending}); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UserGet(ctx, "acme", "google:9"); err != nil || u.Status != store.UserStatusPending {
		t.Fatalf("status = %q, %v", u.Status, err)
	}
	active := "active"
	if err := s.UserUpdate(ctx, "acme", "google:9", store.UserPatch{Status: &active}); err != nil {
		t.Fatal(err)
	}
}
