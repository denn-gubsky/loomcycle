package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SCIMRemovalReserve implements store.SCIMStore: a transaction-scoped
// advisory lock per (gate, tenant) serialises the count and the insert across
// every replica, so concurrent removals cannot pass the ceiling together.
func (s *Store) SCIMRemovalReserve(ctx context.Context, row store.AuthRemovalRow, since time.Time, ceiling int) (bool, error) {
	if row.Subject == "" || row.Gate == "" || row.Reason == "" || row.DisabledAt.IsZero() || row.UnbindDueAt.IsZero() {
		return false, fmt.Errorf("scim removal: subject, gate, reason, disabled_at and unbind_due_at are required")
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
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM auth_removals WHERE gate = $1 AND tenant_id = $2 AND disabled_at >= $3`,
		row.Gate, row.TenantID, since.UTC()).Scan(&n); err != nil {
		return false, fmt.Errorf("scim removal: count: %w", err)
	}
	if n >= ceiling {
		return false, store.ErrSCIMRemovalCeiling
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO auth_removals (`+removalCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, NULL, $8)
		ON CONFLICT (tenant_id, subject) DO NOTHING`,
		row.TenantID, row.Subject, row.Gate, row.Reason, row.DisabledAt.UTC(), row.UnbindDueAt.UTC(),
		pgNullTime(row.EraseDueAt), row.CreatedBy)
	if err != nil {
		return false, fmt.Errorf("scim removal: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
