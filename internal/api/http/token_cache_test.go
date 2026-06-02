package http

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// hashFailStore wraps a store and injects a non-ErrNotFound outage on the
// auth hot-path lookup, to exercise the fail-closed / don't-cache-outage path.
type hashFailStore struct {
	store.Store
	fail bool
}

func (f *hashFailStore) OperatorTokenDefGetByTokenHash(ctx context.Context, hash string) (store.OperatorTokenDefRow, error) {
	if f.fail {
		return store.OperatorTokenDefRow{}, errors.New("connection refused") // outage, not ErrNotFound
	}
	return f.Store.OperatorTokenDefGetByTokenHash(ctx, hash)
}

// A transient store outage must NOT be cached as a negative — otherwise a
// valid token is locked out for the full TTL after the DB recovers.
func TestResolvePrincipal_OutageNotCached(t *testing.T) {
	s, st := tokenAuthServer(t, "") // no legacy fallback → clean outage path
	seedToken(t, st, "lct_v", "acme", "v", []string{auth.ScopeRunsCreate}, time.Time{})
	fs := &hashFailStore{Store: st, fail: true}
	s.store = fs
	s.EnableTokenCache(30 * time.Second)

	// During the outage the valid token fails closed...
	if _, ok := s.resolvePrincipal(context.Background(), "lct_v"); ok {
		t.Fatal("during a store outage resolution must fail closed")
	}
	// ...but the failure must NOT be memoised.
	if _, _, ok := s.tokenCache.get(auth.HashToken(testTokenPepper, "lct_v")); ok {
		t.Fatal("a store-outage negative must not be cached (would be a sticky lockout)")
	}
	// After recovery the valid token resolves immediately — not served a
	// stale cached negative.
	fs.fail = false
	if _, ok := s.resolvePrincipal(context.Background(), "lct_v"); !ok {
		t.Fatal("after recovery the valid token must resolve (a cached outage-negative was served)")
	}
}

// The cache is bounded: an invalid-bearer spray (distinct hashes) cannot grow
// the map without limit (each negative is cached before the scope check).
func TestTokenCache_BoundedBySize(t *testing.T) {
	c := newTokenCache(30 * time.Second)
	c.maxSize = 4
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	for i := 0; i < 1000; i++ {
		c.put(fmt.Sprintf("h%d", i), auth.Principal{}, false) // distinct negative entries
	}
	if len(c.m) > c.maxSize {
		t.Fatalf("cache grew to %d entries, want <= %d (negative-spray DoS bound)", len(c.m), c.maxSize)
	}
}

func TestTokenCache_HitMissExpiryFlush(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newTokenCache(30 * time.Second)
	c.now = func() time.Time { return now }

	c.put("h1", auth.Principal{Subject: "alice"}, true)
	if p, found, ok := c.get("h1"); !ok || !found || p.Subject != "alice" {
		t.Fatalf("hit expected: ok=%v found=%v p=%+v", ok, found, p)
	}
	if _, _, ok := c.get("absent"); ok {
		t.Error("absent key should miss")
	}
	// Advance past the TTL → expired miss.
	now = now.Add(31 * time.Second)
	if _, _, ok := c.get("h1"); ok {
		t.Error("entry past TTL should miss")
	}
	// Re-put then flush.
	now = time.Unix(2000, 0)
	c.put("h2", auth.Principal{}, false) // negative entry
	if _, found, ok := c.get("h2"); !ok || found {
		t.Errorf("negative entry should be a hit with found=false (ok=%v found=%v)", ok, found)
	}
	c.flush()
	if _, _, ok := c.get("h2"); ok {
		t.Error("flush should drop all entries")
	}
}

func TestTokenCache_DisabledTTLZero(t *testing.T) {
	var c *tokenCache // nil receiver — disabled
	c.put("h", auth.Principal{Subject: "x"}, true)
	if _, _, ok := c.get("h"); ok {
		t.Error("nil cache must always miss")
	}
	c.flush() // must not panic
	c2 := newTokenCache(0)
	c2.put("h", auth.Principal{Subject: "x"}, true)
	if _, _, ok := c2.get("h"); ok {
		t.Error("ttl<=0 cache must always miss")
	}
}

// TestResolvePrincipal_CacheServesAndInvalidates proves the cache fronts
// the DB lookup (a row retired directly in the store still resolves from
// cache) and that invalidateTokenCache flushes it (the retire then takes
// effect). Exercises the RFC L Decision 11 propagation path.
func TestResolvePrincipal_CacheServesAndInvalidates(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.EnableTokenCache(30 * time.Second)
	seedToken(t, st, "lct_x", "acme", "alice", []string{auth.ScopeAdmin}, time.Time{})

	// Prime the cache.
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); !ok {
		t.Fatal("expected initial resolution")
	}
	// Retire the row DIRECTLY in the store (bypassing the connector, so no
	// flush) — the cache should still serve the stale positive result.
	if err := st.OperatorTokenDefSetRetiredAt(context.Background(), "def_alice", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); !ok {
		t.Error("cache should still serve the pre-retire resolution (≤TTL window)")
	}
	// Flush (what a mutation broadcast does) → the retire now takes effect.
	s.invalidateTokenCache(context.Background())
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); ok {
		t.Error("after invalidation, the retired token must no longer resolve")
	}
}
