package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// users.go — RFC BX P2a tenant-owned user-identity CRUD against the `users`
// table. Sibling of internal/store/postgres/users.go; both keep the same
// contract (see internal/store/storetest testUserCRUD / testUserTenantIsolation).

// UserCreate inserts a first-class identity row. Returns
// *store.ErrConflict{Kind:"user", ID:subject} on a (tenant, subject) PK
// violation (modernc.org/sqlite surfaces it as "UNIQUE constraint failed").
func (s *Store) UserCreate(ctx context.Context, row store.UserRow) error {
	if !store.ValidUserAccessMode(row.AccessMode) {
		return fmt.Errorf("user create: access_mode must be one of tenant|isolated, got %q", row.AccessMode)
	}
	if !store.ValidUserStatus(row.Status) {
		return fmt.Errorf("user create: status must be one of active|disabled, got %q", row.Status)
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (
			tenant_id, subject, display_name, access_mode, status, created_at, created_by
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		row.TenantID, row.Subject, row.DisplayName, row.AccessMode, row.Status,
		createdAt.UnixNano(), row.CreatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return &store.ErrConflict{Kind: "user", ID: row.Subject}
		}
		return fmt.Errorf("user create: %w", err)
	}
	return nil
}

// UserGet returns one identity row. Returns *store.ErrNotFound{Kind:"user"}
// when the (tenantID, subject) row is absent.
func (s *Store) UserGet(ctx context.Context, tenantID, subject string) (store.UserRow, error) {
	var r store.UserRow
	var createdNano int64
	var createdBy sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT tenant_id, subject, display_name, access_mode, status, created_at, created_by
		FROM users
		WHERE tenant_id = ? AND subject = ?
	`, tenantID, subject).Scan(
		&r.TenantID, &r.Subject, &r.DisplayName, &r.AccessMode, &r.Status, &createdNano, &createdBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return store.UserRow{}, &store.ErrNotFound{Kind: "user", ID: subject}
	}
	if err != nil {
		return store.UserRow{}, fmt.Errorf("user get: %w", err)
	}
	r.CreatedAt = time.Unix(0, createdNano).UTC()
	r.CreatedBy = createdBy.String
	return r, nil
}

// UserList returns every identity row for tenantID ordered by
// (tenant_id, subject). tenantID "" returns ALL tenants' rows (super-admin).
// Empty slice (not nil) when none match.
func (s *Store) UserList(ctx context.Context, tenantID string) ([]store.UserRow, error) {
	q := `
		SELECT tenant_id, subject, display_name, access_mode, status, created_at, created_by
		FROM users`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY tenant_id, subject`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("user list: %w", err)
	}
	defer rows.Close()
	out := []store.UserRow{}
	for rows.Next() {
		var r store.UserRow
		var createdNano int64
		var createdBy sql.NullString
		if err := rows.Scan(
			&r.TenantID, &r.Subject, &r.DisplayName, &r.AccessMode, &r.Status, &createdNano, &createdBy,
		); err != nil {
			return nil, fmt.Errorf("user list scan: %w", err)
		}
		r.CreatedAt = time.Unix(0, createdNano).UTC()
		r.CreatedBy = createdBy.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserUpdate patches mutable fields. Nil patch fields leave the corresponding
// column unchanged. Returns *store.ErrNotFound{Kind:"user"} when the row is
// absent, and rejects a bad access_mode / status.
func (s *Store) UserUpdate(ctx context.Context, tenantID, subject string, patch store.UserPatch) error {
	if patch.AccessMode != nil && !store.ValidUserAccessMode(*patch.AccessMode) {
		return fmt.Errorf("user update: access_mode must be one of tenant|isolated, got %q", *patch.AccessMode)
	}
	if patch.Status != nil && !store.ValidUserStatus(*patch.Status) {
		return fmt.Errorf("user update: status must be one of active|disabled, got %q", *patch.Status)
	}
	sets := []string{}
	args := []any{}
	if patch.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *patch.DisplayName)
	}
	if patch.AccessMode != nil {
		sets = append(sets, "access_mode = ?")
		args = append(args, *patch.AccessMode)
	}
	if patch.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *patch.Status)
	}
	if len(sets) == 0 {
		// Nothing to update — verify existence and return.
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE tenant_id = ? AND subject = ?`, tenantID, subject).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &store.ErrNotFound{Kind: "user", ID: subject}
			}
			return fmt.Errorf("user update existence: %w", err)
		}
		return nil
	}
	args = append(args, tenantID, subject)
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE tenant_id = ? AND subject = ?`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("user update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("user update rows-affected: %w", err)
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "user", ID: subject}
	}
	return nil
}

// UserDelete hard-deletes one identity row and reports whether a row was
// removed. Owned data (runs / sessions / memory) has no FK here and stays.
func (s *Store) UserDelete(ctx context.Context, tenantID, subject string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE tenant_id = ? AND subject = ?`, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("user delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("user delete rows-affected: %w", err)
	}
	return n > 0, nil
}
