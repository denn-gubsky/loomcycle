package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// auth_lifecycle.go — RFC FA P4: the external gates' replay cache and the
// removal stage machine. Sibling of internal/store/postgres/auth_lifecycle.go;
// the contract is on store.AuthLifecycleStore and asserted by
// internal/store/storetest. Unix-nano INTEGER timestamps, NULL = unset.

// AuthAssertionUseOnce implements store.AuthLifecycleStore. A single
// statement: the write lock SQLite takes for it is what makes one of two
// racing callers the winner.
func (s *Store) AuthAssertionUseOnce(ctx context.Context, gate, jti string, expiresAt, now time.Time) (bool, error) {
	if gate == "" || jti == "" {
		return false, fmt.Errorf("auth assertion: gate and jti are required")
	}
	var got string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO auth_assertion_jtis (gate, jti, expires_at) VALUES (?1, ?2, ?3)
		ON CONFLICT (gate, jti) DO UPDATE SET expires_at = excluded.expires_at
			WHERE auth_assertion_jtis.expires_at < ?4
		RETURNING jti`, gate, jti, expiresAt.UnixNano(), now.UnixNano()).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth assertion use: %w", err)
	}
	return true, nil
}

// AuthAssertionDeleteExpired implements store.AuthLifecycleStore.
func (s *Store) AuthAssertionDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_assertion_jtis WHERE expires_at < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("auth assertion sweep: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const removalCols = `tenant_id, subject, gate, reason, disabled_at, unbind_due_at, erase_due_at, unbound_at, erased_at, created_by`

func scanRemoval(row gateScanner) (store.AuthRemovalRow, error) {
	var r store.AuthRemovalRow
	var disabled, unbindDue int64
	var eraseDue, unbound, erased sql.NullInt64
	if err := row.Scan(&r.TenantID, &r.Subject, &r.Gate, &r.Reason, &disabled, &unbindDue,
		&eraseDue, &unbound, &erased, &r.CreatedBy); err != nil {
		return store.AuthRemovalRow{}, err
	}
	r.DisabledAt, r.UnbindDueAt = time.Unix(0, disabled).UTC(), time.Unix(0, unbindDue).UTC()
	r.EraseDueAt, r.UnboundAt, r.ErasedAt = gateTime(eraseDue), gateTime(unbound), gateTime(erased)
	return r, nil
}

// AuthRemovalStart implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalStart(ctx context.Context, row store.AuthRemovalRow) (bool, error) {
	if row.Subject == "" || row.Reason == "" || row.DisabledAt.IsZero() || row.UnbindDueAt.IsZero() {
		return false, fmt.Errorf("auth removal: subject, reason, disabled_at and unbind_due_at are required")
	}
	if row.UnbindDueAt.Before(row.DisabledAt) || (!row.EraseDueAt.IsZero() && row.EraseDueAt.Before(row.UnbindDueAt)) {
		return false, fmt.Errorf("auth removal: stages must be due in order (disable <= unbind <= erase)")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_removals (`+removalCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
		ON CONFLICT (tenant_id, subject) DO UPDATE SET gate = excluded.gate, reason = excluded.reason,
			created_by = excluded.created_by
		WHERE auth_removals.reason IN ('scim', 'scim_pending') AND excluded.reason NOT IN ('scim', 'scim_pending')`,
		row.TenantID, row.Subject, row.Gate, row.Reason, row.DisabledAt.UnixNano(), row.UnbindDueAt.UnixNano(),
		gateNanos(row.EraseDueAt), row.CreatedBy)
	if err != nil {
		return false, fmt.Errorf("auth removal start: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AuthRemovalGet implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalGet(ctx context.Context, tenantID, subject string) (store.AuthRemovalRow, error) {
	r, err := scanRemoval(s.db.QueryRowContext(ctx,
		`SELECT `+removalCols+` FROM auth_removals WHERE tenant_id = ? AND subject = ?`, tenantID, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuthRemovalRow{}, &store.ErrNotFound{Kind: "auth_removal", ID: subject}
	}
	if err != nil {
		return store.AuthRemovalRow{}, fmt.Errorf("auth removal get: %w", err)
	}
	return r, nil
}

func (s *Store) listRemovals(ctx context.Context, q string, args ...any) ([]store.AuthRemovalRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	return s.listRemovals(ctx, `SELECT `+removalCols+` FROM auth_removals WHERE tenant_id = ?
		ORDER BY disabled_at, subject`, tenantID)
}

// AuthRemovalListAllTenants implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalListAllTenants(ctx context.Context) ([]store.AuthRemovalRow, error) {
	return s.listRemovals(ctx, `SELECT `+removalCols+` FROM auth_removals ORDER BY disabled_at, tenant_id, subject`)
}

// AuthRemovalDue implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalDue(ctx context.Context, now time.Time, limit int) ([]store.AuthRemovalRow, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.listRemovals(ctx, `SELECT `+removalCols+` FROM auth_removals
		WHERE (unbound_at IS NULL AND unbind_due_at <= ?1)
		   OR (unbound_at IS NOT NULL AND erased_at IS NULL AND erase_due_at IS NOT NULL AND erase_due_at <= ?1)
		ORDER BY disabled_at, tenant_id, subject LIMIT ?2`, now.UnixNano(), limit)
}

func (s *Store) removalUpdate(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AuthRemovalMarkUnbound implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalMarkUnbound(ctx context.Context, tenantID, subject string, at time.Time) (bool, error) {
	ok, err := s.removalUpdate(ctx, `UPDATE auth_removals SET unbound_at = ?
		WHERE tenant_id = ? AND subject = ? AND unbound_at IS NULL`, at.UnixNano(), tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal unbound: %w", err)
	}
	return ok, nil
}

// AuthRemovalMarkErased implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalMarkErased(ctx context.Context, tenantID, subject string, at time.Time) (bool, error) {
	ok, err := s.removalUpdate(ctx, `UPDATE auth_removals SET erased_at = ?
		WHERE tenant_id = ? AND subject = ? AND unbound_at IS NOT NULL AND erased_at IS NULL`, at.UnixNano(), tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal erased: %w", err)
	}
	return ok, nil
}

// AuthRemovalDelete implements store.AuthLifecycleStore.
func (s *Store) AuthRemovalDelete(ctx context.Context, tenantID, subject string) (bool, error) {
	ok, err := s.removalUpdate(ctx, `DELETE FROM auth_removals WHERE tenant_id = ? AND subject = ?`, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("auth removal delete: %w", err)
	}
	return ok, nil
}

// AuthDormantCandidates implements store.AuthLifecycleStore.
func (s *Store) AuthDormantCandidates(ctx context.Context, before time.Time, limit int) ([]store.UserRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.tenant_id, u.subject, u.display_name, u.access_mode, u.status, u.created_at, u.created_by
		FROM users u
		JOIN user_identities i ON i.tenant_id = u.tenant_id AND i.subject = u.subject
		WHERE u.status = 'active'
		  AND NOT EXISTS (SELECT 1 FROM auth_removals r WHERE r.tenant_id = u.tenant_id AND r.subject = u.subject)
		GROUP BY u.tenant_id, u.subject
		HAVING MAX(COALESCE(i.last_login_at, i.created_at)) < ?
		ORDER BY u.tenant_id, u.subject
		LIMIT ?`, before.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("auth dormant candidates: %w", err)
	}
	defer rows.Close()
	out := []store.UserRow{}
	for rows.Next() {
		var r store.UserRow
		var created int64
		var createdBy sql.NullString
		if err := rows.Scan(&r.TenantID, &r.Subject, &r.DisplayName, &r.AccessMode, &r.Status, &created, &createdBy); err != nil {
			return nil, fmt.Errorf("auth dormant candidates scan: %w", err)
		}
		r.CreatedAt = time.Unix(0, created).UTC()
		r.CreatedBy = createdBy.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserIdentityListGate implements store.AuthIdentityStore.
func (s *Store) UserIdentityListGate(ctx context.Context, gate string) ([]store.UserIdentityRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+identityCols+` FROM user_identities WHERE gate = ?
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
	res, err := s.db.ExecContext(ctx, `UPDATE ui_sessions SET revoked_at = ?
		WHERE tenant_id = ? AND subject = ? AND revoked_at IS NULL`, at.UnixNano(), tenantID, subject)
	if err != nil {
		return 0, fmt.Errorf("ui session revoke subject: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
