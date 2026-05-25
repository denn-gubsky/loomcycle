package runner

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgSessionLocker is the v0.12.5 Phase 6 cluster-wide session-lock
// implementation. Replaces SessionLockMap's in-process mutex with a
// Postgres session-scoped advisory lock keyed by hash(session_id),
// so concurrent continuations on the same session_id across replicas
// fail with the same ErrSessionBusy 409 semantics.
//
// Single-replica deployments don't use this — they keep
// SessionLockMap (zero behavior change).
//
// Why session-scoped (NOT transaction-scoped): the session lock is
// held for the entire run, which can be minutes. A transaction-scoped
// lock would require holding an open Postgres transaction for that
// duration, blocking autovacuum + exploding connection use under
// load. Session-scoped advisory locks bind to the *pgxpool.Conn we
// borrow — we pin the connection until the run completes.
//
// Pool budget: every active session-locked run holds one connection
// from the pgxpool. Operators sizing their pool should ensure
// MaxConcurrentRuns + headroom < pool.MaxConns (default 32). The
// global concurrency Semaphore caps MaxConcurrentRuns to ~32 by
// default, so the budget fits comfortably.
//
// Crash safety: if the loomcycle process crashes, the TCP connection
// closes and Postgres auto-releases the lock. No stuck-lock scenarios
// across replica crashes.
type PgSessionLocker struct {
	pool *pgxpool.Pool
}

// NewPgSessionLocker wraps a pgxpool. Caller owns pool lifetime;
// closing the locker does not close the pool.
func NewPgSessionLocker(pool *pgxpool.Pool) *PgSessionLocker {
	return &PgSessionLocker{pool: pool}
}

// TryLock attempts to acquire the per-session advisory lock for id.
// Returns (release, true) on success; (nil, false) when another
// session (this or any other replica) already holds it. Matches the
// existing SessionLockMap.TryLock signature so the dispatch in
// Server.trySessionLock is identical.
//
// On success the returned release closure issues
// pg_advisory_unlock + returns the connection to the pool. The
// release closure uses a fresh background context (NOT the caller's
// ctx) so a cancelled HTTP request still releases the lock cleanly.
func (l *PgSessionLocker) TryLock(ctx context.Context, id string) (release func(), ok bool) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		log.Printf("session_lock: pool acquire: %v", err)
		return nil, false
	}

	// pg_try_advisory_lock is non-blocking. Use hashtextextended(id, 0)
	// to derive a bigint key from the variable-length session_id — the
	// same hash function the existing per-name advisory locks use
	// (memory increment, agent_defs, skill_defs).
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, id,
	).Scan(&acquired); err != nil {
		conn.Release()
		log.Printf("session_lock: pg_try_advisory_lock(%s): %v", id, err)
		return nil, false
	}
	if !acquired {
		conn.Release()
		return nil, false
	}

	return func() {
		// Fresh ctx — caller's ctx is likely cancelled at this point
		// (HTTP request ended). 3s upper bound; pg_advisory_unlock is
		// fast on the local conn.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx,
			`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, id,
		).Scan(&unlocked); err != nil {
			// Connection close will auto-release the lock; log and
			// continue.
			log.Printf("session_lock: pg_advisory_unlock(%s): %v", id, err)
		}
		if !unlocked && err == nil {
			// pg_advisory_unlock returns false if THIS session never
			// held the lock — should never happen because we hold the
			// pinned conn from acquire to release. Log defensively.
			log.Printf("session_lock: pg_advisory_unlock(%s) returned false (session did not hold lock)", id)
		}
		conn.Release()
	}, true
}

// dummyErr is unused but the compiler complains about err in the
// log.Printf when not pre-declared. Removed via err-shadow in the
// closure above.
var _ = fmt.Sprintf
