package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EI P3: queued runs — accepted, waiting for a run slot. See the
// store.Store interface for the contract of each operation.

func (s *Store) CreateQueuedRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	return s.createRun(ctx, sessionID, identity, store.RunQueued, nil)
}

func (s *Store) QueueConfiguredRun(ctx context.Context, runID string, draft json.RawMessage) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET status = $2
		WHERE id = $1 AND status = $3 AND ($4::jsonb IS NULL OR draft = $4::jsonb)`,
		runID, string(store.RunQueued), string(store.RunConfigured), nullableJSONArg(draft),
	)
	if err != nil {
		return fmt.Errorf("queue configured run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.draftGuardErr(ctx, runID)
	}
	return nil
}

func (s *Store) StartQueuedRun(ctx context.Context, runID string, identity store.RunIdentity) (store.Run, error) {
	n, err := s.startRun(ctx, runID, identity, store.RunQueued, nil)
	if err != nil {
		return store.Run{}, fmt.Errorf("start queued run: %w", err)
	}
	if n == 0 {
		var status string
		err := s.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
		}
		if err != nil {
			return store.Run{}, fmt.Errorf("read run status: %w", err)
		}
		return store.Run{}, store.ErrRunNotQueued
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) FailQueuedRuns(ctx context.Context, replicaID string, startedBefore time.Time, reason string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET status = $1, completed_at = $2, error = $3, stop_reason = 'restarted',
			draft = NULL, idempotency_key = NULL
		WHERE status = $4 AND started_at < $6 AND ($5 = '' OR COALESCE(replica_id, '') = $5)`,
		string(store.RunFailed), time.Now().UTC(), reason, string(store.RunQueued), replicaID, startedBefore.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("fail queued runs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
