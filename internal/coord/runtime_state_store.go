package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RuntimeStateStore is the v0.12.3 Phase 4 read/write surface for
// the cluster's pause state. Backed by the single-row runtime_state
// table (id = 'singleton'). Every replica in cluster mode reads
// here on its run-admission gate + iteration-boundary check (via
// pause.Manager's 1s cache); only pause/resume callers write.
//
// Lives in the coord package alongside ReplicaStore + UserQuotaStore
// — Postgres-only (SQLite refuses cluster mode), takes a pgxpool
// directly (not the store.Store interface).
type RuntimeStateStore struct {
	pool *pgxpool.Pool
}

func NewRuntimeStateStore(pool *pgxpool.Pool) *RuntimeStateStore {
	return &RuntimeStateStore{pool: pool}
}

// Get reads the singleton state row. Returns ("running", 0, nil) if
// the row doesn't exist (migration 0025 seeds it on first apply, so
// this should never happen in practice; the defensive default keeps
// the boot path resilient against an out-of-band migration rollback).
func (s *RuntimeStateStore) Get(ctx context.Context) (state string, pausedRunsCount int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT state, paused_runs_count
		  FROM runtime_state
		 WHERE id = 'singleton'
	`).Scan(&state, &pausedRunsCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "running", 0, nil
		}
		return "", 0, fmt.Errorf("runtime_state get: %w", err)
	}
	return state, pausedRunsCount, nil
}

// Set writes the singleton row atomically. state must be one of
// "running", "pausing", "paused"; validation is the caller's
// responsibility (the schema has no CHECK constraint so we can add
// states without a migration). paused_at is written when state is
// "paused" and cleared otherwise.
func (s *RuntimeStateStore) Set(ctx context.Context, state string, pausedRunsCount int) error {
	if state == "" {
		return errors.New("runtime_state: state is empty")
	}
	pausedAt := "NULL"
	if state == "paused" {
		pausedAt = "now()"
	}
	// pg_notify-style param substitution would need a CASE; simpler
	// to splice the literal (pausedAt is a closed-set constant string
	// from this function, not user input).
	query := fmt.Sprintf(`
		INSERT INTO runtime_state (id, state, state_changed_at, paused_at, paused_runs_count)
		VALUES ('singleton', $1, now(), %s, $2)
		ON CONFLICT (id) DO UPDATE
		   SET state             = EXCLUDED.state,
		       state_changed_at  = now(),
		       paused_at         = %s,
		       paused_runs_count = EXCLUDED.paused_runs_count
	`, pausedAt, pausedAt)
	if _, err := s.pool.Exec(ctx, query, state, pausedRunsCount); err != nil {
		return fmt.Errorf("runtime_state set %s: %w", state, err)
	}
	return nil
}
