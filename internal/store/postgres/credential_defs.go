package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// CredentialDef* implements the RFC AR secure credential store on Postgres.
// Flat (tenant_id, scope, scope_id, name) table; `definition` holds only sealed
// ciphertext (inline backend) or an external-backend pointer — never a
// plaintext secret. See internal/store/store.go for the contract.

func (s *Store) CredentialDefPut(ctx context.Context, row store.CredentialDefRow) (store.CredentialDefRow, error) {
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO credential_defs (tenant_id, scope, scope_id, name, backend, definition, expires_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)
		 ON CONFLICT (tenant_id, scope, scope_id, name) DO UPDATE SET
		     backend    = EXCLUDED.backend,
		     definition = EXCLUDED.definition,
		     expires_at = EXCLUDED.expires_at,
		     updated_at = EXCLUDED.updated_at`,
		row.TenantID, row.Scope, row.ScopeID, row.Name, row.Backend, string(row.Definition),
		row.ExpiresAt, now, now,
	); err != nil {
		return store.CredentialDefRow{}, err
	}
	return s.CredentialDefGet(ctx, row.TenantID, row.Scope, row.ScopeID, row.Name)
}

func (s *Store) CredentialDefGet(ctx context.Context, tenantID, scope, scopeID, name string) (store.CredentialDefRow, error) {
	var (
		out        store.CredentialDefRow
		definition string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, scope, scope_id, name, backend, definition::text, expires_at, created_at, updated_at
		 FROM credential_defs WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 AND name = $4`,
		tenantID, scope, scopeID, name,
	).Scan(&out.TenantID, &out.Scope, &out.ScopeID, &out.Name, &out.Backend, &definition, &out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CredentialDefRow{}, &store.ErrNotFound{Kind: "credential_def", ID: name}
	}
	if err != nil {
		return store.CredentialDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	return out, nil
}

func (s *Store) CredentialDefList(ctx context.Context, tenantID, scope, scopeID string) ([]store.CredentialDefRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, scope, scope_id, name, backend, definition::text, expires_at, created_at, updated_at
		 FROM credential_defs WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 ORDER BY name`,
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
		)
		if err := rows.Scan(&r.TenantID, &r.Scope, &r.ScopeID, &r.Name, &r.Backend, &definition, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CredentialDefDelete(ctx context.Context, tenantID, scope, scopeID, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM credential_defs WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 AND name = $4`,
		tenantID, scope, scopeID, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
