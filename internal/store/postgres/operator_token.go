package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	rotated_from, retired_at, expires_at, restrictions FROM operator_token_defs`

func scanOperatorTokenDef(row pgx.Row) (store.OperatorTokenDefRow, error) {
	var (
		out        store.OperatorTokenDefRow
		scopesJSON string
		retiredAt  *time.Time
		expiresAt  *time.Time
		restrict   sql.NullString
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
		&rotated, &retiredAt, &expiresAt, &restrict,
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
	if expiresAt != nil {
		out.ExpiresAt = *expiresAt
	}
	out.RestrictionsJSON = restrict.String
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
	var retired, expires *time.Time
	if !row.RetiredAt.IsZero() {
		t := row.RetiredAt.UTC()
		retired = &t
	}
	if !row.ExpiresAt.IsZero() {
		t := row.ExpiresAt.UTC()
		expires = &t
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO operator_token_defs (
			def_id, name, tenant_id, subject, token_hash, allowed_scopes,
			created_at, created_by_agent_id, created_by_run_id, rotated_from, retired_at, expires_at, restrictions
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13::jsonb)`,
		row.DefID, row.Name, row.TenantID, row.Subject, row.TokenHash, string(scopesJSON),
		row.CreatedAt, nullableString(row.CreatedByAgentID), nullableString(row.CreatedByRunID),
		nullableString(row.RotatedFrom), retired, expires, nullableString(row.RestrictionsJSON),
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
		operatorTokenDefSelect+` WHERE name = $1 AND retired_at IS NULL
			AND (expires_at IS NULL OR expires_at > now()) ORDER BY created_at DESC LIMIT 1`, name))
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

func (s *Store) OperatorTokenDefListBySubject(ctx context.Context, tenantID, subject string) ([]store.OperatorTokenDefRow, error) {
	rows, err := s.pool.Query(ctx,
		operatorTokenDefSelect+` WHERE tenant_id = $1 AND subject = $2 ORDER BY created_at DESC`, tenantID, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.OperatorTokenDefRow{}
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
	// One summary per (tenant, name): a per-user name repeats across tenants.
	// The subject is the newest row's, current_def_id the newest live row's;
	// the aggregate columns roll up the pair's whole history.
	rows, err := s.pool.Query(ctx, `
		SELECT name, tenant_id,
		       COUNT(*) AS token_count,
		       MAX(created_at) AS last_updated,
		       BOOL_OR(retired_at IS NULL AND (expires_at IS NULL OR expires_at > now())) AS has_current,
		       (ARRAY_AGG(subject ORDER BY created_at DESC))[1] AS subject,
		       (ARRAY_AGG(def_id ORDER BY created_at DESC)
		            FILTER (WHERE retired_at IS NULL AND (expires_at IS NULL OR expires_at > now())))[1] AS current_def_id
		FROM operator_token_defs GROUP BY tenant_id, name ORDER BY name, tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorTokenDefNameSummary
	for rows.Next() {
		var (
			sum     store.OperatorTokenDefNameSummary
			current *string
		)
		if err := rows.Scan(&sum.Name, &sum.TenantID, &sum.TokenCount, &sum.LastUpdated, &sum.HasCurrent, &sum.Subject, &current); err != nil {
			return nil, err
		}
		if current != nil {
			sum.CurrentDefID = *current
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

func (s *Store) OperatorTokenRehash(ctx context.Context, defID, oldHash, newHash string) (bool, error) {
	if defID == "" || oldHash == "" || newHash == "" {
		return false, fmt.Errorf("operator_token_def rehash: def_id + old and new hash required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE operator_token_defs SET token_hash = $1 WHERE def_id = $2 AND token_hash = $3`, newHash, defID, oldHash)
	if err != nil {
		return false, fmt.Errorf("operator_token_def rehash: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) OperatorTokenRehashAll(ctx context.Context, ops []store.OperatorTokenRehashOp) ([]bool, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	changed := make([]bool, len(ops))
	for i, op := range ops {
		if op.DefID == "" || op.OldHash == "" || op.NewHash == "" {
			return nil, fmt.Errorf("operator_token_def rehash: def_id + old and new hash required")
		}
		tag, err := tx.Exec(ctx,
			`UPDATE operator_token_defs SET token_hash = $1 WHERE def_id = $2 AND token_hash = $3`, op.NewHash, op.DefID, op.OldHash)
		if err != nil {
			return nil, fmt.Errorf("operator_token_def rehash %q: %w", op.DefID, err)
		}
		changed[i] = tag.RowsAffected() == 1
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return changed, nil
}

func (s *Store) OperatorTokenDefCountActiveAdmin(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM operator_token_defs
		WHERE (retired_at IS NULL OR retired_at > now())
		  AND (expires_at IS NULL OR expires_at > now())
		  AND allowed_scopes @> '["substrate:admin"]'::jsonb`).Scan(&n)
	return n, err
}

func (s *Store) OperatorTokenDefCountActiveAdminMatching(ctx context.Context, m store.TokenHashMatch) (int, error) {
	q := `SELECT COUNT(*) FROM operator_token_defs
		WHERE (retired_at IS NULL OR retired_at > now())
		  AND (expires_at IS NULL OR expires_at > now())
		  AND allowed_scopes @> '["substrate:admin"]'::jsonb`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	// starts_with-style comparison on left(), not LIKE: a prefix is matched
	// literally, whatever it contains.
	if len(m.Prefixes) > 0 {
		var ors []string
		for _, p := range m.Prefixes {
			ors = append(ors, `left(token_hash, `+arg(len(p))+`) = `+arg(p))
		}
		q += ` AND (` + strings.Join(ors, ` OR `) + `)`
	}
	for _, p := range m.ExcludePrefixes {
		q += ` AND left(token_hash, ` + arg(len(p)) + `) <> ` + arg(p)
	}
	var n int
	err := s.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}

func (s *Store) OperatorTokenDefDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cutoff := before.UTC()
	// See the SQLite twin: detach rotated_from first so the reference can
	// never refuse the delete.
	if _, err := tx.Exec(ctx, `
		UPDATE operator_token_defs SET rotated_from = NULL
		WHERE rotated_from IN (
			SELECT def_id FROM operator_token_defs WHERE expires_at IS NOT NULL AND expires_at < $1)`, cutoff); err != nil {
		return 0, err
	}
	// See the SQLite twin: the last row naming a tenant is kept, so the tenant
	// stays in the case-fold registry.
	tag, err := tx.Exec(ctx, `
		DELETE FROM operator_token_defs
		WHERE expires_at IS NOT NULL AND expires_at < $1
		  AND (EXISTS (SELECT 1 FROM operator_token_defs k
		               WHERE k.tenant_id = operator_token_defs.tenant_id
		                 AND (k.expires_at IS NULL OR k.expires_at >= $1))
		       OR def_id <> (SELECT MAX(k.def_id) FROM operator_token_defs k
		                     WHERE k.tenant_id = operator_token_defs.tenant_id))`, cutoff)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}
