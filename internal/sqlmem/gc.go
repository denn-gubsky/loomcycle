package sqlmem

import (
	"log/slog"
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
		slog.Warn("sqlmem: touch durable scope failed", "err", err)
	}
}

// startGC launches the durable-scope sweeper. No-op unless TTL GC (ScopeTTLMS)
// OR size-based GC (TotalMaxBytes) is enabled.
func (m *Manager) startGC() {
	m.gcStop = make(chan struct{})
	if m.cfg.ScopeTTLMS <= 0 && m.cfg.TotalMaxBytes <= 0 {
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

// runGC sweeps durable scopes: first by TTL (idle longer than ttl), then by the
// aggregate size budget (TotalMaxBytes, evicting the largest idle scopes). Each
// sub-sweep is independent and only runs when its knob is set.
func (m *Manager) runGC(ttl time.Duration) {
	var dropped int
	if m.cfg.ScopeTTLMS > 0 {
		n, err := m.backend.sweepStale(time.Now().Add(-ttl))
		if err != nil {
			slog.Warn("sqlmem: durable-scope TTL GC sweep failed", "err", err)
		}
		if n > 0 {
			slog.Info("sqlmem: durable-scope TTL GC dropped idle scopes", "scopes", n)
		}
		dropped += n
	}
	if m.cfg.TotalMaxBytes > 0 {
		n, err := m.backend.sweepBudget(m.cfg.TotalMaxBytes)
		if err != nil {
			slog.Warn("sqlmem: durable-scope size GC sweep failed", "err", err)
		}
		if n > 0 {
			slog.Info("sqlmem: durable-scope size GC dropped scopes over the byte budget", "scopes", n, "budget_bytes", m.cfg.TotalMaxBytes)
		}
		dropped += n
	}
	if dropped > 0 {
		// Forget debounce entries so a scope that is recreated after a drop
		// re-records its last_used promptly (the old entry would suppress it).
		m.touchMu.Lock()
		m.lastTouch = make(map[string]time.Time)
		m.touchMu.Unlock()
	}
}
