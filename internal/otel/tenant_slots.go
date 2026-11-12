package otel

import (
	"context"
	"sync"
	"time"
)

// exportSlots is the cap on tenant exports in flight: span batches, metric
// pushes and test exports alike. A global cap alone is not fair: a collector
// that never answers holds a slot for a whole export timeout, so a handful of
// tenants pointing at tarpits (each holding a span export, a flush and a
// metric push) would hold every slot and every other tenant's exports would
// drop as busy. Each tenant may therefore hold at most perTenant slots, and no
// export waits longer than wait for one: past it, it fails as busy (and is
// counted by its caller) rather than stall its pipeline behind someone else's.
type exportSlots struct {
	global    chan struct{}
	perTenant int
	wait      time.Duration

	mu      sync.Mutex
	tenants map[string]*tenantSlots
}

// tenantSlots is one tenant's share. refs counts holders and waiters; at zero
// the entry is deleted, so the map holds only tenants with an export under way.
type tenantSlots struct {
	ch   chan struct{}
	refs int
}

func newExportSlots(global, perTenant int, wait time.Duration) *exportSlots {
	return &exportSlots{
		global: make(chan struct{}, global), perTenant: perTenant, wait: wait,
		tenants: map[string]*tenantSlots{},
	}
}

// acquire takes one of tenant's slots and one of the global ones, waiting at
// most s.wait (less if ctx ends first). ok is false when none came in time;
// otherwise release must be called once the export ends.
func (s *exportSlots) acquire(ctx context.Context, tenant string) (release func(), ok bool) {
	s.mu.Lock()
	ts := s.tenants[tenant]
	if ts == nil {
		ts = &tenantSlots{ch: make(chan struct{}, s.perTenant)}
		s.tenants[tenant] = ts
	}
	ts.refs++
	s.mu.Unlock()
	unref := func() {
		s.mu.Lock()
		if ts.refs--; ts.refs == 0 {
			delete(s.tenants, tenant)
		}
		s.mu.Unlock()
	}
	wait, cancel := context.WithTimeout(ctx, s.wait)
	defer cancel()
	select {
	case ts.ch <- struct{}{}:
	case <-wait.Done():
		unref()
		return nil, false
	}
	select {
	case s.global <- struct{}{}:
	case <-wait.Done():
		<-ts.ch
		unref()
		return nil, false
	}
	return func() {
		<-s.global
		<-ts.ch
		unref()
	}, true
}
