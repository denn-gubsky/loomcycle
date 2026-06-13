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
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
)

// Bus is the in-process notification bus. v0.12.3 Phase 4 adds an
// optional backplane fanout: when SetBackplane is called, every local
// Notify ALSO publishes on `loomcycle.channel` so remote replicas'
// SubscribeBackplane goroutine wakes their local Wait callers. The
// PostgresBackplane self-filter prevents the originating replica
// from looping.
type Bus struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}

	// bp is the cluster-mode backplane. Nil = single-replica mode.
	bp coord.Backplane

	// bpPublishTimeout bounds the best-effort backplane publish in Notify.
	// Notify runs synchronously on the publish path (after the storage
	// commit), so an unbounded context.Background() publish could hang that
	// path indefinitely if the backplane stalls (e.g. an exhausted pgx pool
	// or a slow pg_notify). Set in NewBus; a missed fanout degrades to plain
	// polling on remote replicas, so a bounded best-effort publish is safe.
	bpPublishTimeout time.Duration
}

// channelBackplaneEvent is the wire payload on `loomcycle.channel`.
// Tiny — just the channel name; the receiving Bus re-queries its
// store, same as the local Notify path.
type channelBackplaneEvent struct {
	Channel string `json:"channel"`
}

// NewBus returns a fresh, empty bus. Single instance per loomcycle
// process; injected into the Channel tool at registration time.
func NewBus() *Bus {
	return &Bus{
		waiters:          make(map[string][]chan struct{}),
		bpPublishTimeout: 5 * time.Second,
	}
}

// Notify wakes every subscriber currently blocked in Wait on the
// supplied channel name. Idempotent + non-blocking — Notify on an
// empty channel is free.
//
// The publish path calls this AFTER the storage write commits, so
// any waiter that wakes is guaranteed to find at least one new row.
func (b *Bus) Notify(channel string) {
	b.notifyLocal(channel)
	// v0.12.3 Phase 4: cluster-mode fanout. Non-blocking — backplane
	// errors are logged but never fail Notify.
	b.mu.Lock()
	bp := b.bp
	b.mu.Unlock()
	if bp != nil {
		payload, _ := json.Marshal(channelBackplaneEvent{Channel: channel})
		timeout := b.bpPublishTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := bp.Publish(ctx, "loomcycle.channel", payload); err != nil {
			log.Printf("channels: backplane publish failed for %s: %v", channel, err)
		}
		cancel()
	}
}

// notifyLocal is the v0.12.3 fan-out-to-local-waiters half of Notify.
// Extracted so SubscribeBackplane can call it on remote events
// WITHOUT triggering another backplane publish.
func (b *Bus) notifyLocal(channel string) {
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

// SetBackplane installs the v0.12.3 Phase 4 cluster-mode fanout.
// Nil-safe: calling with nil disables fanout.
func (b *Bus) SetBackplane(bp coord.Backplane) {
	b.mu.Lock()
	b.bp = bp
	b.mu.Unlock()
}

// SubscribeBackplane starts a goroutine that listens for remote
// channel-notify events on `loomcycle.channel` and wakes local Wait
// callers via notifyLocal. Exits on ctx.Done.
func (b *Bus) SubscribeBackplane(ctx context.Context, bp coord.Backplane) error {
	ch, err := bp.Subscribe(ctx, "loomcycle.channel")
	if err != nil {
		return err
	}
	go func() {
		for evt := range ch {
			var p channelBackplaneEvent
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				log.Printf("channels: malformed backplane event: %v", err)
				continue
			}
			b.notifyLocal(p.Channel)
		}
	}()
	return nil
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
//
// **Race warning**: Wait registers its waker AFTER you call it. If
// you have a "check store → if empty, wait" pattern, there's a race
// window between the check returning empty and Wait registering the
// waker. A concurrent publish in that window fires Notify against an
// empty waiter slice — the notification is lost and the caller waits
// the full timeout. Use Register + Unregister instead for the
// race-free check-then-wait pattern (see Register's doc).
func (b *Bus) Wait(ctx context.Context, channel string, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	waker := b.Register(channel)
	defer b.Unregister(channel, waker)

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

// Register adds a waker for `channel` and returns it. The caller MUST
// call Unregister(channel, waker) when done — typically via defer —
// to prevent waker-slice leaks.
//
// Use this instead of Wait when you need to register the waker
// BEFORE doing an initial state check. The pattern closes the
// check-then-wait race that bare Wait has:
//
//	waker := bus.Register(channel)
//	defer bus.Unregister(channel, waker)
//	rows, _ := store.Read(...)
//	if len(rows) > 0 {
//	    return rows                    // synchronous fast path
//	}
//	select {
//	case <-waker:
//	    return store.Read(...)         // notify fired during wait
//	case <-time.After(timeout):
//	    return nil                     // timeout
//	case <-ctx.Done():
//	    return nil                     // cancelled
//	}
//
// Why this is race-free: by the time `store.Read` runs, the waker
// is already in the waiters slice. ANY publish that commits-and-
// notifies from this point on will either:
//
//  1. Race the read and win — Read returns the row (no wait needed)
//  2. Race the read and lose — Notify fires the waker, which is
//     already queued; the select wakes, Read returns the row
//
// The lost-notification case ("publish notified before subscriber
// registered") is structurally eliminated.
func (b *Bus) Register(channel string) chan struct{} {
	waker := make(chan struct{}, 1)
	b.mu.Lock()
	b.waiters[channel] = append(b.waiters[channel], waker)
	b.mu.Unlock()
	return waker
}

// Unregister removes the waker from the channel's waiter slice.
// Idempotent — safe to defer even if the waker already fired
// (Notify drained the slice; this finds nothing to remove and
// returns silently).
func (b *Bus) Unregister(channel string, waker chan struct{}) {
	b.removeWaiter(channel, waker)
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
