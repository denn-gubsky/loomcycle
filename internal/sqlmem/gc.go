package sqlmem

import (
	"log"
	"time"
)

// gc.go — RFC AA Phase 3d: durable-scope garbage collection. A background
// sweeper drops durable (agent/user) scopes idle longer than ScopeTTLMS. OFF by
// default (ScopeTTLMS=0) because GC discards data — a scope that comes back
// after the TTL has lost its tables. Run scopes are never GC'd (they drop at
// run-end). The sweep reuses the per-tier fenced/owned drop, so it adds no new
// trust surface; concurrent sweeps across replicas are safe (idempotent drops).

const gcDefaultIntervalMS = 3600_000 // 1 hour

// touchKey is the debounce key for a durable scope.
func touchKey(key ScopeKey) string {
	return key.Tenant + "\x1f" + key.Scope + "\x1f" + key.ScopeID
}

// touchDebounce coalesces touches of one scope to at most ~once per window — a
// fraction of the TTL, clamped — so the hot path writes last_used rarely while
// staying well under the TTL.
func (m *Manager) touchDebounce() time.Duration {
	d := time.Duration(m.cfg.ScopeTTLMS) * time.Millisecond / 20
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// touch records that a durable scope was just used (debounced). No-op when GC
// is off or for the run scope.
func (m *Manager) touch(key ScopeKey) {
	if m.cfg.ScopeTTLMS <= 0 || key.Scope == runScope {
		return
	}
	id := touchKey(key)
	now := time.Now()
	m.touchMu.Lock()
	if last, ok := m.lastTouch[id]; ok && now.Sub(last) < m.touchDebounce() {
		m.touchMu.Unlock()
		return
	}
	m.lastTouch[id] = now
	m.touchMu.Unlock()
	if err := m.backend.touchScope(key); err != nil {
		log.Printf("sqlmem: touch durable scope: %v", err)
	}
}

// startGC launches the durable-scope sweeper (no-op when ScopeTTLMS <= 0).
func (m *Manager) startGC() {
	m.gcStop = make(chan struct{})
	if m.cfg.ScopeTTLMS <= 0 {
		return
	}
	interval := time.Duration(m.cfg.GCIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = gcDefaultIntervalMS * time.Millisecond
	}
	ttl := time.Duration(m.cfg.ScopeTTLMS) * time.Millisecond
	m.gcDone = make(chan struct{})
	stop, done := m.gcStop, m.gcDone
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.runGC(ttl)
			}
		}
	}()
}

// stopGC signals the sweeper and JOINS it, so no sweep runs after Close.
func (m *Manager) stopGC() {
	if m.gcStop != nil {
		close(m.gcStop)
		m.gcStop = nil
	}
	if m.gcDone != nil {
		<-m.gcDone
		m.gcDone = nil
	}
}

// runGC sweeps durable scopes idle longer than ttl.
func (m *Manager) runGC(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	dropped, err := m.backend.sweepStale(cutoff)
	if err != nil {
		log.Printf("sqlmem: durable-scope GC sweep: %v", err)
	}
	if dropped > 0 {
		log.Printf("sqlmem: durable-scope GC dropped %d idle scope(s)", dropped)
		// Forget debounce entries so a scope that is recreated after a drop
		// re-records its last_used promptly (the old entry would suppress it).
		m.touchMu.Lock()
		m.lastTouch = make(map[string]time.Time)
		m.touchMu.Unlock()
	}
}
