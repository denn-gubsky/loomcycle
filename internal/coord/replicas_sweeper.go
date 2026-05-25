package coord

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReplicasSweeper closes the v0.12.1 + v0.12.2 crash-safety gaps by
// reaping replicas whose heartbeat has stopped. For each dead replica
// it:
//
//  1. Marks every runs row with replica_id = <dead> AND status = 'running'
//     as failed with error='owner replica died'. This finalises Phase 3
//     cross-replica cancel's "owner dead" handling without waiting for
//     a cancel request — operators see correct status on /v1/agents/{id}
//     and /v1/users/{user_id}/agents within one sweep tick.
//
//  2. Decrements user_quotas for every user who had a run on the dead
//     replica. Closes Phase 2's documented crash-safety gap: a replica
//     that crashed mid-run left its TryAcquire slot permanently
//     incremented. GREATEST(0, ...) prevents underflow when the slot
//     was already released by some other path.
//
//  3. DELETEs the replicas row so the next tick doesn't re-process.
//
// Runs only in cluster mode (started inside main.go's
// `if cfg.Env.ReplicaID != ""` block). Single-replica deployments
// don't need this — there is no "other replica" to die.
//
// Race with the existing heartbeat sweeper: the heartbeat sweeper
// marks stale runs failed by timeout (error='heartbeat timeout').
// Our quota-reap query filters on error='owner replica died' so we
// only decrement quotas for runs WE just marked. If the heartbeat
// sweeper raced ahead, the affected user's slot leaks until the
// operator manually reconciles via /v1/_concurrency/stats. This
// residual race is documented in the Phase 5 RFC; the window is
// millisecond-wide and the consequence is bounded (one slot per
// affected user per dead-replica event).
type ReplicasSweeper struct {
	pool       *pgxpool.Pool
	interval   time.Duration
	staleAfter time.Duration
	lock       *AdvisoryLock
	logf       func(format string, args ...any)
}

// ReplicasSweeperConfig is the constructor input.
type ReplicasSweeperConfig struct {
	// Interval between sweeps. Default 60s.
	Interval time.Duration

	// StaleAfter is the cutoff: replicas with last_heartbeat_at
	// older than now() - StaleAfter are reaped. Default 90s = 3×
	// the 30s heartbeat interval. Keep the 3× relationship if the
	// heartbeat interval is ever made configurable.
	StaleAfter time.Duration

	// Lock is the optional cluster-wide singleton gate. Without it,
	// every replica's sweeper goroutine would run on every tick;
	// idempotent SQL keeps that correct but noisy. With it, only
	// the lock-holder sweeps per tick.
	Lock *AdvisoryLock

	// Logger. Defaults to log.Printf.
	Logger func(format string, args ...any)
}

const (
	defaultReplicasSweepInterval = 60 * time.Second
	defaultReplicasStaleAfter    = 90 * time.Second
)

// NewReplicasSweeper constructs a ReplicasSweeper. Pool is required.
// Other fields have sensible defaults.
func NewReplicasSweeper(pool *pgxpool.Pool, cfg ReplicasSweeperConfig) *ReplicasSweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultReplicasSweepInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaultReplicasStaleAfter
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Printf
	}
	return &ReplicasSweeper{
		pool:       pool,
		interval:   cfg.Interval,
		staleAfter: cfg.StaleAfter,
		lock:       cfg.Lock,
		logf:       cfg.Logger,
	}
}

// Run drives the sweep ticker. Exits on ctx.Done.
//
// When Lock is set, each tick runs only on the replica that wins
// the advisory lock for that tick. When Lock is nil (test mode or
// single-replica), runs unconditionally.
func (s *ReplicasSweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *ReplicasSweeper) tick(ctx context.Context) {
	if s.lock == nil {
		if _, err := s.sweepOnce(ctx); err != nil {
			s.logf("coord: replicas sweep: %v", err)
		}
		return
	}
	acquired, err := s.lock.TryRun(ctx, LockKeyReplicasSweeper, func(ctx context.Context) error {
		_, swerr := s.sweepOnce(ctx)
		return swerr
	})
	if err != nil {
		s.logf("coord: replicas sweep: advisory lock: %v", err)
	}
	_ = acquired // silent on lost race
}

// sweepOnce runs one full reap pass. Returns the number of replica
// rows DELETEd. Test helper that bypasses the ticker.
func (s *ReplicasSweeper) sweepOnce(ctx context.Context) (int, error) {
	// 30s upper bound so a stuck sweep doesn't pin a connection.
	swctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Step 1: identify stale replicas.
	cutoff := time.Now().Add(-s.staleAfter)
	rows, err := s.pool.Query(swctx, `
		SELECT id FROM replicas WHERE last_heartbeat_at < $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("query stale replicas: %w", err)
	}
	staleIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stale replica id: %w", err)
		}
		staleIDs = append(staleIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate stale replicas: %w", err)
	}
	if len(staleIDs) == 0 {
		return 0, nil
	}

	reaped := 0
	for _, id := range staleIDs {
		if err := s.reapReplica(swctx, id); err != nil {
			// Continue on per-replica failure — partial success is
			// better than full abort. Next tick will retry the row.
			s.logf("coord: replicas sweep: reap %s: %v", id, err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

// reapReplica processes one dead replica row: mark runs failed,
// decrement user_quotas, delete the row.
func (s *ReplicasSweeper) reapReplica(ctx context.Context, replicaID string) error {
	// Step 2a: mark owned runs failed. Phase 3 gap closure.
	markRes, err := s.pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'failed',
		       completed_at = now(),
		       error = 'owner replica died',
		       stop_reason = 'replica_died'
		 WHERE replica_id = $1
		   AND status = 'running'
	`, replicaID)
	if err != nil {
		return fmt.Errorf("mark runs failed: %w", err)
	}
	markedRuns := markRes.RowsAffected()
	if markedRuns > 0 {
		s.logf("coord: replicas sweep: replica %s — marked %d run(s) failed", replicaID, markedRuns)
	}

	// Skip the quota reap when step 2a affected 0 rows. Why: if a
	// previous tick succeeded at steps 2a+2b but FAILED at step 2c
	// (DELETE FROM replicas), the same replicaID will reappear on the
	// next tick's staleIDs list. Step 2a will affect 0 rows (already
	// marked failed). Without this gate, step 2b would re-read the
	// same already-failed rows and double-decrement user_quotas
	// (clamped at 0 by GREATEST, but still wrong). Review-1 #2.
	if markedRuns > 0 {
		if err := s.reapQuotas(ctx, replicaID); err != nil {
			s.logf("coord: replicas sweep: quota reap for replica %s: %v", replicaID, err)
			// fall through to step 2c — quota leak is bounded and
			// recoverable; failing the whole reap would leave the
			// replica row in place to retry forever
		}
	}

	// Step 2c: delete the replica row.
	if _, err := s.pool.Exec(ctx, `DELETE FROM replicas WHERE id = $1`, replicaID); err != nil {
		return fmt.Errorf("delete replica row: %w", err)
	}
	s.logf("coord: replicas sweep: reaped replica %s", replicaID)
	return nil
}

// reapQuotas executes step 2b — per-user run count + per-user
// user_quotas decrement. Extracted from reapReplica so the
// markedRuns > 0 gate stays clean.
func (s *ReplicasSweeper) reapQuotas(ctx context.Context, replicaID string) error {
	quotaRows, err := s.pool.Query(ctx, `
		SELECT user_id, COUNT(*) AS leaked
		  FROM runs
		 WHERE replica_id = $1
		   AND status = 'failed'
		   AND error = 'owner replica died'
		   AND user_id IS NOT NULL AND user_id != ''
		 GROUP BY user_id
	`, replicaID)
	if err != nil {
		return fmt.Errorf("query quota reap: %w", err)
	}
	type reap struct {
		userID string
		leaked int
	}
	var reaps []reap
	for quotaRows.Next() {
		var r reap
		if err := quotaRows.Scan(&r.userID, &r.leaked); err != nil {
			quotaRows.Close()
			return fmt.Errorf("scan quota reap row: %w", err)
		}
		reaps = append(reaps, r)
	}
	quotaRows.Close()
	if err := quotaRows.Err(); err != nil {
		return fmt.Errorf("iterate quota reap: %w", err)
	}
	for _, r := range reaps {
		_, qerr := s.pool.Exec(ctx, `
			UPDATE user_quotas
			   SET active_count = GREATEST(0, active_count - $2),
			       updated_at   = now()
			 WHERE user_id = $1
		`, r.userID, r.leaked)
		if qerr != nil {
			s.logf("coord: replicas sweep: quota decrement for user %s by %d: %v", r.userID, r.leaked, qerr)
			continue
		}
		s.logf("coord: replicas sweep: quota reap for user %s: decremented by %d", r.userID, r.leaked)
	}
	return nil
}
