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
	"log"
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

// userQuotaGate is the contract the v0.12.1 cluster-wide per-user
// counter must satisfy. coord.UserQuotaStore implements this implicitly;
// keeping the interface in this package (rather than importing coord)
// means concurrency stays a low-level dependency-free package and tests
// can stub the gate without standing up Postgres.
//
// TryAcquire returns (acquired, err). false+nil = the user is at cap;
// false+non-nil = infrastructure error (DB unreachable, etc).
type userQuotaGate interface {
	TryAcquire(ctx context.Context, userID string, cap int) (bool, error)
	Release(ctx context.Context, userID string)
	Snapshot(ctx context.Context) (map[string]int, error)
}

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
	//
	// In v0.12.1 cluster mode (quotaStore != nil), perUser is left
	// untouched — the DB-backed counter is authoritative. Single-
	// replica deployments (quotaStore == nil) use perUser exactly as
	// v0.10.1 did.
	perUser map[string]int
	waiters []chan struct{}

	// quotaStore is the v0.12.1 cluster-wide per-user counter. Nil in
	// single-replica mode (LOOMCYCLE_REPLICA_ID unset); set by
	// WithUserQuotaStore in cluster mode. When non-nil, AcquireForUser
	// delegates the per-user cap enforcement to the DB instead of the
	// in-memory perUser map. Set via WithUserQuotaStore.
	quotaStore userQuotaGate
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

// WithUserQuotaStore installs the v0.12.1 cluster-wide per-user
// counter. Pass *coord.UserQuotaStore here in cluster mode; pass nil
// (or skip the call) for single-replica mode. When set, AcquireForUser
// delegates the per-user cap check to the DB-backed counter; Stats()
// reads per-user breakdowns from the DB instead of the in-memory map.
//
// Idempotent on identical input; safe to call after the Semaphore is
// in use (the next AcquireForUser picks up the new gate). Existing
// in-flight slots accounted to the in-memory map stay there until they
// release — there is no retroactive migration of counts.
func (s *Semaphore) WithUserQuotaStore(qs userQuotaGate) *Semaphore {
	s.mu.Lock()
	s.quotaStore = qs
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
	// v0.12.1: snapshot the quotaStore reference (and cap) outside the
	// global-state mutex so the cluster-mode DB round-trip doesn't hold
	// s.mu. Capturing here means a concurrent WithUserQuotaStore call
	// can't swap modes mid-Acquire — the release/cancel paths use the
	// same captured qs. perUserActive is derived from this LOCKED snapshot,
	// not a second unlocked read of s.maxPerUser — WithPerUserCap writes it
	// under s.mu, so an unlocked read here is a data race (flagged by -race;
	// the cap setter is boot-time today but the API doc promises runtime-safe).
	s.mu.Lock()
	qs := s.quotaStore
	cap := s.maxPerUser
	s.mu.Unlock()

	perUserActive := cap > 0 && userID != ""

	// Cluster mode: acquire the per-user quota slot FIRST via the DB.
	// On any later rejection (global-queue full, timeout, cancel), we
	// compensate-Release so the cluster-wide count stays balanced.
	if perUserActive && qs != nil {
		ok, qerr := qs.TryAcquire(ctx, userID, cap)
		if qerr != nil {
			return nil, fmt.Errorf("user_quotas acquire: %w", qerr)
		}
		if !ok {
			return nil, &ErrPerUserQuotaExhausted{UserID: userID, Cap: cap}
		}
	}

	s.mu.Lock()
	// Single-replica mode keeps the v0.10.1 in-memory check. In cluster
	// mode the DB TryAcquire above already enforced the cap and we
	// skip this block — perUser stays untouched.
	if perUserActive && qs == nil {
		if s.perUser == nil {
			s.perUser = map[string]int{}
		}
		// Use the locked snapshot `cap` (not s.maxPerUser) — consistent with the
		// perUserActive gate above, and the error is built AFTER Unlock so a bare
		// s.maxPerUser read there would be a data race with WithPerUserCap.
		if s.perUser[userID] >= cap {
			s.mu.Unlock()
			return nil, &ErrPerUserQuotaExhausted{UserID: userID, Cap: cap}
		}
	}

	if s.active < s.maxConcurrent {
		s.active++
		if perUserActive && qs == nil {
			s.perUser[userID]++
		}
		s.mu.Unlock()
		return s.releaseFn(userID, qs), nil
	}
	if s.queued >= s.maxQueue {
		s.mu.Unlock()
		// Cluster mode: compensate-Release the DB slot we just took.
		if perUserActive && qs != nil {
			go releaseInBackground(qs, userID)
		}
		return nil, &BackpressureError{msg: "queue full"}
	}
	w := make(chan struct{}, 1)
	s.waiters = append(s.waiters, w)
	s.queued++
	if perUserActive && qs == nil {
		s.perUser[userID]++
	}
	s.mu.Unlock()

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case <-w:
		// Slot acquired. The releaseFn from the previous holder
		// already promoted us (queued--; active++). Our perUser count
		// (or DB slot) stays the same — increment happened at enqueue,
		// decrement happens at release.
		return s.releaseFn(userID, qs), nil
	case <-ctx.Done():
		s.cancelWaiter(w, userID, qs)
		return nil, ctx.Err()
	case <-timer.C:
		s.cancelWaiter(w, userID, qs)
		return nil, &BackpressureError{msg: "queue timeout"}
	}
}

// releaseInBackground decouples the caller from a slow DB Release.
// Used on the compensate path (queue full after TryAcquire) and on
// every release/cancel in cluster mode. The defer release() pattern
// in handler code expects fast return; the Release goroutine carries
// its own bounded context so a permanently-down DB can't leak
// goroutines indefinitely.
func releaseInBackground(qs userQuotaGate, userID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qs.Release(ctx, userID)
}

// Stats returns a snapshot for /metrics or admin views. The PerUser
// map is a defensive copy — callers can iterate without holding s.mu.
// The map is nil when no per-user accounting has been done yet
// (maxPerUser==0 always, OR maxPerUser>0 but no users have hit the
// substrate).
func (s *Semaphore) Stats() Stats {
	s.mu.Lock()
	qs := s.quotaStore
	out := Stats{Active: s.active, Queued: s.queued}
	// In-memory perUser is only authoritative in single-replica mode.
	// Cluster mode reads from the DB below (after releasing s.mu so
	// the round-trip doesn't block Acquire callers).
	if qs == nil && len(s.perUser) > 0 {
		out.PerUser = make(map[string]int, len(s.perUser))
		for k, v := range s.perUser {
			out.PerUser[k] = v
		}
	}
	s.mu.Unlock()

	if qs != nil {
		// 1s timeout: the admin /v1/_concurrency/stats endpoint hits
		// this; a permanently-down DB shouldn't stall the operator's
		// dashboard. On error we log and return Stats with PerUser=nil
		// (active+queued remain accurate; the DB hiccup is observable).
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		perUser, err := qs.Snapshot(ctx)
		if err != nil {
			log.Printf("concurrency: quota snapshot failed: %v", err)
		} else {
			out.PerUser = perUser
		}
	}
	return out
}

// releaseFn captures the qs reference at acquire time so a later
// WithUserQuotaStore swap can't make a still-in-flight slot decrement
// against the wrong gate. qs == nil means we used the in-memory path;
// non-nil means the DB-backed Release fires in a goroutine.
func (s *Semaphore) releaseFn(userID string, qs userQuotaGate) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.active--
			if qs == nil {
				s.decrementPerUser(userID)
			}
			// Wake the next waiter if any. NOTE: the woken waiter's
			// perUser count (or DB slot) stays as-is — its increment
			// happened at enqueue, its decrement happens when ITS
			// release fires.
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
			s.mu.Unlock()
			// Cluster mode: DB Release after the mutex is released so
			// the network round-trip doesn't block other Acquire
			// callers. Background goroutine with a 5s timeout for safety.
			if qs != nil && userID != "" {
				go releaseInBackground(qs, userID)
			}
		})
	}
}

func (s *Semaphore) cancelWaiter(target chan struct{}, userID string, qs userQuotaGate) {
	s.mu.Lock()
	needRelease := false
	for i, w := range s.waiters {
		if w == target {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.queued--
			if qs == nil {
				s.decrementPerUser(userID)
			} else {
				needRelease = true
			}
			s.mu.Unlock()
			if needRelease && userID != "" {
				go releaseInBackground(qs, userID)
			}
			return
		}
	}
	// Not found: releaseFn already won the race and transferred a slot to
	// us. The buffered chan holds a stranded token; drain it and decrement
	// active to give the slot back, otherwise it's accounted to a goroutine
	// that returned with ctx.Err() and never released. perUser (or the
	// DB slot in cluster mode) is decremented in the same arm so the
	// accounting stays balanced.
	//
	// The `default:` arm is unreachable under the current invariants
	// (target is a buffered-1 chan with releaseFn as its only sender,
	// and releaseFn holds s.mu while sending — by the time we reach
	// this select with s.mu held, releaseFn has already either
	// (a) appeared in s.waiters and the loop above found it, or
	// (b) removed target from waiters AND placed the token in the
	// buffer, so the case <-target arm will fire). The panic is the
	// CLAUDE.md "intentional panic('unreachable') site" pattern — a
	// future refactor that changes the buffer size or introduces a
	// second sender turns a silent active-counter leak into an
	// immediate, debuggable crash.
	select {
	case <-target:
		s.active--
		if qs == nil {
			s.decrementPerUser(userID)
		} else {
			needRelease = true
		}
		s.mu.Unlock()
		if needRelease && userID != "" {
			go releaseInBackground(qs, userID)
		}
	default:
		s.mu.Unlock()
		panic("concurrency: cancelWaiter stranded-token default arm fired — invariant violation (buffered-1 chan, single sender)")
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
