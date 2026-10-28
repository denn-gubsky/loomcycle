package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SCIMRemovalReserve implements store.SCIMStore. The count and the insert run
// in one IMMEDIATE transaction (the write lock taken at BEGIN, on a pinned
// connection, as MemoryIncrement does), so concurrent removals serialise and
// cannot pass the ceiling together.
func (s *Store) SCIMRemovalReserve(ctx context.Context, row store.AuthRemovalRow, since time.Time, ceiling int) (bool, error) {
	if row.Subject == "" || row.Gate == "" || row.Reason == "" || row.DisabledAt.IsZero() || row.UnbindDueAt.IsZero() {
		return false, fmt.Errorf("scim removal: subject, gate, reason, disabled_at and unbind_due_at are required")
	}
	if row.UnbindDueAt.Before(row.DisabledAt) || (!row.EraseDueAt.IsZero() && row.EraseDueAt.Before(row.UnbindDueAt)) {
		return false, fmt.Errorf("scim removal: stages must be due in order (disable <= unbind <= erase)")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_removals WHERE gate = ? AND tenant_id = ? AND disabled_at >= ?`,
		row.Gate, row.TenantID, since.UnixNano()).Scan(&n); err != nil {
		return false, fmt.Errorf("scim removal: count: %w", err)
	}
	if n >= ceiling {
		return false, store.ErrSCIMRemovalCeiling
	}
	res, err := conn.ExecContext(ctx, `
		INSERT INTO auth_removals (`+removalCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
		ON CONFLICT (tenant_id, subject) DO NOTHING`,
		row.TenantID, row.Subject, row.Gate, row.Reason, row.DisabledAt.UnixNano(), row.UnbindDueAt.UnixNano(),
		gateNanos(row.EraseDueAt), row.CreatedBy)
	if err != nil {
		return false, fmt.Errorf("scim removal: insert: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return inserted > 0, nil
}
