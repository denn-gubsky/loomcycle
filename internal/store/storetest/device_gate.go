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

// RFC FA P5 device authorization + per-tenant gate policy contract, asserted
// against both backends.

func mkDeviceAuth(dev, user string, now time.Time) store.DeviceAuthRow {
	return store.DeviceAuthRow{
		DeviceCodeHash: dev, UserCodeHash: user, ClientID: "loomcycle-cli",
		SourceAddr: "203.0.113.7", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Interval: 5 * time.Second,
	}
}

func testDeviceAuthCreateGetRoundTripAndConflict(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	if err := s.DeviceAuthCreate(ctx, mkDeviceAuth("d1", "u1", now)); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.DeviceAuthGetByUserCode(ctx, "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DeviceCodeHash != "d1" || got.ClientID != "loomcycle-cli" ||
		got.SourceAddr != "203.0.113.7" || got.Status != store.DeviceAuthPending || got.Interval != 5*time.Second ||
		!got.LastPolledAt.IsZero() || !got.DecidedAt.IsZero() || !got.RedeemedAt.IsZero() ||
		!sameInstant(got.ExpiresAt, now.Add(10*time.Minute)) {
		t.Errorf("round trip = %+v", got)
	}
	var conflict *store.ErrConflict
	if err := s.DeviceAuthCreate(ctx, mkDeviceAuth("d1", "u2", now)); !errors.As(err, &conflict) {
		t.Errorf("duplicate device hash: err = %v, want ErrConflict", err)
	}
	if err := s.DeviceAuthCreate(ctx, mkDeviceAuth("d2", "u1", now)); !errors.As(err, &conflict) {
		t.Errorf("duplicate user hash: err = %v, want ErrConflict", err)
	}
	var nf *store.ErrNotFound
	if _, err := s.DeviceAuthGetByUserCode(ctx, "nope"); !errors.As(err, &nf) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	if err := s.DeviceAuthCreate(ctx, store.DeviceAuthRow{DeviceCodeHash: "d3"}); err == nil {
		t.Error("a row without user code / client / expiry / interval was accepted")
	}
}

func testDeviceAuthPollSlowsDownEarlyPollsOnly(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	if err := s.DeviceAuthCreate(ctx, mkDeviceAuth("d1", "u1", now)); err != nil {
		t.Fatal(err)
	}
	step, max := 5*time.Second, 12*time.Second
	row, early, err := s.DeviceAuthPoll(ctx, "d1", now, step, max)
	if err != nil || early || row.Interval != 5*time.Second || !sameInstant(row.LastPolledAt, now) {
		t.Fatalf("first poll: early=%v interval=%s err=%v", early, row.Interval, err)
	}
	// Two seconds later is early: slow_down, interval +5s.
	row, early, err = s.DeviceAuthPoll(ctx, "d1", now.Add(2*time.Second), step, max)
	if err != nil || !early || row.Interval != 10*time.Second {
		t.Fatalf("early poll: early=%v interval=%s err=%v", early, row.Interval, err)
	}
	// Again early (5s < the new 10s): capped at max.
	row, early, err = s.DeviceAuthPoll(ctx, "d1", now.Add(7*time.Second), step, max)
	if err != nil || !early || row.Interval != 12*time.Second {
		t.Fatalf("second early poll: early=%v interval=%s err=%v", early, row.Interval, err)
	}
	// On time against the latest poll and the current interval: not early,
	// the interval stays where the early polls pushed it.
	row, early, err = s.DeviceAuthPoll(ctx, "d1", now.Add(20*time.Second), step, max)
	if err != nil || early || row.Interval != 12*time.Second {
		t.Fatalf("on-time poll: early=%v interval=%s err=%v", early, row.Interval, err)
	}
	var nf *store.ErrNotFound
	if _, _, err := s.DeviceAuthPoll(ctx, "unknown", now, step, max); !errors.As(err, &nf) {
		t.Errorf("unknown code: err = %v, want ErrNotFound", err)
	}
}

// pinnedApproval is an approval through gate "local" that pins its session.
func pinnedApproval(tenant, subject string, at, notAfter time.Time) store.DeviceAuthDecision {
	return store.DeviceAuthDecision{
		Approve: true, TenantID: tenant, Subject: subject, Gate: "local", At: at,
		Session: "sess-" + subject, CredMAC: "mac-" + subject, NotAfter: notAfter,
	}
}

func testDeviceAuthDecideOnceAndRedeemOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	if err := s.DeviceAuthCreate(ctx, mkDeviceAuth("d1", "u1", now)); err != nil {
		t.Fatal(err)
	}
	// Not approved yet: nothing to redeem.
	if _, ok, err := s.DeviceAuthRedeem(ctx, "d1", now); err != nil || ok {
		t.Fatalf("redeem pending: ok=%v err=%v", ok, err)
	}
	// An approval that does not pin its approving session is refused, and
	// leaves the row pending for a proper one.
	unpinned := store.DeviceAuthDecision{Approve: true, TenantID: "acme", Subject: "alice", Gate: "local", At: now}
	if ok, err := s.DeviceAuthDecide(ctx, "u1", unpinned); err == nil || ok {
		t.Fatalf("unpinned approval: ok=%v err=%v, want refused", ok, err)
	}
	notAfter := now.Add(time.Hour)
	alice := pinnedApproval("acme", "alice", now, notAfter)
	if ok, err := s.DeviceAuthDecide(ctx, "u1", alice); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	// Bob cannot take it over, approve or deny.
	for _, d := range []store.DeviceAuthDecision{
		pinnedApproval("acme", "bob", now, notAfter),
		{Approve: false, TenantID: "acme", Subject: "bob", Gate: "local", At: now},
	} {
		if ok, err := s.DeviceAuthDecide(ctx, "u1", d); err != nil || ok {
			t.Fatalf("second decision %+v: ok=%v err=%v", d, ok, err)
		}
	}
	// Concurrent redemptions: exactly one wins, and it carries alice.
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []store.DeviceAuthRow
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row, ok, err := s.DeviceAuthRedeem(ctx, "d1", now.Add(time.Second))
			if err != nil {
				t.Errorf("redeem: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins = append(wins, row)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("%d redemptions succeeded, want exactly 1", len(wins))
	}
	w := wins[0]
	if w.Status != store.DeviceAuthRedeemed || w.DecidedTenant != "acme" || w.DecidedSubject != "alice" ||
		w.DecidedGate != "local" || w.RedeemedAt.IsZero() ||
		w.DecidedSession != "sess-alice" || w.DecidedCredMAC != "mac-alice" {
		t.Errorf("redeemed row = %+v", w)
	}
	if d := w.DecidedNotAfter.Sub(notAfter); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("redeemed row's not-after = %v, want %v", w.DecidedNotAfter, notAfter)
	}
	got, err := s.DeviceAuthGetByUserCode(ctx, "u1")
	if err != nil || got.Status != store.DeviceAuthRedeemed {
		t.Fatalf("after redeem: %+v err=%v", got, err)
	}
}

func testDeviceAuthExpiredOrDeniedCannotBeDecidedOrRedeemed(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	exp := mkDeviceAuth("d1", "u1", now)
	exp.ExpiresAt = now.Add(time.Minute)
	if err := s.DeviceAuthCreate(ctx, exp); err != nil {
		t.Fatal(err)
	}
	late := now.Add(2 * time.Minute)
	if ok, err := s.DeviceAuthDecide(ctx, "u1", pinnedApproval("acme", "alice", late, late.Add(time.Hour))); err != nil || ok {
		t.Fatalf("approve after expiry: ok=%v err=%v", ok, err)
	}
	// Approved in time, redeemed too late.
	if err := s.DeviceAuthCreate(ctx, func() store.DeviceAuthRow {
		r := mkDeviceAuth("d2", "u2", now)
		r.ExpiresAt = now.Add(time.Minute)
		return r
	}()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.DeviceAuthDecide(ctx, "u2", pinnedApproval("acme", "alice", now, now.Add(time.Hour))); !ok {
		t.Fatal("approve in time failed")
	}
	if _, ok, err := s.DeviceAuthRedeem(ctx, "d2", late); err != nil || ok {
		t.Fatalf("redeem after expiry: ok=%v err=%v", ok, err)
	}
	// Denied: never redeemable.
	if err := s.DeviceAuthCreate(ctx, mkDeviceAuth("d3", "u3", now)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.DeviceAuthDecide(ctx, "u3", store.DeviceAuthDecision{Approve: false, TenantID: "acme", Subject: "alice", Gate: "local", At: now}); !ok {
		t.Fatal("deny failed")
	}
	if got, _ := s.DeviceAuthGetByUserCode(ctx, "u3"); got.Status != store.DeviceAuthDenied || got.DecidedSubject != "alice" {
		t.Fatalf("denied row = %+v", got)
	}
	if _, ok, err := s.DeviceAuthRedeem(ctx, "d3", now); err != nil || ok {
		t.Fatalf("redeem denied: ok=%v err=%v", ok, err)
	}
	// The sweep removes only rows expired before the cut.
	n, err := s.DeviceAuthDeleteExpired(ctx, now.Add(90*time.Second))
	if err != nil || n != 2 {
		t.Fatalf("sweep: n=%d err=%v, want 2", n, err)
	}
	if _, err := s.DeviceAuthGetByUserCode(ctx, "u3"); err != nil {
		t.Errorf("live denied row swept: %v", err)
	}
}

func testTenantAuthPolicyPutGetListDeleteAndRevokeSessions(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	var nf *store.ErrNotFound
	if _, err := s.TenantAuthPolicyGet(ctx, "acme"); !errors.As(err, &nf) {
		t.Fatalf("missing policy: err = %v", err)
	}
	if err := s.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "acme", Gates: []string{"local", "google", "local"}, UpdatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.TenantAuthPolicyGet(ctx, "acme")
	if err != nil || !reflect.DeepEqual(got.Gates, []string{"google", "local"}) || got.UpdatedBy != "admin" || got.UpdatedAt.IsZero() {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	if err := s.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "acme", Gates: []string{"google"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "beta", Gates: []string{"local"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "gamma", Gates: nil}); err == nil {
		t.Error("a policy with no gate was accepted (it would lock the tenant out)")
	}
	if err := s.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "", Gates: []string{"local"}}); err == nil {
		t.Error("a policy for the shared tenant was accepted")
	}
	list, err := s.TenantAuthPolicyList(ctx)
	if err != nil || len(list) != 2 || list[0].TenantID != "acme" || !reflect.DeepEqual(list[0].Gates, []string{"google"}) || list[1].TenantID != "beta" {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// Sessions: acme via local and google, beta via local.
	mk := func(hash, tenant, gate string) {
		r := mkUISession(hash, now)
		r.TenantID, r.Gate = tenant, gate
		if err := s.UISessionCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	mk("a-local", "acme", "local")
	mk("a-google", "acme", "google")
	mk("b-local", "beta", "local")
	n, err := s.TenantAuthPolicyRevokeSessions(ctx, "acme", []string{"google"}, now)
	if err != nil || n != 1 {
		t.Fatalf("revoke: n=%d err=%v, want 1", n, err)
	}
	for hash, revoked := range map[string]bool{"a-local": true, "a-google": false, "b-local": false} {
		row, err := s.UISessionGet(ctx, hash)
		if err != nil || row.RevokedAt.IsZero() == revoked {
			t.Errorf("session %s: revoked=%v err=%v, want revoked=%v", hash, !row.RevokedAt.IsZero(), err, revoked)
		}
	}
	if ok, err := s.TenantAuthPolicyDelete(ctx, "acme"); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if ok, err := s.TenantAuthPolicyDelete(ctx, "acme"); err != nil || ok {
		t.Fatalf("second delete: ok=%v err=%v", ok, err)
	}
}

func testTenantAuthPolicyIssuedTokensListsOnlyTheTenantsGateIssuedRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, r := range []store.OperatorTokenDefRow{
		{TenantID: "acme", Subject: "alice", IssuedByGate: "google"},
		{TenantID: "acme", Subject: "bob", IssuedByGate: "local"},
		{TenantID: "acme", Subject: "ops"}, // hand-minted
		{TenantID: "beta", Subject: "carol", IssuedByGate: "google"},
	} {
		r.DefID = fmt.Sprintf("def-%d", i)
		r.Name = fmt.Sprintf("u-tok-%d", i)
		r.TokenHash = fmt.Sprintf("hash-%d", i)
		r.AllowedScopes = []string{"substrate:user"}
		if _, err := s.OperatorTokenDefCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TenantAuthPolicyIssuedTokens(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]string{}
	for _, r := range got {
		subjects[r.Subject] = r.IssuedByGate
	}
	if len(got) != 2 || subjects["alice"] != "google" || subjects["bob"] != "local" {
		t.Fatalf("issued tokens of acme = %+v", got)
	}
	if none, err := s.TenantAuthPolicyIssuedTokens(ctx, "gamma"); err != nil || len(none) != 0 || none == nil {
		t.Fatalf("a tenant with none = %v, %v (want an empty slice)", none, err)
	}
}
