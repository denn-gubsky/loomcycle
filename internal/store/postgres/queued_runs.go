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

func (s *Store) SetQueuedInput(ctx context.Context, runID string, input json.RawMessage) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET queued_input = $2::jsonb WHERE id = $1 AND status = $3`,
		runID, nullableJSONArg(input), string(store.RunQueued),
	)
	if err != nil {
		return fmt.Errorf("set queued input: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrRunNotQueued
	}
	return nil
}

func (s *Store) ListQueuedRuns(ctx context.Context, replicaID string, startedBefore time.Time) ([]store.QueuedRun, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, queued_input FROM runs
		WHERE status = $1 AND started_at < $2 AND ($3 = '' OR COALESCE(replica_id, '') = $3)
		ORDER BY started_at, id`,
		string(store.RunQueued), startedBefore.UTC(), replicaID,
	)
	if err != nil {
		return nil, fmt.Errorf("list queued runs: %w", err)
	}
	type found struct {
		id    string
		input []byte
	}
	var ids []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.id, &f.input); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list queued runs: %w", err)
		}
		ids = append(ids, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queued runs: %w", err)
	}
	out := make([]store.QueuedRun, 0, len(ids))
	for _, f := range ids {
		run, err := s.GetRun(ctx, f.id)
		if err != nil {
			return nil, err
		}
		q := store.QueuedRun{Run: run}
		if len(f.input) > 0 {
			q.Input = json.RawMessage(f.input)
		}
		out = append(out, q)
	}
	return out, nil
}

func (s *Store) FailQueuedRun(ctx context.Context, runID, reason string) error {
	return s.EndQueuedRun(ctx, runID, store.RunFailed, "restarted", reason)
}

func (s *Store) EndQueuedRun(ctx context.Context, runID string, status store.RunStatus, stopReason, errMsg string) error {
	if status != store.RunCancelled && status != store.RunFailed {
		return fmt.Errorf("end queued run: %q is not an end a queued run can have", status)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET status = $2, completed_at = $3, error = $4, stop_reason = $5,
			draft = NULL, queued_input = NULL, idempotency_key = NULL, delivery_alt_key = NULL
		WHERE id = $1 AND status = $6`,
		runID, string(status), time.Now().UTC(), errMsg, stopReason, string(store.RunQueued),
	)
	if err != nil {
		return fmt.Errorf("end queued run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrRunNotQueued
	}
	return nil
}
