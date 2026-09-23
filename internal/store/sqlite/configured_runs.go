package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

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
	err := s.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.ErrNotFound{Kind: "run", ID: runID}
	}
	if err != nil {
		return err
	}
	return store.ErrRunNotConfigured
}

func (s *Store) GetRunDraft(ctx context.Context, runID string) (json.RawMessage, error) {
	var draft sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT draft FROM runs WHERE id = ? AND status = ?`, runID, string(store.RunConfigured),
	).Scan(&draft)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, s.draftGuardErr(ctx, runID)
	}
	if err != nil {
		return nil, err
	}
	if !draft.Valid || draft.String == "" {
		return nil, nil
	}
	return json.RawMessage(draft.String), nil
}

func (s *Store) UpdateRunDraft(ctx context.Context, runID string, draft json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET draft = ? WHERE id = ? AND status = ?`,
		nilIfEmptyRaw(draft), runID, string(store.RunConfigured),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.draftGuardErr(ctx, runID)
	}
	return nil
}

func (s *Store) StartConfiguredRun(ctx context.Context, runID string, identity store.RunIdentity) (store.Run, error) {
	now := time.Now().UnixNano()
	// SQLite's runs table has no replica_id column: a single-file store is a
	// single-replica deployment, so there is nothing to route cancel to.
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET
			status                  = ?,
			started_at              = ?,
			last_heartbeat_at       = ?,
			agent_def_id            = ?,
			model                   = ?,
			user_tier               = ?,
			interactive             = ?,
			operator_key_restricted = ?,
			isolated                = ?,
			run_config              = ?,
			draft                   = NULL
		WHERE id = ? AND status = ?`,
		string(store.RunRunning), now, now,
		nilIfEmpty(identity.AgentDefID),
		nilIfEmpty(identity.Model),
		nilIfEmpty(identity.UserTier),
		boolToInt(identity.Interactive),
		boolToInt(identity.OperatorKeyRestricted),
		boolToInt(identity.Isolated),
		nilIfEmptyRaw(identity.RunConfig),
		runID, string(store.RunConfigured),
	)
	if err != nil {
		return store.Run{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.Run{}, s.draftGuardErr(ctx, runID)
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) DeleteConfiguredRun(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var sessionID string
	err = tx.QueryRowContext(ctx,
		`SELECT session_id FROM runs WHERE id = ? AND status = ?`, runID, string(store.RunConfigured),
	).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return s.draftGuardErr(ctx, runID)
	}
	if err != nil {
		return err
	}
	if err := deleteDraftTx(ctx, tx, runID, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteDraftTx removes one draft and, when it was the only run in its
// session, the session. The status guard is repeated in the DELETE so a start
// that won the race between the caller's read and this statement is never
// deleted out from under a running loop.
func deleteDraftTx(ctx context.Context, tx *sql.Tx, runID, sessionID string) error {
	// Events first, as DeleteSessionCascade orders them: they reference the
	// run. Inside the transaction, so a guard that fails below rolls this back.
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE run_id = ?`, runID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id = ? AND status = ?`, runID, string(store.RunConfigured))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrRunNotConfigured
	}
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE session_id = ?`, sessionID).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE session_id = ?`, sessionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CountConfiguredRuns(ctx context.Context, tenantID, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE status = ? AND COALESCE(tenant_id, '') = ? AND COALESCE(user_id, '') = ?`,
		string(store.RunConfigured), tenantID, userID,
	).Scan(&n)
	return n, err
}

func (s *Store) SweepExpiredConfiguredRuns(ctx context.Context, cutoff time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id FROM runs WHERE status = ? AND started_at < ?`,
		string(store.RunConfigured), cutoff.UnixNano(),
	)
	if err != nil {
		return 0, err
	}
	type draft struct{ id, session string }
	var expired []draft
	for rows.Next() {
		var d draft
		if err := rows.Scan(&d.id, &d.session); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	swept := 0
	for _, d := range expired {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return swept, err
		}
		err = deleteDraftTx(ctx, tx, d.id, d.session)
		if errors.Is(err, store.ErrRunNotConfigured) {
			_ = tx.Rollback() // started since the scan; not ours to delete
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			return swept, err
		}
		if err := tx.Commit(); err != nil {
			return swept, err
		}
		swept++
	}
	return swept, nil
}
