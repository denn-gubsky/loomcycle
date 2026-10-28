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

// SCIMRemoveUser implements store.SCIMStore (the state machine is on the
// interface): the count and the writes in one IMMEDIATE transaction.
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
		// The ceiling before any write. A stale row of this subject is not
		// counted: it is replaced below, not added to.
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_removals WHERE gate = ? AND tenant_id = ? AND disabled_at >= ? AND subject <> ?`,
			row.Gate, row.TenantID, since.UnixNano(), row.Subject).Scan(&n); err != nil {
			return fmt.Errorf("scim removal: count: %w", err)
		}
		if n >= ceiling {
			return store.ErrSCIMRemovalCeiling
		}
		// A removal row behind a user who is not disabled is no longer in
		// force: it goes, with its deadlines, and a fresh one replaces it.
		if _, err := conn.ExecContext(ctx, `DELETE FROM auth_removals WHERE tenant_id = ? AND subject = ?`, row.TenantID, row.Subject); err != nil {
			return fmt.Errorf("scim removal: drop a stale removal: %w", err)
		}
		reason := store.RemovalReasonSCIM
		if status == store.UserStatusPending {
			reason = store.RemovalReasonSCIMPending
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

// claimedReason is the reactivating reason a SCIM reason claims to.
func claimedReason(reason string) string {
	if reason == store.RemovalReasonSCIMPending || reason == store.RemovalReasonSCIMReactivatingPending {
		return store.RemovalReasonSCIMReactivatingPending
	}
	return store.RemovalReasonSCIMReactivating
}

// SCIMReactivateClaim implements store.SCIMStore.
func (s *Store) SCIMReactivateClaim(ctx context.Context, gate, tenantID, subject string) (bool, error) {
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
		if _, err := conn.ExecContext(ctx, `UPDATE auth_removals SET reason = ? WHERE tenant_id = ? AND subject = ?`,
			claimedReason(reason), tenantID, subject); err != nil {
			return fmt.Errorf("scim reactivate: claim: %w", err)
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
		switch {
		case rowGate != gate:
			return errNoChange
		case reason == store.RemovalReasonSCIMReactivating:
			restored = "active"
		case reason == store.RemovalReasonSCIMReactivatingPending:
			restored = store.UserStatusPending
		default:
			return errNoChange
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
