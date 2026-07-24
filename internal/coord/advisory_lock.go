package coord

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLock is the v0.12.4 Phase 5 singleton-sweeper coordinator.
// Wraps Postgres pg_try_advisory_lock so that across N replicas in a
// cluster, only one replica runs each sweeper tick — eliminating the
// N-times-per-tick log noise and concurrent DELETE pressure that
// every sweeper exhibited in Phases 1-4.
//
// Lock lifecycle (session-scoped):
//
//	pool.Acquire(ctx)   →  one *pgxpool.Conn pinned for the call
//	    pg_try_advisory_lock(key) → bool
//	    fn(ctx)                   ← runs only when lock is held
//	    pg_advisory_unlock(key)   ← always called via defer
//	conn.Release()       →  back to the pool; session stays alive
//
// CRITICAL: the lock is a session-level Postgres lock. It MUST stay
// on the same connection from acquire through unlock. Using pool.Exec
// for the lock + a separate pool.Exec for fn would let pgxpool hand
// you different connections, leaving the lock orphaned (released only
// on session close, which happens when the connection is closed —
// not when it returns to the pool). Always use the held *pgxpool.Conn.
//
// Crash safety: if the process crashes between TryLock and Unlock,
// the TCP connection closes and Postgres auto-releases the lock. No
// stuck-lock scenarios across replica crashes.
type AdvisoryLock struct {
	pool *pgxpool.Pool
}

// NewAdvisoryLock wraps a pgxpool for advisory-locked sweeper
// coordination. The pool is not owned; closing the lock does not
// close the pool.
func NewAdvisoryLock(pool *pgxpool.Pool) *AdvisoryLock {
	return &AdvisoryLock{pool: pool}
}

// TryRun attempts to acquire the named lock and runs fn under it.
// Returns:
//
//	(true,  nil)         → lock acquired, fn ran (fn's err if any is wrapped)
//	(false, nil)         → another replica holds the lock; silent skip
//	(_,     non-nil err) → infra failure (pool acquire, network); fn did not run
//
// Use the returned bool to discriminate the "expected lost race"
// case from the "broken infrastructure" case. Callers in main.go log
// only the err case; lost-race is silent (every other replica's
// every-tick would otherwise be noisy).
func (l *AdvisoryLock) TryRun(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("advisory lock: acquire conn: %w", err)
	}

	// pg_try_advisory_lock is non-blocking; cap the query itself to
	// 30s defensively in case the connection itself stalls.
	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var acquired bool
	if err := conn.QueryRow(lockCtx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&acquired); err != nil {
		conn.Release()
		return false, fmt.Errorf("advisory lock: pg_try_advisory_lock(%d): %w", lockKey, err)
	}
	if !acquired {
		conn.Release()
		return false, nil
	}

	// Register defers in LIFO order — Release fires LAST so the
	// unlock query has a live connection (review-1 finding #1: the
	// original `defer conn.Release()` was registered first, which
	// meant unlock ran AFTER Release; pgx v5 panics on QueryRow
	// against a released connection, and the lock leaked until the
	// connection's TCP session closed).
	defer conn.Release()
	defer func() {
		// Fresh context for the unlock so a cancelled outer ctx does
		// not leave the lock held. 5s upper bound — pg_advisory_unlock
		// is fast.
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		var unlocked bool
		if uerr := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockKey).Scan(&unlocked); uerr != nil {
			// Log but do not propagate — the connection close on
			// Release will also drop the lock.
			fmt.Printf("coord: advisory_unlock(%d): %v\n", lockKey, uerr)
		}
		_ = unlocked
	}()

	fnErr := fn(ctx)
	return true, fnErr
}

// Sweeper lock keys. Each value is FNV-1a 64-bit of the
// human-readable sweeper name. Computed in init() once at process
// start so the values are stable across builds (FNV-1a is
// deterministic) without needing a manual cut-and-paste constant.
//
// When adding a new sweeper: append a new var entry below + bump
// the var-block size in init(). Never reuse a key for a different
// sweeper.
var (
	LockKeyHeartbeatSweeper    int64
	LockKeyMemorySweeper       int64
	LockKeyChannelsSweeper     int64
	LockKeyInterruptsSweeper   int64
	LockKeyMetricsSweeper      int64
	LockKeyDynamicAgentSweeper int64
	LockKeyReplicasSweeper     int64
	// LockKeyResumePausedRuns gates the one-shot boot-time re-dispatch of
	// pause_state='paused' runs (F42 / RFC X Phase 2) so exactly ONE replica
	// resurrects each paused run's loop in a cluster — not a periodic sweep.
	LockKeyResumePausedRuns int64
	// LockKeyEphemeralVolumeSweeper gates the RFC AH Phase 2b crash-recovery
	// sweep of ephemeral volumes whose owning run is terminal (not paused), so
	// only one replica per tick runs the fenced os.RemoveAll.
	LockKeyEphemeralVolumeSweeper int64
	// LockKeyUsageSweeper gates the RFC AV Phase 2b token-usage
	// rollup-and-prune, so only one replica per tick folds old
	// token_usage rows into usage_archive and deletes them.
	LockKeyUsageSweeper int64
	// LockKeyRetentionSweeper gates the RFC BM data-retention sweep, so only
	// one replica per tick exports + purges retired-and-old substrate def
	// versions (agent/skill/team/mcp_server/schedule/a2a/webhook/memory_backend).
	LockKeyRetentionSweeper int64
	// LockKeyHelpIndexReconcile gates the one-shot boot-time reconcile of the
	// help-topic search index (RFC BL P1) so exactly ONE replica re-embeds the
	// changed/new help sections into the reserved global namespace — not a
	// periodic sweep. The go:embed help corpus is immutable in-process, so this
	// runs once per boot; the content-hash gate makes an unchanged corpus a
	// zero-embed no-op even on the replica that wins the lock.
	LockKeyHelpIndexReconcile int64
)

// MemoryConsolidatorLockKey derives the RFC BL P2 consolidation fan-out's
// advisory-lock key from the SCHEDULE DEF id, so only one replica per tick
// enumerates that schedule's targets and dispatches their runs.
//
// Distinct from the per-target lease the pass itself takes (Memory cursor_lease):
// the lease stops two passes fighting over one target, this stops N replicas each
// dispatching a full fan-out in the same tick — which would burn N× the tokens
// before the leases sorted it out.
//
// Per-def rather than one process-wide constant because consolidation schedules
// FAN OUT and an operator will have several — typically one per tenant. With a
// single shared key, two defs due in the same tick collide and the loser is
// skip-but-advanced, so it silently forfeits its whole cadence to whichever def
// the sweeper reached first. Keyed per def they are independent; the same def
// still admits exactly one replica.
func MemoryConsolidatorLockKey(defID string) int64 {
	return fnvKey("memory_consolidator:" + defID)
}

func init() {
	LockKeyHeartbeatSweeper = fnvKey("heartbeat_sweeper")
	LockKeyMemorySweeper = fnvKey("memory_sweeper")
	LockKeyChannelsSweeper = fnvKey("channels_sweeper")
	LockKeyInterruptsSweeper = fnvKey("interrupts_sweeper")
	LockKeyMetricsSweeper = fnvKey("metrics_sweeper")
	LockKeyDynamicAgentSweeper = fnvKey("dynamic_agent_sweeper")
	LockKeyReplicasSweeper = fnvKey("replicas_sweeper")
	LockKeyResumePausedRuns = fnvKey("resume_paused_runs")
	LockKeyEphemeralVolumeSweeper = fnvKey("ephemeral_volume_sweeper")
	LockKeyUsageSweeper = fnvKey("usage_sweeper")
	LockKeyRetentionSweeper = fnvKey("retention_sweeper")
	LockKeyHelpIndexReconcile = fnvKey("help_index_reconcile")
}

// fnvKey hashes a sweeper-name string to a stable int64 lock key.
// FNV-1a 64-bit; the high bit's sign is irrelevant for Postgres
// pg_try_advisory_lock which accepts any int8.
func fnvKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) //nolint:gosec // intentional uint64 → int64 reinterpretation
}
