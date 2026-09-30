package webhook

import (
	"fmt"
	"net/http"
	"testing"
)

// These drive the receiver against a real sqlite store (helpers in
// dedup_scope_test.go) so the buckets are keyed by the def the real
// lookup resolves.

func rateLimitedDef(tenant string, rpm, burst int) map[string]any {
	def := signedSpawnDef(tenant)
	def["rate_limit"] = map[string]any{"requests_per_minute": rpm, "burst": burst}
	return def
}

// Two tenants' same-named webhooks are different webhooks: one tenant's
// traffic must not drain the other's bucket, size it, or refill it.
func TestReceiver_SameNamedWebhookInAnotherTenant_NotThrottledByNeighbour(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		globexRPM, globexBurst int
	}{
		{"same limits", 1, 1},
		{"different limits", 600, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openScopeStore(t)
			putWebhookDef(t, st, "acme", "gh", rateLimitedDef("acme", 1, 1))
			putWebhookDef(t, st, "globex", "gh", rateLimitedDef("globex", tc.globexRPM, tc.globexBurst))
			fr := &storeRunner{st: st}
			rec := newScopeReceiver(st, fr, nil)

			code, got := postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"a1"}`), "")
			assertFreshRun(t, "acme first", code, got)
			if code, got = postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"a2"}`), ""); code != http.StatusTooManyRequests {
				t.Fatalf("acme second = %d %v; want 429 (its own 1-burst bucket is empty)", code, got)
			}
			code, got = postSigned(t, rec, "/v1/_webhooks/globex/gh", []byte(`{"goal":"g1"}`), "")
			assertFreshRun(t, "globex first", code, got)
			// And globex's delivery must not have refilled acme's bucket.
			if code, got = postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"a3"}`), ""); code != http.StatusTooManyRequests {
				t.Errorf("acme third = %d %v; want 429 (globex's traffic refilled acme's bucket)", code, got)
			}
		})
	}
}

// A rate_limit edit (a new active def version) applies to the next delivery,
// in both directions, without a restart.
func TestReceiver_RateLimitEdit_TakesEffectWithoutRestart(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "acme", "gh", rateLimitedDef("acme", 1, 1))
	fr := &storeRunner{st: st}
	rec := newScopeReceiver(st, fr, nil)
	n := 0
	post := func() (int, map[string]string) {
		n++
		return postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(fmt.Sprintf(`{"goal":"g%d"}`, n)), "")
	}

	code, got := post()
	assertFreshRun(t, "first", code, got)
	if code, got = post(); code != http.StatusTooManyRequests {
		t.Fatalf("second = %d %v; want 429 at burst 1", code, got)
	}

	// Loosen: the empty 1-burst bucket must not outlive the edit.
	putWebhookDef(t, st, "acme", "gh", rateLimitedDef("acme", 600, 5))
	for i := 0; i < 5; i++ {
		code, got = post()
		assertFreshRun(t, fmt.Sprintf("after loosening, delivery %d", i+1), code, got)
	}

	// Tighten: the new burst of 1 applies at once.
	putWebhookDef(t, st, "acme", "gh", rateLimitedDef("acme", 1, 1))
	code, got = post()
	assertFreshRun(t, "after tightening", code, got)
	if code, got = post(); code != http.StatusTooManyRequests {
		t.Fatalf("second after tightening = %d %v; want 429 at burst 1", code, got)
	}
}
