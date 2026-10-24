// Package replay remembers the one-time values (nonces, signatures, jti) the
// gates have already accepted, so a captured credential cannot be presented
// twice while it is still inside its validity window.
//
// The cache is in-process and bounded. It answers for one replica only: a
// gate whose credential can be replayed against a second replica within its
// window needs a store-backed admitter beside it (the AWS IAM gate has one).
package replay

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrReplayed: the value was already admitted and has not expired.
	ErrReplayed = errors.New("replay: value already used")
	// ErrFull: the cache holds its maximum of unexpired values. The caller
	// refuses the credential: admitting it unrecorded would let it be
	// replayed, and evicting a live entry would let THAT one be.
	ErrFull = errors.New("replay: cache full")
)

// DefaultMax bounds a cache built with New(0). At about 150 bytes an entry it
// is ~15 MB at worst, and a value only stays until its window closes.
const DefaultMax = 100_000

// Cache is a set of admitted values with expiry. The zero value is not
// usable; build one with New.
type Cache struct {
	mu  sync.Mutex
	m   map[string]time.Time
	max int
	now func() time.Time
}

// New builds a cache holding at most max unexpired values (DefaultMax when
// max ≤ 0).
func New(max int) *Cache {
	if max <= 0 {
		max = DefaultMax
	}
	return &Cache{m: make(map[string]time.Time), max: max, now: time.Now}
}

// SetClock replaces the cache's clock (tests).
func (c *Cache) SetClock(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Admit records key as used until expires. It is check-and-insert in one
// step, so two concurrent presentations of one value admit exactly one.
func (c *Cache) Admit(key string, expires time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if until, ok := c.m[key]; ok && now.Before(until) {
		return ErrReplayed
	}
	if len(c.m) >= c.max {
		// Sweep only when full: an O(n) pass amortised over max inserts.
		for k, until := range c.m {
			if !now.Before(until) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			return ErrFull
		}
	}
	c.m[key] = expires
	return nil
}

// Len is the number of values held, expired ones included until a sweep.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
