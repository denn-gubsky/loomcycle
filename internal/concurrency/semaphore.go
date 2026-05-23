// Package concurrency caps simultaneous agent runs and queues bursts.
//
// Same shape as jobs-search-agent's concurrency.ts: a counting semaphore plus
// a bounded FIFO queue with a per-acquire timeout. Excess returns BackpressureError.
//
// v0.10.1 — per-tenant fairness. AcquireForUser(ctx, userID) enforces a
// per-user cap on (active + queued) runs. When the cap is exceeded the
// caller gets ErrPerUserQuotaExhausted (mapped to HTTP 429 +
// Retry-After: 5). When maxPerUser==0 or userID=="", the per-user check
// is skipped — back-compat for the existing Acquire path and for
// anonymous / system-initiated calls.
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// BackpressureError signals the queue is full; the caller should HTTP 429.
type BackpressureError struct{ msg string }

func (e *BackpressureError) Error() string { return e.msg }

// Code is the typed identifier used in the HTTP error envelope so
// adapter consumers can branch retry strategies on the wire shape.
func (e *BackpressureError) Code() string { return "backpressure" }

// ErrPerUserQuotaExhausted signals that the user's per-tenant cap was
// hit. The caller should surface HTTP 429 with `Retry-After: 5` and
// `code: "per_user_quota_exhausted"` in the JSON body. Distinct from
// BackpressureError (queue full) because the appropriate retry
// strategy differs — backpressure is operator-wide load, per-user
// quota is "you specifically need to wait."
type ErrPerUserQuotaExhausted struct {
	UserID string
	Cap    int
}

func (e *ErrPerUserQuotaExhausted) Error() string {
	return fmt.Sprintf("per-user quota exhausted: user=%s cap=%d", e.UserID, e.Cap)
}

// Code is the typed identifier used in the HTTP error envelope.
func (e *ErrPerUserQuotaExhausted) Code() string { return "per_user_quota_exhausted" }

// Semaphore caps concurrent acquisitions. Acquire blocks (with timeout) when
// slots are full and the queue has room; returns BackpressureError when the
// queue is also full.
type Semaphore struct {
	maxConcurrent int
	maxQueue      int
	maxPerUser    int // 0 = disabled (no per-user check)
	timeout       time.Duration

	mu     sync.Mutex
	active int
	queued int
	// perUser tracks active+queued runs per non-empty user_id.
	// Both Acquire (with empty userID) and AcquireForUser write to
	// the same global active/queued counters; only AcquireForUser
	// with a non-empty userID touches this map.
	perUser map[string]int
	waiters []chan struct{}
}

// Stats is a point-in-time snapshot of the semaphore's accounting.
// Returned by Stats() so future fields can be added without churning
// the call signature.
type Stats struct {
	Active  int
	Queued  int
	PerUser map[string]int
}

// New constructs a Semaphore. timeout is the max wait per acquire.
// Per-tenant fairness is off by default; opt in via WithPerUserCap.
// This keeps the constructor signature stable across the 58 existing
// callers (mostly tests) — only main.go needs to chain the new setter.
func New(maxConcurrent, maxQueue int, timeout time.Duration) *Semaphore {
	return &Semaphore{
		maxConcurrent: maxConcurrent,
		maxQueue:      maxQueue,
		timeout:       timeout,
	}
}

// WithPerUserCap configures the per-tenant fairness limit. n=0 (the
// default) disables the check entirely — global-quota behavior is
// unchanged. n>0 caps each non-empty user_id at n active+queued runs;
// excess Acquire calls return *ErrPerUserQuotaExhausted.
//
// Fluent: `concurrency.New(...).WithPerUserCap(4)`. Safe to call after
// the Semaphore is in use; the new cap applies to subsequent Acquire
// calls. In-flight counts are NOT retroactively trimmed (a user at 6
// in-flight when the cap drops to 4 stays at 6 until releases unwind
// the count below the new cap).
func (s *Semaphore) WithPerUserCap(n int) *Semaphore {
	s.mu.Lock()
	s.maxPerUser = n
	s.mu.Unlock()
	return s
}

// Acquire returns a release func. Call release exactly once when done.
// Returns ctx.Err() if ctx cancels first; *BackpressureError if queue is full.
// Semantically equivalent to AcquireForUser(ctx, "") — i.e. no per-user
// accounting. Existing call sites that don't care about per-tenant
// fairness keep using this entry point.
func (s *Semaphore) Acquire(ctx context.Context) (release func(), err error) {
	return s.AcquireForUser(ctx, "")
}

// AcquireForUser is Acquire with a per-user cap layered on top. When
// maxPerUser==0 or userID=="", the per-user check is skipped and the
// call is semantically identical to Acquire.
//
// The per-user count tracks active+queued — a user at cap can't enqueue
// more runs even when the global queue has slack, otherwise a noisy
// user could fill the queue with their own runs and starve everyone
// else for the queue's lifetime.
//
// On per-user-cap rejection, returns *ErrPerUserQuotaExhausted (HTTP
// 429 + Retry-After: 5). On global-queue rejection or timeout,
// returns *BackpressureError (HTTP 429). On ctx cancel, returns
// ctx.Err().
func (s *Semaphore) AcquireForUser(ctx context.Context, userID string) (release func(), err error) {
	perUserActive := s.maxPerUser > 0 && userID != ""

	s.mu.Lock()
	if perUserActive {
		if s.perUser == nil {
			s.perUser = map[string]int{}
		}
		if s.perUser[userID] >= s.maxPerUser {
			s.mu.Unlock()
			return nil, &ErrPerUserQuotaExhausted{UserID: userID, Cap: s.maxPerUser}
		}
	}

	if s.active < s.maxConcurrent {
		s.active++
		if perUserActive {
			s.perUser[userID]++
		}
		s.mu.Unlock()
		return s.releaseFn(userID), nil
	}
	if s.queued >= s.maxQueue {
		s.mu.Unlock()
		return nil, &BackpressureError{msg: "queue full"}
	}
	w := make(chan struct{}, 1)
	s.waiters = append(s.waiters, w)
	s.queued++
	if perUserActive {
		s.perUser[userID]++
	}
	s.mu.Unlock()

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case <-w:
		// Slot acquired. The releaseFn from the previous holder
		// already promoted us (queued--; active++). Our perUser count
		// stays the same — increment happened at enqueue, decrement
		// happens at release.
		return s.releaseFn(userID), nil
	case <-ctx.Done():
		s.cancelWaiter(w, userID)
		return nil, ctx.Err()
	case <-timer.C:
		s.cancelWaiter(w, userID)
		return nil, &BackpressureError{msg: "queue timeout"}
	}
}

// Stats returns a snapshot for /metrics or admin views. The PerUser
// map is a defensive copy — callers can iterate without holding s.mu.
// The map is nil when no per-user accounting has been done yet
// (maxPerUser==0 always, OR maxPerUser>0 but no users have hit the
// substrate).
func (s *Semaphore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{Active: s.active, Queued: s.queued}
	if len(s.perUser) > 0 {
		out.PerUser = make(map[string]int, len(s.perUser))
		for k, v := range s.perUser {
			out.PerUser[k] = v
		}
	}
	return out
}

func (s *Semaphore) releaseFn(userID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			s.decrementPerUser(userID)
			// Wake the next waiter if any. NOTE: the woken waiter's
			// perUser count stays as-is — its increment happened at
			// enqueue, its decrement happens when ITS release fires.
			if len(s.waiters) > 0 {
				w := s.waiters[0]
				s.waiters = s.waiters[1:]
				s.queued--
				s.active++
				select {
				case w <- struct{}{}:
				default:
				}
			}
		})
	}
}

func (s *Semaphore) cancelWaiter(target chan struct{}, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.waiters {
		if w == target {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.queued--
			s.decrementPerUser(userID)
			return
		}
	}
	// Not found: releaseFn already won the race and transferred a slot to
	// us. The buffered chan holds a stranded token; drain it and decrement
	// active to give the slot back, otherwise it's accounted to a goroutine
	// that returned with ctx.Err() and never released. perUser is
	// decremented in the same arm so the accounting stays balanced.
	select {
	case <-target:
		s.active--
		s.decrementPerUser(userID)
	default:
	}
}

// decrementPerUser is the single place perUser counts decrement. No-op
// when userID is empty OR the map hasn't been initialized (back-compat
// with Acquire's empty-userID path). Removes the entry when the count
// hits zero so Stats().PerUser doesn't accumulate stale entries for
// users no longer in flight.
func (s *Semaphore) decrementPerUser(userID string) {
	if userID == "" || s.perUser == nil {
		return
	}
	s.perUser[userID]--
	if s.perUser[userID] <= 0 {
		delete(s.perUser, userID)
	}
}

// IsBackpressure reports whether err is a BackpressureError.
func IsBackpressure(err error) bool {
	var b *BackpressureError
	return errors.As(err, &b)
}

// IsPerUserQuotaExhausted reports whether err is an
// *ErrPerUserQuotaExhausted. Mirrors IsBackpressure.
func IsPerUserQuotaExhausted(err error) bool {
	var e *ErrPerUserQuotaExhausted
	return errors.As(err, &e)
}
