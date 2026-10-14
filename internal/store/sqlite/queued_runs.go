package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EI P3: queued runs — accepted, waiting for a run slot. See the
// store.Store interface for the contract of each operation.

func (s *Store) CreateQueuedRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	return s.createRun(ctx, sessionID, identity, store.RunQueued, nil)
}

func (s *Store) QueueConfiguredRun(ctx context.Context, runID string, draft json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ? WHERE id = ? AND status = ? AND (? IS NULL OR draft = ?)`,
		string(store.RunQueued), runID, string(store.RunConfigured), nilIfEmptyRaw(draft), nilIfEmptyRaw(draft),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.draftGuardErr(ctx, runID)
	}
	return nil
}

func (s *Store) StartQueuedRun(ctx context.Context, runID string, identity store.RunIdentity) (store.Run, error) {
	n, err := s.startRun(ctx, runID, identity, store.RunQueued, nil)
	if err != nil {
		return store.Run{}, err
	}
	if n == 0 {
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
		}
		if err != nil {
			return store.Run{}, err
		}
		return store.Run{}, store.ErrRunNotQueued
	}
	return s.GetRun(ctx, runID)
}

// FailQueuedRuns ignores replicaID: a single-file store is a single-replica
// deployment (its runs table has no replica_id), so every queued row lost its
// waiter with the restart.
func (s *Store) FailQueuedRuns(ctx context.Context, _ string, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, completed_at = ?, error = ?, draft = NULL WHERE status = ?`,
		string(store.RunFailed), time.Now().UnixNano(), reason, string(store.RunQueued),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
