package http

import (
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// RFC L Decision 11 — the per-replica auth-token resolution cache.
//
// resolvePrincipal does an indexed peppered-hash DB lookup per
// authenticated request. This cache memoises the WHOLE resolution
// outcome (token hit, legacy fallback, or not-found) keyed by the
// token's SHA-256 hash — never a secret — so a hot path of repeated
// bearers skips the DB. Entries expire after a short TTL (default 30s,
// LOOMCYCLE_AUTH_CACHE_TTL_SECONDS); the TTL is the worst-case
// enforcement lag for a revocation if a cross-replica invalidation
// NOTIFY is dropped. A mutation (create/rotate/retire) flushes the local
// cache immediately and broadcasts a flush to peer replicas, so typical
// propagation is one backplane round-trip (50–200ms).
//
// TTL <= 0 disables the cache entirely (every request does the direct
// lookup — immediate revocation, the safest setting for an operator who
// prefers correctness over the DB-hit reduction).

type tokenCacheEntry struct {
	principal auth.Principal
	found     bool
	expiresAt time.Time
}

// tokenCacheMaxEntries bounds the map. resolvePrincipal caches a negative for
// every distinct bearer hash, and it runs BEFORE the scope check, so an
// attacker spraying distinct random bearers at any authed route would
// otherwise grow the map without bound for the TTL (a memory-amplification
// DoS — each 256-bit bearer is a fresh key). At the cap, put() sweeps expired
// entries and, if still full, skips caching (correctness preserved: the next
// request does the direct lookup). 16384 entries ≈ a few MB — ample for the
// real working set of live tokens, a hard ceiling for negative spray.
const tokenCacheMaxEntries = 16384

type tokenCache struct {
	mu      sync.RWMutex
	m       map[string]tokenCacheEntry
	ttl     time.Duration
	maxSize int
	now     func() time.Time // injectable for tests
}

func newTokenCache(ttl time.Duration) *tokenCache {
	return &tokenCache{m: make(map[string]tokenCacheEntry), ttl: ttl, maxSize: tokenCacheMaxEntries, now: time.Now}
}

// get returns the cached resolution for a hash. ok=false on a miss or an
// expired entry (or when the cache is disabled).
func (c *tokenCache) get(hash string) (auth.Principal, bool /*found*/, bool /*ok*/) {
	if c == nil || c.ttl <= 0 {
		return auth.Principal{}, false, false
	}
	c.mu.RLock()
	e, ok := c.m[hash]
	c.mu.RUnlock()
	if !ok || c.now().After(e.expiresAt) {
		return auth.Principal{}, false, false
	}
	return e.principal, e.found, true
}

// put records a resolution. No-op when the cache is disabled. Bounded by
// maxSize: at the cap it first evicts expired entries, then — if still full —
// skips caching this entry (the next request does a direct lookup). This caps
// the negative-cache memory an invalid-bearer spray can consume.
func (c *tokenCache) put(hash string, p auth.Principal, found bool) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[hash]; !exists && c.maxSize > 0 && len(c.m) >= c.maxSize {
		now := c.now()
		for k, e := range c.m {
			if now.After(e.expiresAt) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.maxSize {
			return // still full of live entries — skip caching, stay bounded
		}
	}
	c.m[hash] = tokenCacheEntry{principal: p, found: found, expiresAt: c.now().Add(c.ttl)}
}

// flush drops all entries. Called on a local token mutation and on
// receipt of a cross-replica invalidation. Flush-all (not per-hash)
// because a create/rotate/retire can change ANY resolution — including
// enabling/disabling the legacy fallback (the no-lockout gate).
func (c *tokenCache) flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.m = make(map[string]tokenCacheEntry)
	c.mu.Unlock()
}
