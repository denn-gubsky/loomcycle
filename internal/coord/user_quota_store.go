package coord

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UserQuotaStore is the v0.12.1 cluster-wide per-user concurrency
// counter, backed by the user_quotas Postgres table. Replaces the
// in-process counter that v0.10.1's concurrency.Semaphore used in
// single-replica mode — the in-process path stays unchanged for
// deployments without LOOMCYCLE_REPLICA_ID set.
//
// Lives in the coord package next to ReplicaStore because:
//   - both are Postgres-only by contract (SQLite refuses cluster mode)
//   - both need a pgxpool directly (not the store.Store interface)
//   - both are wired in main.go's `if cfg.Env.ReplicaID != ""` block
//
// Concurrency contract: every method is safe for concurrent use by
// multiple goroutines. The atomic UPDATE pattern in TryAcquire makes
// the cap check race-free across replicas — two replicas both reading
// active_count = cap-1 will both INSERT-or-UPDATE atomically; only one
// will affect a row.
//
// Crash-safety gap (until Phase 5): if a replica acquires a slot via
// TryAcquire and crashes before Release fires, the user's active_count
// is permanently incremented until a human operator runs:
//
//	UPDATE user_quotas SET active_count = active_count - N
//	WHERE user_id = '<user>';
//
// or restarts loomcycle (which does NOT auto-reconcile the DB counter).
// Phase 5's replicas TTL sweeper will fix this by joining user_quotas
// against runs WHERE replica_id = dead_replica AND status IN
// ('active', 'queued') to compute the leaked count and emit
// compensating UPDATEs. Until then, operators monitor
// GET /v1/_concurrency/stats for stuck non-zero counts after all known
// runs have completed.
type UserQuotaStore struct {
	pool *pgxpool.Pool
}

// NewUserQuotaStore wraps an existing pgxpool. The pool is not owned;
// closing UserQuotaStore does not close the pool (matches ReplicaStore).
func NewUserQuotaStore(pool *pgxpool.Pool) *UserQuotaStore {
	return &UserQuotaStore{pool: pool}
}

// TryAcquire attempts to increment user_quotas[userID].active_count
// atomically, bounded by cap. Returns:
//
//	(true, nil)  → slot acquired (row created at count=1 or
//	                incremented past the prior count)
//	(false, nil) → user is at cap; caller should map to the
//	                ErrPerUserQuotaExhausted shape on its side
//	(_, err)     → infrastructure error (network, constraint violation)
//
// The two-arm signature (bool + error) lets the concurrency package
// keep its own ErrPerUserQuotaExhausted error type without forcing
// concurrency → coord import direction. cap must be > 0; cap == 0 is
// the caller's "per-user check disabled" sentinel and TryAcquire must
// not be reached in that case.
//
// Pattern: single-statement INSERT ... ON CONFLICT DO UPDATE WHERE
// active_count < cap. Avoids the obvious two-step UPDATE→INSERT race.
// The INSERT arm always fires for a new user_id (initial count = 1);
// the UPDATE arm only fires when the existing count is strictly less
// than cap. rows_affected discriminates the at-cap case.
func (s *UserQuotaStore) TryAcquire(ctx context.Context, userID string, cap int) (bool, error) {
	if userID == "" {
		return false, errors.New("user_id is empty")
	}
	if cap <= 0 {
		return false, fmt.Errorf("cap must be > 0 (got %d)", cap)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO user_quotas (user_id, active_count, updated_at)
		VALUES ($1, 1, now())
		ON CONFLICT (user_id) DO UPDATE
		  SET active_count = user_quotas.active_count + 1,
		      updated_at   = now()
		  WHERE user_quotas.active_count < $2
	`, userID, cap)
	if err != nil {
		return false, fmt.Errorf("user_quotas upsert %s: %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Release decrements user_quotas[userID].active_count. Guarded with
// `WHERE active_count > 0` so a double-release (or a Phase 5 reap
// landing between our acquire and release) does not underflow into
// the CHECK constraint. rows_affected = 0 is logged as a warning —
// it means a slot was released that we didn't think we held, which is
// worth knowing during debugging but not a hard error.
//
// The row is NOT deleted when count reaches zero — that's a deliberate
// choice to avoid the INSERT-on-next-acquire churn for active users.
// The row with count=0 is a tombstone that costs one Postgres row and
// is harmless. Phase 5's sweeper can DELETE WHERE active_count = 0 if
// row count ever becomes a concern.
func (s *UserQuotaStore) Release(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_quotas
		   SET active_count = active_count - 1,
		       updated_at   = now()
		 WHERE user_id = $1 AND active_count > 0
	`, userID)
	if err != nil {
		log.Printf("coord: user_quotas release %s: %v", userID, err)
		return
	}
	if tag.RowsAffected() == 0 {
		log.Printf("coord: user_quotas release %s: no-op (already at 0 — double-release or Phase 5 reap)", userID)
	}
}

// Snapshot returns the per-user active count map for users with
// at least one active run. Backs GET /v1/_concurrency/stats's
// per_user field. Returns nil map when no users have active runs
// (matches the v0.10.1 in-process Stats() behavior so the
// omitempty JSON marshal stays consistent across modes).
func (s *UserQuotaStore) Snapshot(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, active_count
		  FROM user_quotas
		 WHERE active_count > 0
	`)
	if err != nil {
		return nil, fmt.Errorf("user_quotas snapshot: %w", err)
	}
	defer rows.Close()
	var out map[string]int
	for rows.Next() {
		var (
			userID string
			count  int
		)
		if err := rows.Scan(&userID, &count); err != nil {
			return nil, fmt.Errorf("scan user_quotas row: %w", err)
		}
		if out == nil {
			out = make(map[string]int)
		}
		out[userID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user_quotas: %w", err)
	}
	return out, nil
}
