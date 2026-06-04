package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC L OSS multi-tenant authorization — OperatorTokenDef storage.
// Bearer tokens bound to an authoritative principal. NOT versioned or
// forkable: identity is token_hash (unique-indexed for the auth hot
// path), lifecycle is rotate→grace→retire. retired_at NULL = never
// retired; validity (NULL or now < retired_at) is decided by the auth
// layer, keeping the rotation-grace logic in one testable place.

const operatorTokenDefSelect = `SELECT def_id, name, tenant_id, subject, token_hash,
	allowed_scopes, created_at, created_by_agent_id, created_by_run_id,
	rotated_from, retired_at FROM operator_token_defs`

func scanOperatorTokenDef(row pgx.Row) (store.OperatorTokenDefRow, error) {
	var (
		out        store.OperatorTokenDefRow
		scopesJSON string
		retiredAt  *time.Time
		// created_by_agent_id / created_by_run_id / rotated_from are
		// NULLABLE columns (the CREATE path writes NULL via nullableString
		// when empty — e.g. a token minted through the HTTP/CLI admin path
		// has no run context, so created_by_run_id is NULL). They MUST scan
		// into sql.NullString; scanning a NULL into a plain *string fails
		// with "cannot scan NULL into *string", which on the auth hot path
		// (OperatorTokenDefGetByTokenHash) takes down token resolution for
		// every such token on Postgres. (SQLite already scanned these as
		// sql.NullString — this restores backend parity.)
		agentID sql.NullString
		runID   sql.NullString
		rotated sql.NullString
	)
	if err := row.Scan(
		&out.DefID, &out.Name, &out.TenantID, &out.Subject, &out.TokenHash,
		&scopesJSON, &out.CreatedAt, &agentID, &runID,
		&rotated, &retiredAt,
	); err != nil {
		return store.OperatorTokenDefRow{}, err
	}
	out.CreatedByAgentID = agentID.String
	out.CreatedByRunID = runID.String
	out.RotatedFrom = rotated.String
	if err := json.Unmarshal([]byte(scopesJSON), &out.AllowedScopes); err != nil {
		return store.OperatorTokenDefRow{}, fmt.Errorf("operator_token_def: decode allowed_scopes: %w", err)
	}
	if retiredAt != nil {
		out.RetiredAt = *retiredAt
	}
	return out, nil
}

func (s *Store) OperatorTokenDefCreate(ctx context.Context, row store.OperatorTokenDefRow) (store.OperatorTokenDefRow, error) {
	if row.DefID == "" || row.Name == "" || row.TenantID == "" || row.Subject == "" || row.TokenHash == "" {
		return store.OperatorTokenDefRow{}, fmt.Errorf("operator_token_def: def_id + name + tenant_id + subject + token_hash required")
	}
	if row.AllowedScopes == nil {
		row.AllowedScopes = []string{}
	}
	scopesJSON, err := json.Marshal(row.AllowedScopes)
	if err != nil {
		return store.OperatorTokenDefRow{}, fmt.Errorf("operator_token_def: encode allowed_scopes: %w", err)
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	var retired *time.Time
	if !row.RetiredAt.IsZero() {
		t := row.RetiredAt.UTC()
		retired = &t
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO operator_token_defs (
			def_id, name, tenant_id, subject, token_hash, allowed_scopes,
			created_at, created_by_agent_id, created_by_run_id, rotated_from, retired_at
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11)`,
		row.DefID, row.Name, row.TenantID, row.Subject, row.TokenHash, string(scopesJSON),
		row.CreatedAt, nullableString(row.CreatedByAgentID), nullableString(row.CreatedByRunID),
		nullableString(row.RotatedFrom), retired,
	); err != nil {
		return store.OperatorTokenDefRow{}, fmt.Errorf("operator_token_def insert: %w", err)
	}
	return row, nil
}

func (s *Store) OperatorTokenDefGet(ctx context.Context, defID string) (store.OperatorTokenDefRow, error) {
	row, err := scanOperatorTokenDef(s.pool.QueryRow(ctx, operatorTokenDefSelect+` WHERE def_id = $1`, defID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OperatorTokenDefRow{}, &store.ErrNotFound{Kind: "operator_token_def", ID: defID}
	}
	return row, err
}

func (s *Store) OperatorTokenDefGetByTokenHash(ctx context.Context, tokenHash string) (store.OperatorTokenDefRow, error) {
	row, err := scanOperatorTokenDef(s.pool.QueryRow(ctx, operatorTokenDefSelect+` WHERE token_hash = $1`, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OperatorTokenDefRow{}, &store.ErrNotFound{Kind: "operator_token_def", ID: "<hash>"}
	}
	return row, err
}

func (s *Store) OperatorTokenDefGetCurrentByName(ctx context.Context, name string) (store.OperatorTokenDefRow, error) {
	row, err := scanOperatorTokenDef(s.pool.QueryRow(ctx,
		operatorTokenDefSelect+` WHERE name = $1 AND retired_at IS NULL ORDER BY created_at DESC LIMIT 1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OperatorTokenDefRow{}, &store.ErrNotFound{Kind: "operator_token_def", ID: name}
	}
	return row, err
}

func (s *Store) OperatorTokenDefListByName(ctx context.Context, name string) ([]store.OperatorTokenDefRow, error) {
	rows, err := s.pool.Query(ctx, operatorTokenDefSelect+` WHERE name = $1 ORDER BY created_at DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorTokenDefRow
	for rows.Next() {
		r, err := scanOperatorTokenDef(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) OperatorTokenDefListNames(ctx context.Context) ([]store.OperatorTokenDefNameSummary, error) {
	// DISTINCT ON keeps the newest row per name for tenant/subject, while
	// the aggregate columns roll up the whole name's history.
	rows, err := s.pool.Query(ctx, `
		SELECT name,
		       COUNT(*) AS token_count,
		       MAX(created_at) AS last_updated,
		       BOOL_OR(retired_at IS NULL) AS has_current,
		       (ARRAY_AGG(tenant_id ORDER BY created_at DESC))[1] AS tenant_id,
		       (ARRAY_AGG(subject   ORDER BY created_at DESC))[1] AS subject
		FROM operator_token_defs GROUP BY name ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorTokenDefNameSummary
	for rows.Next() {
		var sum store.OperatorTokenDefNameSummary
		if err := rows.Scan(&sum.Name, &sum.TokenCount, &sum.LastUpdated, &sum.HasCurrent, &sum.TenantID, &sum.Subject); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

func (s *Store) OperatorTokenDefSetRetiredAt(ctx context.Context, defID string, retiredAt time.Time) error {
	var retired *time.Time
	if !retiredAt.IsZero() {
		t := retiredAt.UTC()
		retired = &t
	}
	tag, err := s.pool.Exec(ctx, `UPDATE operator_token_defs SET retired_at = $1 WHERE def_id = $2`, retired, defID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "operator_token_def", ID: defID}
	}
	return nil
}

func (s *Store) OperatorTokenDefCountActiveAdmin(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM operator_token_defs
		WHERE (retired_at IS NULL OR retired_at > now())
		  AND allowed_scopes @> '["substrate:admin"]'::jsonb`).Scan(&n)
	return n, err
}
