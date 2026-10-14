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

func (s *Store) SetQueuedInput(ctx context.Context, runID string, input json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET queued_input = ? WHERE id = ? AND status = ?`,
		nilIfEmptyRaw(input), runID, string(store.RunQueued),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrRunNotQueued
	}
	return nil
}

// ListQueuedRuns ignores replicaID: a single-file store is a single-replica
// deployment (its runs table has no replica_id), so every queued row lost its
// waiter with the restart.
func (s *Store) ListQueuedRuns(ctx context.Context, _ string, startedBefore time.Time) ([]store.QueuedRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, queued_input FROM runs WHERE status = ? AND started_at < ? ORDER BY started_at, id`,
		string(store.RunQueued), startedBefore.UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	type found struct {
		id    string
		input sql.NullString
	}
	var ids []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.id, &f.input); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, f)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	out := make([]store.QueuedRun, 0, len(ids))
	for _, f := range ids {
		run, err := s.GetRun(ctx, f.id)
		if err != nil {
			return nil, err
		}
		q := store.QueuedRun{Run: run}
		if f.input.Valid && f.input.String != "" {
			q.Input = json.RawMessage(f.input.String)
		}
		out = append(out, q)
	}
	return out, nil
}

func (s *Store) FailQueuedRun(ctx context.Context, runID, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, completed_at = ?, error = ?, stop_reason = 'restarted',
			draft = NULL, queued_input = NULL, idempotency_key = NULL, delivery_alt_key = NULL
		WHERE id = ? AND status = ?`,
		string(store.RunFailed), time.Now().UnixNano(), reason, runID, string(store.RunQueued),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrRunNotQueued
	}
	return nil
}
