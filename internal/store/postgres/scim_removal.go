package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SCIMRemoveUser implements store.SCIMStore. A transaction-scoped advisory
// lock per (gate, tenant) serialises the count, the disable and the insert
// across every replica; the user row is locked too, so an operator's
// concurrent change of it waits or wins whole.
func (s *Store) SCIMRemoveUser(ctx context.Context, row store.AuthRemovalRow, since time.Time, ceiling int) (bool, error) {
	if row.Subject == "" || row.Gate == "" || row.DisabledAt.IsZero() || row.UnbindDueAt.IsZero() {
		return false, fmt.Errorf("scim removal: subject, gate, disabled_at and unbind_due_at are required")
	}
	if row.UnbindDueAt.Before(row.DisabledAt) || (!row.EraseDueAt.IsZero() && row.EraseDueAt.Before(row.UnbindDueAt)) {
		return false, fmt.Errorf("scim removal: stages must be due in order (disable <= unbind <= erase)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('loomcycle/scim-removal/' || $1::text || '/' || $2::text, 0))`,
		row.Gate, row.TenantID); err != nil {
		return false, fmt.Errorf("scim removal: lock: %w", err)
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`, row.TenantID, row.Subject).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, &store.ErrNotFound{Kind: "user", ID: row.Subject}
	}
	if err != nil {
		return false, fmt.Errorf("scim removal: read user: %w", err)
	}
	if status == "disabled" {
		return false, nil
	}
	reason := store.RemovalReasonSCIM
	if status == store.UserStatusPending {
		reason = store.RemovalReasonSCIMPending
	}
	var existingGate, existingReason string
	err = tx.QueryRow(ctx, `SELECT gate, reason FROM auth_removals WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`,
		row.TenantID, row.Subject).Scan(&existingGate, &existingReason)
	switch {
	case err == nil:
		// A removal row behind a user who is not disabled: fail closed —
		// disable the user under the row. Another owner's row stays theirs;
		// this gate's SCIM row records the status the user had now.
		if _, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = $1 AND subject = $2`, row.TenantID, row.Subject); err != nil {
			return false, fmt.Errorf("scim removal: disable: %w", err)
		}
		if existingGate == row.Gate && store.SCIMRemovalReason(existingReason) {
			if _, err := tx.Exec(ctx, `UPDATE auth_removals SET reason = $3 WHERE tenant_id = $1 AND subject = $2`,
				row.TenantID, row.Subject, reason); err != nil {
				return false, fmt.Errorf("scim removal: refresh: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("scim removal: read removal: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM auth_removals WHERE gate = $1 AND tenant_id = $2 AND disabled_at >= $3`,
		row.Gate, row.TenantID, since.UTC()).Scan(&n); err != nil {
		return false, fmt.Errorf("scim removal: count: %w", err)
	}
	if n >= ceiling {
		return false, store.ErrSCIMRemovalCeiling
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = $1 AND subject = $2`, row.TenantID, row.Subject); err != nil {
		return false, fmt.Errorf("scim removal: disable: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_removals (`+removalCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, NULL, $8)`,
		row.TenantID, row.Subject, row.Gate, reason, row.DisabledAt.UTC(), row.UnbindDueAt.UTC(),
		pgNullTime(row.EraseDueAt), row.CreatedBy); err != nil {
		return false, fmt.Errorf("scim removal: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// SCIMReactivate implements store.SCIMStore.
func (s *Store) SCIMReactivate(ctx context.Context, gate, tenantID, subject string) (string, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var rowGate, reason string
	err = tx.QueryRow(ctx, `SELECT gate, reason FROM auth_removals WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`,
		tenantID, subject).Scan(&rowGate, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("scim reactivate: read removal: %w", err)
	}
	if rowGate != gate || !store.SCIMRemovalReason(reason) {
		return "", false, nil
	}
	restored := "active"
	if reason == store.RemovalReasonSCIMPending {
		restored = store.UserStatusPending
	}
	tag, err := tx.Exec(ctx, `UPDATE users SET status = $3 WHERE tenant_id = $1 AND subject = $2 AND status = 'disabled'`,
		tenantID, subject, restored)
	if err != nil {
		return "", false, fmt.Errorf("scim reactivate: user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2`, tenantID, subject); err != nil {
		return "", false, fmt.Errorf("scim reactivate: removal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return restored, true, nil
}
