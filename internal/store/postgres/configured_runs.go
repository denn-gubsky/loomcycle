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

// RFC DI D5: configured (created, not started) runs. See the store.Store
// interface for the contract of each operation.

func (s *Store) CreateConfiguredRun(ctx context.Context, sessionID string, identity store.RunIdentity, draft json.RawMessage) (store.Run, error) {
	return s.createRun(ctx, sessionID, identity, store.RunConfigured, draft)
}

// draftGuardErr turns "the guarded statement matched no row" into the right
// sentinel: ErrNotFound when there is no such run, ErrRunNotConfigured when
// there is and it is not a draft.
func (s *Store) draftGuardErr(ctx context.Context, runID string) error {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.ErrNotFound{Kind: "run", ID: runID}
	}
	if err != nil {
		return fmt.Errorf("read run status: %w", err)
	}
	return store.ErrRunNotConfigured
}

func (s *Store) GetRunDraft(ctx context.Context, runID string) (json.RawMessage, error) {
	var draft *string
	err := s.pool.QueryRow(ctx,
		`SELECT draft::text FROM runs WHERE id = $1 AND status = $2`, runID, string(store.RunConfigured),
	).Scan(&draft)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.draftGuardErr(ctx, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("get run draft: %w", err)
	}
	if draft == nil || *draft == "" {
		return nil, nil
	}
	return json.RawMessage(*draft), nil
}

func (s *Store) UpdateRunDraft(ctx context.Context, runID string, draft json.RawMessage) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET draft = $2::jsonb WHERE id = $1 AND status = $3`,
		runID, nullableJSONArg(draft), string(store.RunConfigured),
	)
	if err != nil {
		return fmt.Errorf("update run draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.draftGuardErr(ctx, runID)
	}
	return nil
}

func (s *Store) StartConfiguredRun(ctx context.Context, runID string, identity store.RunIdentity) (store.Run, error) {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET
			status                  = $2,
			started_at              = $3,
			last_heartbeat_at       = $3,
			agent_def_id            = $4,
			model                   = $5,
			user_tier               = $6,
			interactive             = $7,
			operator_key_restricted = $8,
			isolated                = $9,
			run_config              = $10::jsonb,
			replica_id              = $11,
			draft                   = NULL
		WHERE id = $1 AND status = $12`,
		runID, string(store.RunRunning), now,
		nullableText(identity.AgentDefID),
		nullableText(identity.Model),
		nullableText(identity.UserTier),
		identity.Interactive,
		identity.OperatorKeyRestricted,
		identity.Isolated,
		nullableJSONArg(identity.RunConfig),
		nullableText(identity.ReplicaID),
		string(store.RunConfigured),
	)
	if err != nil {
		return store.Run{}, fmt.Errorf("start configured run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.Run{}, s.draftGuardErr(ctx, runID)
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) DeleteConfiguredRun(ctx context.Context, runID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete-draft tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var sessionID string
	err = tx.QueryRow(ctx,
		`SELECT session_id FROM runs WHERE id = $1 AND status = $2 FOR UPDATE`, runID, string(store.RunConfigured),
	).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.draftGuardErr(ctx, runID)
	}
	if err != nil {
		return fmt.Errorf("lock draft: %w", err)
	}
	if err := deleteDraftTx(ctx, tx, runID, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// deleteDraftTx removes one draft and, when it was the only run in its
// session, the session. Events go first, as DeleteSessionCascade orders them;
// the status guard is repeated on the run DELETE so a start that won a race
// is never deleted out from under a running loop (the whole tx rolls back).
func deleteDraftTx(ctx context.Context, tx pgx.Tx, runID, sessionID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM events WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("delete draft events: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM runs WHERE id = $1 AND status = $2`, runID, string(store.RunConfigured))
	if err != nil {
		return fmt.Errorf("delete draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrRunNotConfigured
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM runs WHERE session_id = $1`, sessionID).Scan(&others); err != nil {
		return fmt.Errorf("count session runs: %w", err)
	}
	if others == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM events WHERE session_id = $1`, sessionID); err != nil {
			return fmt.Errorf("delete draft session events: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
			return fmt.Errorf("delete draft session: %w", err)
		}
	}
	return nil
}

func (s *Store) CountConfiguredRuns(ctx context.Context, tenantID, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE status = $1 AND COALESCE(tenant_id, '') = $2 AND COALESCE(user_id, '') = $3`,
		string(store.RunConfigured), tenantID, userID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count configured runs: %w", err)
	}
	return n, nil
}

func (s *Store) SweepExpiredConfiguredRuns(ctx context.Context, cutoff time.Time) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, session_id FROM runs WHERE status = $1 AND started_at < $2`,
		string(store.RunConfigured), cutoff.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("scan expired drafts: %w", err)
	}
	type draft struct{ id, session string }
	var expired []draft
	for rows.Next() {
		var d draft
		if err := rows.Scan(&d.id, &d.session); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired draft: %w", err)
		}
		expired = append(expired, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	swept := 0
	for _, d := range expired {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return swept, fmt.Errorf("begin sweep tx: %w", err)
		}
		err = deleteDraftTx(ctx, tx, d.id, d.session)
		if errors.Is(err, store.ErrRunNotConfigured) {
			_ = tx.Rollback(ctx) // started since the scan; not ours to delete
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return swept, err
		}
		if err := tx.Commit(ctx); err != nil {
			return swept, fmt.Errorf("commit sweep tx: %w", err)
		}
		swept++
	}
	return swept, nil
}
