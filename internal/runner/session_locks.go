package runner

import (
	"sync"
	"time"
)

// SessionLockMap is the GC'd backing store for the per-session
// continuation mutex introduced in v0.3.2. Each session ID has at most
// one *sync.Mutex; concurrent attempts at a continuation on the same
// session fast-fail with ErrSessionBusy via TryLock.
//
// Lives in the runner package (not internal/api/http) because both the
// HTTP and gRPC wire surfaces target the same session_id; a single
// lock map coordinates concurrent continuations across wires.
//
// Lifecycle:
//
//   - tryLock acquires the per-session mutex (with TryLock semantics)
//     and bumps the entry's refcount + lastAccessed timestamp. Returns a
//     release closure that decrements the refcount when the caller is
//     done.
//   - gc walks the map and removes entries whose refcount=0 AND whose
//     lastAccessed is older than maxIdle. This avoids racing with active
//     callers (refcount>0) and avoids yanking locks for sessions that
//     have just woken up (lastAccessed is recent).
//
// The map is protected by mu; per-entry mutexes are independent.
// tryLock is two operations: bump-counter under mu, then TryLock the
// per-entry mutex. Bumping under mu ensures the entry isn't reclaimed
// by a concurrent gc between the LoadOrStore and the TryLock.
type SessionLockMap struct {
	mu      sync.Mutex
	entries map[string]*sessionLockEntry
}

// sessionLockEntry holds the per-session mutex plus its bookkeeping
// for the GC. lastAccessed is updated on every TryLock; refCount is
// incremented on TryLock acquisition and decremented on release.
type sessionLockEntry struct {
	mu           sync.Mutex
	refCount     int       // number of outstanding TryLock holders (always 0 or 1 at v0.5.0; ≥1 in future variants)
	lastAccessed time.Time // monotonically advances on each TryLock
}

// NewSessionLockMap constructs an empty map. Caller (typically
// http.Server, which constructs one and shares the reference with
// the gRPC server) holds the lifetime.
func NewSessionLockMap() *SessionLockMap {
	return &SessionLockMap{
		entries: make(map[string]*sessionLockEntry),
	}
}

// tryLock acquires the session-scoped mutex for id. Returns
// (release, true) on success; (nil, false) if another caller already
// holds the lock.
//
// On success, lastAccessed is set to now and refCount is incremented.
// The returned release closure unlocks the per-entry mutex AND
// decrements refCount, gating future GC.
func (m *SessionLockMap) TryLock(id string) (release func(), ok bool) {
	m.mu.Lock()
	e, exists := m.entries[id]
	if !exists {
		e = &sessionLockEntry{}
		m.entries[id] = e
	}
	if !e.mu.TryLock() {
		// Already held; we did NOT acquire — must NOT bump refCount
		// (the holder's release will decrement once). lastAccessed
		// stays at its previous value; this caller's failed attempt
		// doesn't restart the idle clock.
		m.mu.Unlock()
		return nil, false
	}
	e.refCount++
	e.lastAccessed = time.Now()
	m.mu.Unlock()

	return func() {
		// Unlock the per-entry mutex first so a waiting caller can
		// observe the entry as available. Then decrement refCount
		// under the map mutex so gc sees the final value atomically.
		e.mu.Unlock()
		m.mu.Lock()
		if e.refCount > 0 {
			e.refCount--
		}
		m.mu.Unlock()
	}, true
}

// gc removes entries whose refCount is zero AND whose lastAccessed is
// older than maxIdle. Returns the number of entries removed.
//
// Active entries (refCount > 0) are never reclaimed regardless of
// lastAccessed. Recently-used idle entries are also kept — the
// refcount=0 + idle-aged window means a session that completes a run
// and is then quiet for >= maxIdle gets its lock pruned, while a
// session that just released is given another GC cycle's grace period
// before being eligible.
func (m *SessionLockMap) GC(maxIdle time.Duration) int {
	cutoff := time.Now().Add(-maxIdle)
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	for id, e := range m.entries {
		if e.refCount == 0 && e.lastAccessed.Before(cutoff) {
			delete(m.entries, id)
			removed++
		}
	}
	return removed
}

// Size returns the number of entries currently in the map. Used by
// tests to assert the GC actually reclaims idle entries.
func (m *SessionLockMap) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
