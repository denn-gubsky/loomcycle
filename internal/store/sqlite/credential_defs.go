package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// CredentialDef* implements the RFC AR secure credential store on SQLite. Flat
// (tenant_id, scope, scope_id, name) table; `definition` holds only sealed
// ciphertext (inline backend) or an external-backend pointer — never a
// plaintext secret. See internal/store/store.go for the contract.

func (s *Store) CredentialDefPut(ctx context.Context, row store.CredentialDefRow) (store.CredentialDefRow, error) {
	now := time.Now()
	var expires sql.NullInt64
	if row.ExpiresAt != nil {
		expires = sql.NullInt64{Int64: row.ExpiresAt.UnixNano(), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO credential_defs (tenant_id, scope, scope_id, name, backend, definition, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, scope, scope_id, name) DO UPDATE SET
		     backend    = excluded.backend,
		     definition = excluded.definition,
		     expires_at = excluded.expires_at,
		     updated_at = excluded.updated_at`,
		row.TenantID, row.Scope, row.ScopeID, row.Name, row.Backend, string(row.Definition),
		expires, now.UnixNano(), now.UnixNano(),
	); err != nil {
		return store.CredentialDefRow{}, err
	}
	// Re-read so created_at reflects the original insert on an update.
	return s.CredentialDefGet(ctx, row.TenantID, row.Scope, row.ScopeID, row.Name)
}

func (s *Store) CredentialDefGet(ctx context.Context, tenantID, scope, scopeID, name string) (store.CredentialDefRow, error) {
	var (
		out        store.CredentialDefRow
		definition string
		expires    sql.NullInt64
		createdAt  int64
		updatedAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, scope, scope_id, name, backend, definition, expires_at, created_at, updated_at
		 FROM credential_defs WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND name = ?`,
		tenantID, scope, scopeID, name,
	).Scan(&out.TenantID, &out.Scope, &out.ScopeID, &out.Name, &out.Backend, &definition, &expires, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return store.CredentialDefRow{}, &store.ErrNotFound{Kind: "credential_def", ID: name}
	}
	if err != nil {
		return store.CredentialDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	if expires.Valid {
		t := time.Unix(0, expires.Int64)
		out.ExpiresAt = &t
	}
	out.CreatedAt = time.Unix(0, createdAt)
	out.UpdatedAt = time.Unix(0, updatedAt)
	return out, nil
}

func (s *Store) CredentialDefList(ctx context.Context, tenantID, scope, scopeID string) ([]store.CredentialDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, scope, scope_id, name, backend, definition, expires_at, created_at, updated_at
		 FROM credential_defs WHERE tenant_id = ? AND scope = ? AND scope_id = ? ORDER BY name`,
		tenantID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.CredentialDefRow
	for rows.Next() {
		var (
			r          store.CredentialDefRow
			definition string
			expires    sql.NullInt64
			createdAt  int64
			updatedAt  int64
		)
		if err := rows.Scan(&r.TenantID, &r.Scope, &r.ScopeID, &r.Name, &r.Backend, &definition, &expires, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		if expires.Valid {
			t := time.Unix(0, expires.Int64)
			r.ExpiresAt = &t
		}
		r.CreatedAt = time.Unix(0, createdAt)
		r.UpdatedAt = time.Unix(0, updatedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CredentialDefDelete(ctx context.Context, tenantID, scope, scopeID, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM credential_defs WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND name = ?`,
		tenantID, scope, scopeID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
