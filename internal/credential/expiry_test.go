package credential

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// expiringEngine stores "gh" (value "sk-expiring") at tenant scope, expiring
// at the returned time, and gives the engine a clock the test moves.
func expiringEngine(t *testing.T) (e *Engine, expiresAt time.Time, setNow func(time.Time)) {
	t.Helper()
	e = newEngine(t, testKey(1))
	expiresAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(-time.Second)
	e.now = func() time.Time { return now }
	if _, err := e.PutInline(context.Background(), Identity{TenantID: "t", Scope: "tenant", Name: "gh"}, "sk-expiring", &expiresAt, nil, nil); err != nil {
		t.Fatalf("PutInline: %v", err)
	}
	return e, expiresAt, func(t time.Time) { now = t }
}

// Every by-name entry point hands the credential out before its expires_at
// and refuses it from that instant on — the boundary itself included.
func TestEngine_ExpiredCredentialIsRefusedByEveryLookup(t *testing.T) {
	ctx := netguard.WithPinnable(context.Background())
	const target = "https://api.example.com/v1"

	for _, tc := range []struct {
		name    string
		offset  time.Duration
		expired bool
	}{
		{"one second before", -time.Second, false},
		{"at expires_at", 0, true},
		{"an hour after", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, expiresAt, setNow := expiringEngine(t)
			setNow(expiresAt.Add(tc.offset))

			checkErr := func(op string, err error) {
				t.Helper()
				if !tc.expired {
					if err != nil {
						t.Errorf("%s: %v, want it to resolve", op, err)
					}
					return
				}
				if !errors.Is(err, ErrExpired) {
					t.Errorf("%s: err = %v, want ErrExpired", op, err)
					return
				}
				msg := err.Error()
				if !strings.Contains(msg, `"gh"`) || !strings.Contains(msg, "2026-10-01T12:00:00Z") {
					t.Errorf("%s: error %q should name the credential and when it expired", op, msg)
				}
				if strings.Contains(msg, "sk-expiring") {
					t.Errorf("%s: error carries the secret value", op)
				}
			}

			res, found, err := e.Resolve(ctx, "t", "", "", "gh")
			checkErr("Resolve", err)
			if got := found && res.Value == "sk-expiring"; got == tc.expired {
				t.Errorf("Resolve: handed out=%v, want %v", got, !tc.expired)
			}

			res, found, err = e.ResolveFor(ctx, "t", "", "", "gh", target)
			checkErr("ResolveFor", err)
			if got := found && res.Value == "sk-expiring"; got == tc.expired {
				t.Errorf("ResolveFor: handed out=%v, want %v", got, !tc.expired)
			}

			out, unresolved, err := e.Substitute(ctx, "t", "", "", "Bearer $cred:gh", target, func(string) {})
			checkErr("Substitute", err)
			if tc.expired && (out != "" || unresolved != nil) {
				t.Errorf("Substitute: out=%q unresolved=%v, want nothing to send", out, unresolved)
			}
			if !tc.expired && out != "Bearer sk-expiring" {
				t.Errorf("Substitute: out=%q", out)
			}

			// HasKey says no key, and says why: the name is not absent, so
			// a caller that falls back to the operator's key on "absent"
			// must not here.
			has, err := e.HasKey(ctx, "t", "", "", "gh")
			checkErr("HasKey", err)
			if has == tc.expired {
				t.Errorf("HasKey = %v; want %v", has, !tc.expired)
			}

			found, usable, err := e.HasKeyFor(ctx, "t", "", "", "gh", target)
			checkErr("HasKeyFor", err)
			if !found || usable == tc.expired {
				t.Errorf("HasKeyFor = found %v usable %v; want found true usable %v", found, usable, !tc.expired)
			}
		})
	}
}

// An expired user credential does not quietly fall through to the tenant
// one of the same name: the user is told theirs expired.
func TestEngine_ExpiredUserCredentialDoesNotFallThroughToTenant(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "tenant", Name: "slack"}, "team-token", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Created while still valid, then the clock moves past it.
	e.now = func() time.Time { return past.Add(-time.Minute) }
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "slack"}, "alice-token", &past, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.now = time.Now
	res, found, err := e.Resolve(ctx, "t", "", "alice", "slack")
	if !errors.Is(err, ErrExpired) || found || res.Value != "" {
		t.Fatalf("Resolve for alice = %q found=%v err=%v; want ErrExpired and no value", res.Value, found, err)
	}
	// Someone without a user credential still gets the tenant's.
	if res, found, err := e.Resolve(ctx, "t", "", "bob", "slack"); err != nil || !found || res.Value != "team-token" {
		t.Fatalf("Resolve for bob = %q found=%v err=%v; want the tenant token", res.Value, found, err)
	}
}

// A credential created already expired could never be used, so create
// refuses it — at the boundary too — and stores nothing.
func TestEngine_PutInlineRefusesAnExpiryNotInTheFuture(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	id := Identity{TenantID: "t", Scope: "tenant", Name: "stale"}
	for _, at := range []time.Time{now.Add(-24 * time.Hour), now} {
		_, err := e.PutInline(ctx, id, "v", &at, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "not in the future") {
			t.Errorf("PutInline(expires_at %s) err = %v, want it refused", at.Format(time.RFC3339), err)
		}
	}
	if _, found, _ := e.Resolve(ctx, "t", "", "", "stale"); found {
		t.Fatal("a refused create stored the credential")
	}
	later := now.Add(time.Second)
	if _, err := e.PutInline(ctx, id, "v", &later, nil, nil); err != nil {
		t.Fatalf("PutInline one second ahead: %v", err)
	}
}

// An expiry the store cannot hold is refused at create. SQLite keeps it as
// int64 nanoseconds, so year 2999 would wrap to a past time and the new
// credential would be refused as expired from the start.
func TestEngine_PutInlineRefusesAnExpiryTheStoreCannotHold(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := Identity{TenantID: "t", Scope: "tenant", Name: "far"}
	far := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := e.PutInline(ctx, id, "v", &far, nil, nil); err == nil || !strings.Contains(err.Error(), "outside the range") {
		t.Fatalf("PutInline(2999) err = %v, want it refused", err)
	}
	if _, found, _ := e.Resolve(ctx, "t", "", "", "far"); found {
		t.Fatal("a refused create stored the credential")
	}
	near := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := e.PutInline(ctx, id, "v", &near, nil, nil); err != nil {
		t.Fatalf("PutInline(2200): %v", err)
	}
	if res, found, err := e.Resolve(ctx, "t", "", "", "far"); err != nil || !found || res.Value != "v" {
		t.Fatalf("Resolve with a 2200 expiry = %q found=%v err=%v", res.Value, found, err)
	}
}

// get and list keep showing an expired credential, flagged, so it can be
// seen and rotated; creating it again without expires_at makes it resolve.
func TestEngine_ExpiredCredentialIsListedAndRotatable(t *testing.T) {
	e, expiresAt, setNow := expiringEngine(t)
	ctx := context.Background()
	id := Identity{TenantID: "t", Scope: "tenant", Name: "gh"}

	meta, err := e.Get(ctx, id)
	if err != nil || meta.Expired {
		t.Fatalf("Get before expiry: expired=%v err=%v; want false", meta.Expired, err)
	}
	setNow(expiresAt)
	if meta, err = e.Get(ctx, id); err != nil || !meta.Expired {
		t.Fatalf("Get after expiry: expired=%v err=%v; want true", meta.Expired, err)
	}
	list, err := e.List(ctx, "t", "tenant", "")
	if err != nil || len(list) != 1 || !list[0].Expired {
		t.Fatalf("List after expiry = %+v err=%v; want the one row, expired", list, err)
	}

	rotated, err := e.PutInline(ctx, id, "sk-rotated", nil, nil, nil)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Expired || rotated.ExpiresAt != nil {
		t.Errorf("rotated meta = expired %v expires_at %v; want neither", rotated.Expired, rotated.ExpiresAt)
	}
	if res, found, err := e.Resolve(ctx, "t", "", "", "gh"); err != nil || !found || res.Value != "sk-rotated" {
		t.Fatalf("Resolve after rotation = %q found=%v err=%v", res.Value, found, err)
	}
}
