package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// immediateTx runs fn in an IMMEDIATE transaction on a pinned connection (the
// write lock taken at BEGIN, as MemoryIncrement does), so a read-then-write
// in fn cannot interleave with another writer.
func (s *Store) immediateTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}

// errNoChange ends an immediateTx with nothing written.
var errNoChange = errors.New("no change")

// SCIMRemoveUser implements store.SCIMStore: the count, the disable and the
// insert in one IMMEDIATE transaction.
func (s *Store) SCIMRemoveUser(ctx context.Context, row store.AuthRemovalRow, since time.Time, ceiling int) (bool, error) {
	if row.Subject == "" || row.Gate == "" || row.DisabledAt.IsZero() || row.UnbindDueAt.IsZero() {
		return false, fmt.Errorf("scim removal: subject, gate, disabled_at and unbind_due_at are required")
	}
	if row.UnbindDueAt.Before(row.DisabledAt) || (!row.EraseDueAt.IsZero() && row.EraseDueAt.Before(row.UnbindDueAt)) {
		return false, fmt.Errorf("scim removal: stages must be due in order (disable <= unbind <= erase)")
	}
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		var status string
		err := conn.QueryRowContext(ctx, `SELECT status FROM users WHERE tenant_id = ? AND subject = ?`, row.TenantID, row.Subject).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.ErrNotFound{Kind: "user", ID: row.Subject}
		}
		if err != nil {
			return fmt.Errorf("scim removal: read user: %w", err)
		}
		if status == "disabled" {
			return errNoChange
		}
		reason := store.RemovalReasonSCIM
		if status == store.UserStatusPending {
			reason = store.RemovalReasonSCIMPending
		}
		var existingGate, existingReason string
		err = conn.QueryRowContext(ctx, `SELECT gate, reason FROM auth_removals WHERE tenant_id = ? AND subject = ?`,
			row.TenantID, row.Subject).Scan(&existingGate, &existingReason)
		switch {
		case err == nil:
			// A removal row behind a user who is not disabled: fail closed —
			// disable the user under the row. Another owner's row stays
			// theirs; this gate's SCIM row records the status the user had.
			if _, err := conn.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = ? AND subject = ?`, row.TenantID, row.Subject); err != nil {
				return fmt.Errorf("scim removal: disable: %w", err)
			}
			if existingGate == row.Gate && store.SCIMRemovalReason(existingReason) {
				if _, err := conn.ExecContext(ctx, `UPDATE auth_removals SET reason = ? WHERE tenant_id = ? AND subject = ?`,
					reason, row.TenantID, row.Subject); err != nil {
					return fmt.Errorf("scim removal: refresh: %w", err)
				}
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("scim removal: read removal: %w", err)
		}
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_removals WHERE gate = ? AND tenant_id = ? AND disabled_at >= ?`,
			row.Gate, row.TenantID, since.UnixNano()).Scan(&n); err != nil {
			return fmt.Errorf("scim removal: count: %w", err)
		}
		if n >= ceiling {
			return store.ErrSCIMRemovalCeiling
		}
		if _, err := conn.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = ? AND subject = ?`, row.TenantID, row.Subject); err != nil {
			return fmt.Errorf("scim removal: disable: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO auth_removals (`+removalCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)`,
			row.TenantID, row.Subject, row.Gate, reason, row.DisabledAt.UnixNano(), row.UnbindDueAt.UnixNano(),
			gateNanos(row.EraseDueAt), row.CreatedBy); err != nil {
			return fmt.Errorf("scim removal: insert: %w", err)
		}
		return nil
	})
	if errors.Is(err, errNoChange) {
		return false, nil
	}
	return err == nil, err
}

// SCIMReactivate implements store.SCIMStore.
func (s *Store) SCIMReactivate(ctx context.Context, gate, tenantID, subject string) (string, bool, error) {
	restored := ""
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		var rowGate, reason string
		err := conn.QueryRowContext(ctx, `SELECT gate, reason FROM auth_removals WHERE tenant_id = ? AND subject = ?`,
			tenantID, subject).Scan(&rowGate, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			return errNoChange
		}
		if err != nil {
			return fmt.Errorf("scim reactivate: read removal: %w", err)
		}
		if rowGate != gate || !store.SCIMRemovalReason(reason) {
			return errNoChange
		}
		restored = "active"
		if reason == store.RemovalReasonSCIMPending {
			restored = store.UserStatusPending
		}
		res, err := conn.ExecContext(ctx, `UPDATE users SET status = ? WHERE tenant_id = ? AND subject = ? AND status = 'disabled'`,
			restored, tenantID, subject)
		if err != nil {
			return fmt.Errorf("scim reactivate: user: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errNoChange
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM auth_removals WHERE tenant_id = ? AND subject = ?`, tenantID, subject); err != nil {
			return fmt.Errorf("scim reactivate: removal: %w", err)
		}
		return nil
	})
	if errors.Is(err, errNoChange) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return restored, true, nil
}
