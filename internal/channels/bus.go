// Package channels implements the in-process notification half of
// the v0.8.4 Channel tool. Storage durability lives in the store
// layer (sqlite + postgres); this package adds the "you have new
// mail" tap so same-process subscribers waiting in long-poll mode
// don't have to spin on the DB.
//
// Cross-process notification (multi-replica deployments) is OUT OF
// SCOPE for v0.8.4 — subscribers on a different loomcycle instance
// fall back to plain polling. A future v0.9.x can swap Bus's
// backplane for Postgres LISTEN/NOTIFY or Redis pub/sub without
// changing the tool surface.
//
// Concurrency: Bus is safe for concurrent Notify + Wait calls. Each
// channel has its own waiter slice guarded by the same mutex; the
// slice is rewritten on every wake (cheap when N is small — typical
// agents have <10 active subscriptions per instance).
package channels

import (
	"context"
	"sync"
	"time"
)

// Bus is the in-process notification bus.
type Bus struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

// NewBus returns a fresh, empty bus. Single instance per loomcycle
// process; injected into the Channel tool at registration time.
func NewBus() *Bus {
	return &Bus{waiters: make(map[string][]chan struct{})}
}

// Notify wakes every subscriber currently blocked in Wait on the
// supplied channel name. Idempotent + non-blocking — Notify on an
// empty channel is free.
//
// The publish path calls this AFTER the storage write commits, so
// any waiter that wakes is guaranteed to find at least one new row.
func (b *Bus) Notify(channel string) {
	b.mu.Lock()
	waiters := b.waiters[channel]
	if len(waiters) == 0 {
		b.mu.Unlock()
		return
	}
	delete(b.waiters, channel)
	b.mu.Unlock()

	// Wake outside the lock — a waiter might race to re-register
	// before we finish, but that's the next-cycle's problem.
	// Buffered (cap 1) means this is non-blocking.
	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Wait blocks the caller until either:
//   - Notify(channel) fires (returns true)
//   - timeout elapses (returns false)
//   - ctx is cancelled (returns false)
//
// A zero or negative timeout returns false immediately (no waiter
// is registered — saves an allocation on every Subscribe op with
// wait_ms unset).
//
// Spurious returns are NOT possible: the only path that closes/
// sends on the waker is Notify. Callers should still re-query the
// store after Wait returns true, because Notify only signals
// presence-of-new-data, not which specific rows are new.
func (b *Bus) Wait(ctx context.Context, channel string, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	waker := make(chan struct{}, 1)

	b.mu.Lock()
	b.waiters[channel] = append(b.waiters[channel], waker)
	b.mu.Unlock()

	defer b.removeWaiter(channel, waker)

	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case <-waker:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// removeWaiter is the defer cleanup. Removes the waker from the
// slice in case Wait returned via timeout or ctx (in which case
// Notify never drained the slice). Idempotent — if Notify already
// drained, the slice is gone and this is a no-op.
func (b *Bus) removeWaiter(channel string, waker chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	waiters := b.waiters[channel]
	for i, w := range waiters {
		if w == waker {
			// Replace with last and trim — order doesn't matter
			// for wake semantics.
			waiters[i] = waiters[len(waiters)-1]
			waiters = waiters[:len(waiters)-1]
			break
		}
	}
	if len(waiters) == 0 {
		delete(b.waiters, channel)
	} else {
		b.waiters[channel] = waiters
	}
}
