// Package replay remembers the one-time values (nonces, signatures, jti) the
// gates have already accepted, so a captured credential cannot be presented
// twice while it is still inside its validity window.
//
// The cache is in-process and bounded. It answers for one replica only: a
// gate whose credential can be replayed against a second replica within its
// window records it in the store as well (store.AuthReplayStore).
//
// Two ways in. Admit is for a value whose credential is already
// authenticated (the forward-auth HMAC was checked first). Reserve / Commit /
// Release is for a value that only a remote check can authenticate (an STS
// signature): Reserve holds it in flight, so a concurrent identical
// presentation is refused, and only Commit — after the check passed — records
// it; Release drops a reservation whose check failed. So an unauthenticated
// caller can never leave a value behind: it can hold at most what it has in
// flight.
package replay

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Shared is the cross-replica record of admitted values
// (store.AuthReplayStore): check-and-insert of (scope, key) until expires,
// false when it is already there and live. A gate records in it after its
// own cache, so a value one replica accepted is refused by every other.
type Shared interface {
	AuthReplayAdmit(ctx context.Context, scope, key string, expires, now time.Time) (bool, error)
}

var (
	// ErrReplayed: the value was already admitted and has not expired, or a
	// presentation of it is in flight.
	ErrReplayed = errors.New("replay: value already used")
	// ErrFull: the cache holds its maximum of live values. The caller
	// refuses the credential: admitting it unrecorded would let it be
	// replayed, and evicting a live entry would let THAT one be.
	ErrFull = errors.New("replay: cache full")
)

// DefaultMax bounds a cache built with New(0). At about 150 bytes an entry it
// is ~15 MB at worst, and a value only stays until its window closes.
const DefaultMax = 100_000

// Margin is how long an entry outlives the last moment its value is
// acceptable, so a clock read a moment later than the acceptance check never
// finds the entry already gone.
const Margin = time.Minute

// EntryExpiry is when the entry for a value stamped at `at` and acceptable
// while |now - at| < window may be forgotten.
func EntryExpiry(at time.Time, window time.Duration) time.Time {
	return at.Add(window + Margin)
}

// entry is a committed value (until set) or a reservation in flight.
type entry struct {
	until    time.Time
	inFlight bool
}

// Cache is a set of admitted values with expiry. The zero value is not
// usable; build one with New.
type Cache struct {
	mu  sync.Mutex
	m   map[string]entry
	max int
}

// New builds a cache holding at most max live values (DefaultMax when
// max ≤ 0).
func New(max int) *Cache {
	if max <= 0 {
		max = DefaultMax
	}
	return &Cache{m: make(map[string]entry), max: max}
}

// Admit records key as used until expires, in one check-and-insert step: of
// two concurrent presentations of one value, exactly one is admitted. now is
// the caller's clock read, the same one it judged the credential's window by.
func (c *Cache) Admit(key string, expires, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.claimLocked(key, now); err != nil {
		return err
	}
	c.m[key] = entry{until: expires}
	return nil
}

// Reserve holds key in flight. Every Reserve that returns nil must be
// followed by exactly one Commit or Release.
func (c *Cache) Reserve(key string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.claimLocked(key, now); err != nil {
		return err
	}
	c.m[key] = entry{inFlight: true}
	return nil
}

// Commit turns a reservation into a used value, kept until expires.
func (c *Cache) Commit(key string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = entry{until: expires}
}

// Release drops a reservation. A committed value is left alone.
func (c *Cache) Release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && e.inFlight {
		delete(c.m, key)
	}
}

func (c *Cache) claimLocked(key string, now time.Time) error {
	if e, ok := c.m[key]; ok && (e.inFlight || now.Before(e.until)) {
		return ErrReplayed
	}
	if len(c.m) >= c.max {
		// Sweep only when full: an O(n) pass amortised over max inserts.
		for k, e := range c.m {
			if !e.inFlight && !now.Before(e.until) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			return ErrFull
		}
	}
	return nil
}

// Len is the number of values held (reservations and committed, expired ones
// included until a sweep).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
