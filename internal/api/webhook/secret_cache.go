package webhook

import (
	"context"
	"sync"
	"time"
)

// maxCachedSecrets bounds cachedCredentials: one entry per credential a
// webhook names, so far above any deployment's count; past it nothing more is
// cached (each delivery then reads its credential, as before the cache).
const maxCachedSecrets = 4096

// cachedCredentials is a CredentialResolver that keeps each secret it read
// for storedSecretTTL. A secret stored as a credential is read and decrypted
// to check a delivery's signature — before anything is known of the sender —
// so without it every unsigned post to such a webhook cost a database read and
// a decryption. Only a found secret is kept: a missing one, or an error, is
// asked again next time. Values are compared in-process only, never logged.
type cachedCredentials struct {
	inner CredentialResolver
	now   func() time.Time

	mu sync.Mutex
	m  map[secretCacheKey]cachedSecret
}

type secretCacheKey struct{ tenant, name string }

type cachedSecret struct {
	value   string
	expires time.Time
}

func newCachedCredentials(inner CredentialResolver, now func() time.Time) *cachedCredentials {
	return &cachedCredentials{inner: inner, now: now, m: map[secretCacheKey]cachedSecret{}}
}

func (c *cachedCredentials) ResolveToCompare(ctx context.Context, tenantID, name string) (string, bool, error) {
	k := secretCacheKey{tenantID, name}
	now := c.now()
	c.mu.Lock()
	e, ok := c.m[k]
	c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.value, true, nil
	}
	v, found, err := c.inner.ResolveToCompare(ctx, tenantID, name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || !found || v == "" {
		delete(c.m, k)
		return v, found, err
	}
	if _, held := c.m[k]; !held && len(c.m) >= maxCachedSecrets {
		c.sweepLocked(now)
	}
	if _, held := c.m[k]; held || len(c.m) < maxCachedSecrets {
		c.m[k] = cachedSecret{value: v, expires: now.Add(storedSecretTTL)}
	}
	return v, true, nil
}

// sweep drops the entries that have lapsed.
func (c *cachedCredentials) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(c.now())
}

func (c *cachedCredentials) sweepLocked(now time.Time) {
	for k, e := range c.m {
		if !now.Before(e.expires) {
			delete(c.m, k)
		}
	}
}
