package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// auth_lifecycle.go — RFC FA P4: the external gates' replay cache and the
// removal stage machine (migration 0115). Sibling of
// internal/store/sqlite/auth_lifecycle.go; the contract is on
// store.AuthLifecycleStore and asserted by internal/store/storetest.

// AuthAssertionUseOnce implements store.AuthLifecycleStore. One statement, so
// two replicas racing on one jti serialise on the primary key: exactly one
// inserts (or replaces an expired row) and sees a row back.
func (s *Store) AuthAssertionUseOnce(ctx context.Context, gate, jti string, expiresAt, now time.Time) (bool, error) {
	if gate == "" || jti == "" {
		return false, fmt.Errorf("auth assertion: gate and jti are required")
	}
	var got string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO auth_assertion_jtis (gate, jti, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (gate, jti) DO UPDATE SET expires_at = EXCLUDED.expires_at
			WHERE auth_assertion_jtis.expires_at < $4
		RETURNING jti`, gate, jti, expiresAt.UTC(), now.UTC()).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth assertion use: %w", err)
	}
	return true, nil
}

// AuthAssertionDeleteExpired implements store.AuthLifecycleStore.
func (s *Store) AuthAssertionDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_assertion_jtis WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("auth assertion sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

const removalCols = `tenant_id, subject, gate, reason, disabled_at, unbind_due_at, erase_due_at, unbound_at, erased_at, created_by`

// removalReadCols is what a read returns: removalCols, then the claimed
// stage and the prior status (written by their own statements).
const removalReadCols = removalCols + `, stage, prior_status`

func scanRemoval(row pgx.Row) (store.AuthRemovalRow, error) {
	var r store.AuthRemovalRow
	var eraseDue, unbound, erased *time.Time
	if err := row.Scan(&r.TenantID, &r.Subject, &r.Gate, &r.Reason, &r.DisabledAt, &r.UnbindDueAt,
		&eraseDue, &unbound, &erased, &r.CreatedBy, &r.Stage, &r.PriorStatus); err != nil {
		return store.AuthRemovalRow{}, err
	}
	r.DisabledAt, r.UnbindDueAt = r.DisabledAt.UTC(), r.UnbindDueAt.UTC()
	for _, p := range []struct {
		src *time.Time
		dst *time.Time
	}{{eraseDue, &r.EraseDueAt}, {unbound, &r.UnboundAt}, {erased, &r.ErasedAt}} {
		if p.src != nil {
			*p.dst = p.src.UTC()
		}
	}
	return r, nil
}

func pgNullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// AuthRemovalStart implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalStart(ctx context.Context, row store.AuthRemovalRow) (bool, error) {
	if row.Subject == "" || row.Reason == "" || row.DisabledAt.IsZero() || row.UnbindDueAt.IsZero() {
		return false, fmt.Errorf("auth removal: subject, reason, disabled_at and unbind_due_at are required")
	}
	if row.UnbindDueAt.Before(row.DisabledAt) || (!row.EraseDueAt.IsZero() && row.EraseDueAt.Before(row.UnbindDueAt)) {
		return false, fmt.Errorf("auth removal: stages must be due in order (disable <= unbind <= erase)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The user row first: every removal write locks it before the removal,
	// so none of them can deadlock another.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`, row.TenantID, row.Subject).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, &store.ErrNotFound{Kind: "user", ID: row.Subject}
	}
	if err != nil {
		return false, fmt.Errorf("auth removal start: read user: %w", err)
	}
	prior := ""
	if status != "disabled" {
		prior = status
		// A row behind a user who is not disabled is no longer in force.
		if _, err := tx.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2`, row.TenantID, row.Subject); err != nil {
			return false, fmt.Errorf("auth removal start: drop a stale removal: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE tenant_id = $1 AND subject = $2`, row.TenantID, row.Subject); err != nil {
			return false, fmt.Errorf("auth removal start: disable: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO auth_removals (`+removalCols+`, prior_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, NULL, $8, $9)
		ON CONFLICT (tenant_id, subject) DO UPDATE SET gate = excluded.gate, reason = excluded.reason,
			created_by = excluded.created_by,
			prior_status = CASE WHEN auth_removals.reason IN ('`+store.RemovalReasonSCIMPending+`', '`+store.RemovalReasonSCIMReactivatingPending+`')
				THEN '`+store.UserStatusPending+`' ELSE auth_removals.prior_status END
		WHERE auth_removals.reason IN (`+store.SCIMRemovalReasons+`) AND excluded.reason NOT IN (`+store.SCIMRemovalReasons+`)`,
		row.TenantID, row.Subject, row.Gate, row.Reason, row.DisabledAt.UTC(), row.UnbindDueAt.UTC(),
		pgNullTime(row.EraseDueAt), row.CreatedBy, prior)
	if err != nil {
		return false, fmt.Errorf("auth removal start: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// AuthRemovalClaimStage implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalClaimStage(ctx context.Context, tenantID, subject, stage string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`, tenantID, subject).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth removal claim: read user: %w", err)
	}
	if status != "disabled" {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE auth_removals SET stage = $3
		WHERE tenant_id = $1 AND subject = $2 AND (stage = '' OR stage = $3)
		  AND reason NOT IN ('`+store.RemovalReasonSCIMReactivating+`', '`+store.RemovalReasonSCIMReactivatingPending+`')`,
		tenantID, subject, stage)
	if err != nil {
		return false, fmt.Errorf("auth removal claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// AuthRemovalDeleteIdle implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalDeleteIdle(ctx context.Context, tenantID, subject string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2 AND stage = '' AND erased_at IS NULL`,
		tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AuthRemovalGet implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalGet(ctx context.Context, tenantID, subject string) (store.AuthRemovalRow, error) {
	r, err := scanRemoval(s.pool.QueryRow(ctx,
		`SELECT `+removalReadCols+` FROM auth_removals WHERE tenant_id = $1 AND subject = $2`, tenantID, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AuthRemovalRow{}, &store.ErrNotFound{Kind: "auth_removal", ID: subject}
	}
	if err != nil {
		return store.AuthRemovalRow{}, fmt.Errorf("auth removal get: %w", err)
	}
	return r, nil
}

func (s *Store) listRemovals(ctx context.Context, q string, args ...any) ([]store.AuthRemovalRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("auth removal list: %w", err)
	}
	defer rows.Close()
	out := []store.AuthRemovalRow{}
	for rows.Next() {
		r, err := scanRemoval(rows)
		if err != nil {
			return nil, fmt.Errorf("auth removal scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuthRemovalList implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalList(ctx context.Context, tenantID string) ([]store.AuthRemovalRow, error) {
	return s.listRemovals(ctx, `SELECT `+removalReadCols+` FROM auth_removals WHERE tenant_id = $1
		ORDER BY disabled_at, subject`, tenantID)
}

// AuthRemovalListAllTenants implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalListAllTenants(ctx context.Context) ([]store.AuthRemovalRow, error) {
	return s.listRemovals(ctx, `SELECT `+removalReadCols+` FROM auth_removals ORDER BY disabled_at, tenant_id, subject`)
}

// AuthRemovalDue implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalDue(ctx context.Context, now time.Time, limit int) ([]store.AuthRemovalRow, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.listRemovals(ctx, `SELECT `+removalReadCols+` FROM auth_removals
		WHERE (unbound_at IS NULL AND unbind_due_at <= $1)
		   OR (unbound_at IS NOT NULL AND erased_at IS NULL AND erase_due_at IS NOT NULL AND erase_due_at <= $1)
		ORDER BY disabled_at, tenant_id, subject LIMIT $2`, now.UTC(), limit)
}

// AuthRemovalMarkUnbound implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalMarkUnbound(ctx context.Context, tenantID, subject string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE auth_removals SET unbound_at = $1, stage = ''
		WHERE tenant_id = $2 AND subject = $3 AND unbound_at IS NULL`, at.UTC(), tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal unbound: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AuthRemovalMarkErased implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalMarkErased(ctx context.Context, tenantID, subject string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE auth_removals SET erased_at = $1, stage = ''
		WHERE tenant_id = $2 AND subject = $3 AND unbound_at IS NOT NULL AND erased_at IS NULL`, at.UTC(), tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal erased: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AuthRemovalDelete implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalDelete(ctx context.Context, tenantID, subject string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_removals WHERE tenant_id = $1 AND subject = $2`, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AuthDormantCandidates implements store.AuthLifecycleStore.
func (s *Store) AuthDormantCandidates(ctx context.Context, before time.Time, limit int) ([]store.UserRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.tenant_id, u.subject, u.display_name, u.access_mode, u.status, u.created_at, u.created_by
		FROM users u
		JOIN user_identities i ON i.tenant_id = u.tenant_id AND i.subject = u.subject
		WHERE u.status = 'active'
		  AND NOT EXISTS (SELECT 1 FROM auth_removals r WHERE r.tenant_id = u.tenant_id AND r.subject = u.subject)
		GROUP BY u.tenant_id, u.subject, u.display_name, u.access_mode, u.status, u.created_at, u.created_by
		HAVING MAX(COALESCE(i.last_login_at, i.created_at)) < $1
		ORDER BY u.tenant_id, u.subject
		LIMIT $2`, before.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("auth dormant candidates: %w", err)
	}
	defer rows.Close()
	out := []store.UserRow{}
	for rows.Next() {
		var r store.UserRow
		var createdBy *string
		if err := rows.Scan(&r.TenantID, &r.Subject, &r.DisplayName, &r.AccessMode, &r.Status, &r.CreatedAt, &createdBy); err != nil {
			return nil, fmt.Errorf("auth dormant candidates scan: %w", err)
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if createdBy != nil {
			r.CreatedBy = *createdBy
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserIdentityListGate implements store.AuthIdentityStore.
func (s *Store) UserIdentityListGate(ctx context.Context, gate string) ([]store.UserIdentityRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+identityCols+` FROM user_identities WHERE gate = $1
		ORDER BY tenant_id, subject`, gate)
	if err != nil {
		return nil, fmt.Errorf("user identity list gate: %w", err)
	}
	defer rows.Close()
	out := []store.UserIdentityRow{}
	for rows.Next() {
		r, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("user identity list gate scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UISessionRevokeSubject implements store.UISessionStore.
func (s *Store) UISessionRevokeSubject(ctx context.Context, tenantID, subject string, at time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE ui_sessions SET revoked_at = $1
		WHERE tenant_id = $2 AND subject = $3 AND revoked_at IS NULL`, at.UTC(), tenantID, subject)
	if err != nil {
		return 0, fmt.Errorf("ui session revoke subject: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
