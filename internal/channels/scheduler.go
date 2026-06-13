package channels

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Scheduler tracks pending wake-ups for deferred channel messages
// (v0.8.6 system-channels feature). When the tool layer accepts a
// publish with `deliver_at > now`, the message lands in storage
// immediately but is hidden from reads until visible_at. The
// scheduler arms a single-shot timer that calls Bus.Notify(channel)
// when visible_at arrives, waking any long-poll subscribers blocked
// on the channel.
//
// Without the scheduler, deferred messages still get delivered —
// subscribers see them on their next periodic wake (whatever the
// operator's wait_ms cap is). The scheduler is a latency optimisation,
// not a correctness mechanism. If the process restarts before a
// timer fires, the storage row stays put; the scheduler's optional
// Bootstrap pass on next boot reschedules pending timers.
//
// Concurrency: Schedule is safe to call from any goroutine. Internal
// state is a sync.Map keyed by msg_id; an atomic counter caps total
// pending timers per process to MaxPending (default 10000, override
// via LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED). Over-cap schedules
// silently degrade to "subscribers wake on their periodic poll
// instead" — same behaviour as if the scheduler weren't wired at all.
type Scheduler struct {
	bus *Bus

	// MaxPending caps live timers; zero = no cap (every deferred
	// publish gets a timer). Operators set this to bound memory + the
	// time.AfterFunc heap; the cap is a defense against runaway
	// deferred publishes (e.g. a buggy agent that schedules a million
	// alarms for the same minute).
	MaxPending int

	timers  sync.Map // msg_id (string) → *time.Timer
	pendCnt atomic.Int64
}

// NewScheduler constructs a scheduler bound to the given Bus.
// maxPending: hard cap on live timers; <=0 disables the cap.
func NewScheduler(bus *Bus, maxPending int) *Scheduler {
	return &Scheduler{bus: bus, MaxPending: maxPending}
}

// Schedule arms a timer that calls bus.Notify(channel) at visibleAt.
// msgID is the identifier of the deferred message, used as the timer
// registry key (so Cancel(msgID) can pull it out before firing). If
// visibleAt is in the past, fires Notify immediately (synchronously)
// — the caller may want this for symmetry, though the publish path
// usually skips Schedule when visible_at <= now.
//
// Returns true if a timer was armed; false when:
//   - The MaxPending cap is hit. The caller's choice: the message is
//     still in storage, just no in-process wake-up. Subscribers will
//     see it on their next periodic poll.
//   - visibleAt is non-positive (zero or before now); Notify already
//     fired synchronously, no timer needed.
//   - msgID already has a registered timer (idempotent on re-Schedule).
func (s *Scheduler) Schedule(channel, msgID string, visibleAt time.Time) bool {
	delay := time.Until(visibleAt)
	if delay <= 0 {
		s.bus.Notify(channel)
		return false
	}
	if msgID == "" {
		return false
	}
	if s.MaxPending > 0 && s.pendCnt.Load() >= int64(s.MaxPending) {
		return false // silent fallback to periodic-poll delivery
	}

	// Claim the registry slot BEFORE arming the timer. Arming first (the old
	// code did `t := AfterFunc(...)` then `LoadOrStore(msgID, t)`) opened a
	// window where, for a sub-millisecond delay, the fire closure could run
	// between AfterFunc and the store: its Delete was a no-op (the entry
	// didn't exist yet) and the subsequent store then parked an
	// already-fired timer in the map — an orphaned entry that nothing ever
	// removed (exp7 C1, narrow + low-severity). Reserve a nil placeholder via
	// LoadOrStore; a loaded result means the msgID is already scheduled
	// (idempotent re-Schedule on bootstrap).
	if _, loaded := s.timers.LoadOrStore(msgID, (*time.Timer)(nil)); loaded {
		return false
	}
	// pendCnt is incremented under the claim. Invariant: whoever removes the
	// slot (the fire closure or Cancel, via LoadAndDelete) decrements exactly
	// once — so the count can't drift even if they race.
	s.pendCnt.Add(1)

	t := time.AfterFunc(delay, func() {
		// Order matters: do the bookkeeping BEFORE calling Notify. Notify
		// hands off to the waiting goroutine synchronously (bus.Wait returns
		// true the moment Notify lands). If we notified first, an observer
		// reading PendingCount() right after their Wait returned could see a
		// stale count because the LoadAndDelete + Add(-1) below hadn't
		// completed — the race detector exposed this as a flaky
		// TestScheduler_FiresAtVisibleAt. With this order, PendingCount is
		// post-fire by the time any subscriber observes Notify.
		if _, ok := s.timers.LoadAndDelete(msgID); ok {
			s.pendCnt.Add(-1)
		}
		s.bus.Notify(channel)
	})
	// Swap the placeholder for the armed timer — but only if the closure
	// hasn't already fired and removed the slot. A failed CAS means the timer
	// already fired (and cleaned up via LoadAndDelete), so there is nothing to
	// store and Stop is a harmless no-op; either way no orphan is left behind.
	if !s.timers.CompareAndSwap(msgID, (*time.Timer)(nil), t) {
		t.Stop()
	}
	return true
}

// Cancel stops a pending timer if one is registered for msgID. Safe
// to call on an unknown msgID (no-op). Useful when a deferred message
// is deleted before its visible_at — saves a wasted Bus.Notify when
// nobody will find the row.
func (s *Scheduler) Cancel(msgID string) {
	v, loaded := s.timers.LoadAndDelete(msgID)
	if !loaded {
		return
	}
	// Remover-decrements invariant: we removed a live slot, so we own the
	// decrement. The racing fire closure's LoadAndDelete now returns ok=false,
	// so it won't double-decrement. v may be the nil placeholder (Cancel
	// raced a Schedule between its slot-claim and the CompareAndSwap); guard
	// the nil so Stop isn't called on a nil *time.Timer.
	s.pendCnt.Add(-1)
	if t, ok := v.(*time.Timer); ok && t != nil {
		t.Stop()
	}
}

// PendingCount reports the number of armed timers. Exposed for tests
// and operator introspection.
func (s *Scheduler) PendingCount() int {
	return int(s.pendCnt.Load())
}

// Bootstrap rescans the store for messages with future visible_at and
// reschedules their timers. Called at boot to recover deferred
// publishes that were pending when the previous process exited.
//
// To keep this package free of an import cycle on `store`, the
// caller supplies a callback that yields (channel, msg_id, visible_at)
// tuples — the loomcycle main wires this against a store query that
// matches:
//
//	SELECT channel, id, visible_at FROM channel_messages
//	WHERE visible_at > NOW()
//	  AND (expires_at IS NULL OR expires_at > NOW())
//	ORDER BY visible_at ASC
//	LIMIT max_pending
//
// Errors from the callback abort the bootstrap; partial state is OK
// (unrescheduled rows still get delivered via subscriber periodic poll).
func (s *Scheduler) Bootstrap(ctx context.Context, scan func(yield func(channel, msgID string, visibleAt time.Time)) error) error {
	yield := func(channel, msgID string, visibleAt time.Time) {
		s.Schedule(channel, msgID, visibleAt)
	}
	return scan(yield)
}
