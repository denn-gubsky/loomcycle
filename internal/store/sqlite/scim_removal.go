package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
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

// userStatus reads the user's status inside an IMMEDIATE transaction (which
// already holds the write lock). The same order as Postgres: the user first.
func userStatus(ctx context.Context, conn *sql.Conn, tenantID, subject string) (string, bool, error) {
	var status string
	err := conn.QueryRowContext(ctx, `SELECT status FROM users WHERE tenant_id = ? AND subject = ?`, tenantID, subject).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return status, err == nil, err
}

// SCIMReactivateClaim implements store.SCIMStore.
func (s *Store) SCIMReactivateClaim(ctx context.Context, gate, tenantID, subject string, at, since time.Time, ceiling int) (bool, error) {
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		status, found, err := userStatus(ctx, conn, tenantID, subject)
		if err != nil {
			return fmt.Errorf("scim reactivate: read user: %w", err)
		}
		if !found || status != "disabled" {
			return errNoChange
		}
		var rowGate, reason, stage string
		var erased sql.NullInt64
		err = conn.QueryRowContext(ctx, `SELECT gate, reason, stage, erased_at FROM auth_removals WHERE tenant_id = ? AND subject = ?`,
			tenantID, subject).Scan(&rowGate, &reason, &stage, &erased)
		if errors.Is(err, sql.ErrNoRows) {
			return errNoChange
		}
		if err != nil {
			return fmt.Errorf("scim reactivate: read removal: %w", err)
		}
		if rowGate != gate || !store.SCIMRemovalReason(reason) {
			return errNoChange
		}
		if stage != "" || erased.Valid {
			return store.ErrRemovalBusy
		}
		if claimedReason(reason) != reason { // a new claim, not a retry
			var n int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM scim_users WHERE gate = ? AND tenant_id = ? AND reactivated_at >= ?`,
				gate, tenantID, since.UnixNano()).Scan(&n); err != nil {
				return fmt.Errorf("scim reactivate: count: %w", err)
			}
			if n >= ceiling {
				return store.ErrSCIMReactivationCeiling
			}
		}
		if _, err := conn.ExecContext(ctx, `UPDATE auth_removals SET reason = ? WHERE tenant_id = ? AND subject = ?`,
			claimedReason(reason), tenantID, subject); err != nil {
			return fmt.Errorf("scim reactivate: claim: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE scim_users SET reactivated_at = ? WHERE gate = ? AND tenant_id = ? AND subject = ? AND deleted_at IS NULL`,
			at.UnixNano(), gate, tenantID, subject); err != nil {
			return fmt.Errorf("scim reactivate: stamp: %w", err)
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
		status, found, err := userStatus(ctx, conn, tenantID, subject)
		if err != nil {
			return fmt.Errorf("scim reactivate: read user: %w", err)
		}
		if !found || status != "disabled" {
			return errNoChange
		}
		var rowGate, reason string
		err = conn.QueryRowContext(ctx, `SELECT gate, reason FROM auth_removals WHERE tenant_id = ? AND subject = ?`,
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
		if _, err := conn.ExecContext(ctx, `UPDATE users SET status = ? WHERE tenant_id = ? AND subject = ?`, restored, tenantID, subject); err != nil {
			return fmt.Errorf("scim reactivate: user: %w", err)
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

// OperatorDisableUser implements store.SCIMStore.
func (s *Store) OperatorDisableUser(ctx context.Context, tenantID, subject string) (bool, error) {
	disowned := false
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		status, found, err := userStatus(ctx, conn, tenantID, subject)
		if err != nil {
			return fmt.Errorf("operator disable: read user: %w", err)
		}
		if !found {
			return &store.ErrNotFound{Kind: "user", ID: subject}
		}
		if status == store.UserStatusPending {
			return store.ErrUserPending
		}
		var pendingRemoval bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM auth_removals WHERE tenant_id = ? AND subject = ?
			AND reason IN ('`+store.RemovalReasonSCIMPending+`', '`+store.RemovalReasonSCIMReactivatingPending+`'))`,
			tenantID, subject).Scan(&pendingRemoval); err != nil {
			return fmt.Errorf("operator disable: read removal: %w", err)
		}
		if pendingRemoval {
			return store.ErrUserPending
		}
		if _, err := conn.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = ? AND subject = ?`, tenantID, subject); err != nil {
			return fmt.Errorf("operator disable: %w", err)
		}
		del, err := conn.ExecContext(ctx, `DELETE FROM auth_removals WHERE tenant_id = ? AND subject = ? AND reason IN (`+store.SCIMRemovalReasons+`)`,
			tenantID, subject)
		if err != nil {
			return fmt.Errorf("operator disable: removal: %w", err)
		}
		n, err := del.RowsAffected()
		disowned = n > 0
		return err
	})
	return disowned, err
}

// SCIMUserCreate implements store.SCIMStore: the shared statements, inside
// an IMMEDIATE transaction (the count and the insert as one write).
func (s *Store) SCIMUserCreate(ctx context.Context, row store.SCIMUserRow, since time.Time, ceiling int) error {
	return s.immediateTx(ctx, func(conn *sql.Conn) error {
		return sqlshared.SCIMUserCreate(ctx, sqlshared.SQLiteConn(conn), row, since, ceiling)
	})
}
