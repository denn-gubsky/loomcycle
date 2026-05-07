package http

import (
	"sync"
	"testing"
	"time"
)

// tryLock + release round-trip — basic sanity that the entry's mutex
// is actually acquired and the refcount returns to zero.
func TestSessionLockMap_TryLockReleaseRoundTrip(t *testing.T) {
	m := newSessionLockMap()
	release, ok := m.tryLock("s1")
	if !ok {
		t.Fatal("first tryLock should succeed")
	}
	if m.size() != 1 {
		t.Errorf("size after tryLock: got %d, want 1", m.size())
	}

	release()

	// After release, the entry stays in the map (with refcount=0)
	// until gc reclaims it. This avoids constructing a fresh mutex on
	// every call for an active-but-bursty session.
	if m.size() != 1 {
		t.Errorf("size after release: got %d, want 1 (entry kept until gc)", m.size())
	}
}

// Concurrent tryLock — second caller fails with ok=false instead of
// blocking. This is the v0.3.2 contract.
func TestSessionLockMap_ConcurrentBusy(t *testing.T) {
	m := newSessionLockMap()
	release, ok := m.tryLock("s1")
	if !ok {
		t.Fatal("first tryLock should succeed")
	}
	defer release()

	_, ok2 := m.tryLock("s1")
	if ok2 {
		t.Fatal("second tryLock should fail (busy)")
	}
}

// gc removes idle entries whose refcount is zero. Active entries
// (refcount > 0) and recently-used entries are preserved.
func TestSessionLockMap_GCReclaimsIdle(t *testing.T) {
	m := newSessionLockMap()

	// Three sessions:
	//   idle  — released, will age past the cutoff
	//   busy  — still held when gc runs
	//   fresh — released, but recently
	idleRelease, _ := m.tryLock("idle")
	idleRelease()

	busyRelease, _ := m.tryLock("busy")
	defer busyRelease()

	// Sleep past the cutoff for the idle entry. The "fresh" entry
	// will be created AFTER the sleep so it dodges the cutoff.
	time.Sleep(20 * time.Millisecond)

	freshRelease, _ := m.tryLock("fresh")
	freshRelease()

	if m.size() != 3 {
		t.Fatalf("pre-gc size: got %d, want 3", m.size())
	}

	removed := m.gc(15 * time.Millisecond)
	if removed != 1 {
		t.Errorf("gc removed %d entries, want 1 (idle only)", removed)
	}
	if m.size() != 2 {
		t.Errorf("post-gc size: got %d, want 2 (busy + fresh)", m.size())
	}

	// idle is gone — a new tryLock should find a fresh entry.
	if _, ok := m.tryLock("idle"); !ok {
		t.Errorf("post-gc tryLock(idle) should succeed; got busy")
	}
}

// gc never reclaims an entry whose refcount > 0, even if lastAccessed
// is old. A long-running session must not have its lock yanked
// out from under it.
func TestSessionLockMap_GCNeverEvictsHeld(t *testing.T) {
	m := newSessionLockMap()
	release, ok := m.tryLock("held")
	if !ok {
		t.Fatal("tryLock should succeed")
	}
	defer release()

	// Sleep so lastAccessed is well past the cutoff we'll pass.
	time.Sleep(20 * time.Millisecond)

	removed := m.gc(15 * time.Millisecond)
	if removed != 0 {
		t.Errorf("gc removed %d entries, want 0 (held is busy)", removed)
	}
	if m.size() != 1 {
		t.Errorf("size: got %d, want 1", m.size())
	}
}

// gc + concurrent tryLock — a tryLock racing with gc must always
// observe a consistent state: either the entry is reclaimed (and the
// tryLock creates a fresh one) or the entry is kept (and the tryLock
// uses the existing mutex). The test fires N concurrent tryLocks
// against a single ID while gc cycles in another goroutine.
//
// We don't assert exact numbers — just that no panic / deadlock fires
// and that the lock map returns to a sane state at the end.
func TestSessionLockMap_GCRaceWithTryLock(t *testing.T) {
	m := newSessionLockMap()

	const goroutines = 16
	const iterationsPerGoroutine = 200

	var wg sync.WaitGroup
	stopGC := make(chan struct{})

	go func() {
		for {
			select {
			case <-stopGC:
				return
			default:
				_ = m.gc(0) // aggressive: cutoff = now means every refcount=0 entry is eligible
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterationsPerGoroutine; i++ {
				release, ok := m.tryLock("racy")
				if !ok {
					continue
				}
				release()
			}
		}()
	}
	wg.Wait()
	close(stopGC)

	// Final gc should clean any leftover entry.
	_ = m.gc(0)
	if m.size() != 0 {
		t.Errorf("size after race + final gc: got %d, want 0", m.size())
	}
}
