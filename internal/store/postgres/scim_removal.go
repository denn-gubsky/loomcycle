package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// SCIMRemoveUser implements store.SCIMStore (the state machine is on the
// interface). A transaction-scoped advisory lock per (gate, tenant)
// serialises the count and the writes across every replica; the user row is
// locked too, so an operator's concurrent change of it waits or wins whole.
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
	// Disabled already, by anyone: that disable (and any removal row) stands.
	if status == "disabled" {
		return false, nil
	}
	// The ceiling before any write. A stale row of this subject is not
	// counted: it is replaced below, not added to.
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM auth_removals WHERE gate = $1 AND tenant_id = $2 AND disabled_at >= $3 AND subject <> $4`,
		row.Gate, row.TenantID, since.UTC(), row.Subject).Scan(&n); err != nil {
		return false, fmt.Errorf("scim removal: count: %w", err)
	}
	if n >= ceiling {
		return false, store.ErrSCIMRemovalCeiling
	}
	// A removal row behind a user who is not disabled is no longer in force:
	// it goes, with its deadlines, and a fresh one replaces it.
	if _, err := tx.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2`, row.TenantID, row.Subject); err != nil {
		return false, fmt.Errorf("scim removal: drop a stale removal: %w", err)
	}
	reason := store.RemovalReasonSCIM
	if status == store.UserStatusPending {
		reason = store.RemovalReasonSCIMPending
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

// claimedReason is the reactivating reason a SCIM reason claims to.
func claimedReason(reason string) string {
	if reason == store.RemovalReasonSCIMPending || reason == store.RemovalReasonSCIMReactivatingPending {
		return store.RemovalReasonSCIMReactivatingPending
	}
	return store.RemovalReasonSCIMReactivating
}

// scimLock takes the transaction-scoped advisory lock that serialises one
// kind of SCIM write per (gate, tenant) across every replica. It is always
// taken before any row lock.
func scimLock(ctx context.Context, tx pgx.Tx, kind, gate, tenantID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('loomcycle/scim-' || $1::text || '/' || $2::text || '/' || $3::text, 0))`,
		kind, gate, tenantID)
	return err
}

// lockUserStatus locks the user row and returns its status: every removal
// write locks the user before the removal, so none can deadlock another.
func lockUserStatus(ctx context.Context, tx pgx.Tx, tenantID, subject string) (string, bool, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM users WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`, tenantID, subject).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return status, err == nil, err
}

// SCIMReactivateClaim implements store.SCIMStore.
func (s *Store) SCIMReactivateClaim(ctx context.Context, gate, tenantID, subject string, at, since time.Time, ceiling int) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := scimLock(ctx, tx, "reactivation", gate, tenantID); err != nil {
		return false, fmt.Errorf("scim reactivate: lock: %w", err)
	}
	status, found, err := lockUserStatus(ctx, tx, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("scim reactivate: read user: %w", err)
	}
	if !found || status != "disabled" {
		return false, nil
	}
	var rowGate, reason, stage string
	var erased *time.Time
	err = tx.QueryRow(ctx, `SELECT gate, reason, stage, erased_at FROM auth_removals WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`,
		tenantID, subject).Scan(&rowGate, &reason, &stage, &erased)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("scim reactivate: read removal: %w", err)
	}
	if rowGate != gate || !store.SCIMRemovalReason(reason) {
		return false, nil
	}
	if stage != "" || erased != nil {
		return false, store.ErrRemovalBusy
	}
	if claimedReason(reason) != reason { // a new claim, not a retry
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM scim_users WHERE gate = $1 AND tenant_id = $2 AND reactivated_at >= $3`,
			gate, tenantID, since.UTC()).Scan(&n); err != nil {
			return false, fmt.Errorf("scim reactivate: count: %w", err)
		}
		if n >= ceiling {
			return false, store.ErrSCIMReactivationCeiling
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_removals SET reason = $3 WHERE tenant_id = $1 AND subject = $2`,
		tenantID, subject, claimedReason(reason)); err != nil {
		return false, fmt.Errorf("scim reactivate: claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE scim_users SET reactivated_at = $4 WHERE gate = $1 AND tenant_id = $2 AND subject = $3 AND deleted_at IS NULL`,
		gate, tenantID, subject, at.UTC()); err != nil {
		return false, fmt.Errorf("scim reactivate: stamp: %w", err)
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
	status, found, err := lockUserStatus(ctx, tx, tenantID, subject)
	if err != nil {
		return "", false, fmt.Errorf("scim reactivate: read user: %w", err)
	}
	if !found || status != "disabled" {
		return "", false, nil
	}
	var rowGate, reason string
	err = tx.QueryRow(ctx, `SELECT gate, reason FROM auth_removals WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`,
		tenantID, subject).Scan(&rowGate, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("scim reactivate: read removal: %w", err)
	}
	restored := ""
	switch {
	case rowGate != gate:
		return "", false, nil
	case reason == store.RemovalReasonSCIMReactivating:
		restored = "active"
	case reason == store.RemovalReasonSCIMReactivatingPending:
		restored = store.UserStatusPending
	default:
		return "", false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status = $3 WHERE tenant_id = $1 AND subject = $2`, tenantID, subject, restored); err != nil {
		return "", false, fmt.Errorf("scim reactivate: user: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2`, tenantID, subject); err != nil {
		return "", false, fmt.Errorf("scim reactivate: removal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return restored, true, nil
}

// OperatorDisableUser implements store.SCIMStore.
func (s *Store) OperatorDisableUser(ctx context.Context, tenantID, subject string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	status, found, err := lockUserStatus(ctx, tx, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("operator disable: read user: %w", err)
	}
	if !found {
		return false, &store.ErrNotFound{Kind: "user", ID: subject}
	}
	if status == store.UserStatusPending {
		return false, store.ErrUserPending
	}
	var pendingRemoval bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM auth_removals WHERE tenant_id = $1 AND subject = $2
		AND reason IN ('`+store.RemovalReasonSCIMPending+`', '`+store.RemovalReasonSCIMReactivatingPending+`'))`,
		tenantID, subject).Scan(&pendingRemoval); err != nil {
		return false, fmt.Errorf("operator disable: read removal: %w", err)
	}
	if pendingRemoval {
		return false, store.ErrUserPending
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = $1 AND subject = $2`, tenantID, subject); err != nil {
		return false, fmt.Errorf("operator disable: %w", err)
	}
	del, err := tx.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2 AND reason IN (`+store.SCIMRemovalReasons+`)`,
		tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("operator disable: removal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return del.RowsAffected() > 0, nil
}

// SCIMUserCreate implements store.SCIMStore: the shared statements, inside
// a transaction holding the gate's create lock.
func (s *Store) SCIMUserCreate(ctx context.Context, row store.SCIMUserRow, since time.Time, ceiling int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := scimLock(ctx, tx, "create", row.Gate, row.TenantID); err != nil {
		return fmt.Errorf("scim user create: lock: %w", err)
	}
	if err := sqlshared.SCIMUserCreate(ctx, sqlshared.PostgresTx(tx), row, since, ceiling); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
