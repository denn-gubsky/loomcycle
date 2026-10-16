package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC L OSS multi-tenant authorization — OperatorTokenDef storage.
// Bearer tokens bound to an authoritative principal. NOT versioned or
// forkable: identity is token_hash, lifecycle is rotate→grace→retire.
// retired_at is stored as epoch-nanos (NULL = never retired); validity
// (NULL or now < retired_at) is decided by the auth layer, not here.
// expires_at is stored the same way (NULL = never expires).

const operatorTokenDefSelect = `SELECT def_id, name, tenant_id, subject, token_hash,
	allowed_scopes, created_at, created_by_agent_id, created_by_run_id,
	rotated_from, retired_at, expires_at, restrictions FROM operator_token_defs`

func scanOperatorTokenDef(sc interface{ Scan(...any) error }) (store.OperatorTokenDefRow, error) {
	var (
		row        store.OperatorTokenDefRow
		scopesJSON string
		createdNs  int64
		agent      sql.NullString
		runID      sql.NullString
		rotated    sql.NullString
		retiredNs  sql.NullInt64
		expiresNs  sql.NullInt64
		restrict   sql.NullString
	)
	if err := sc.Scan(&row.DefID, &row.Name, &row.TenantID, &row.Subject, &row.TokenHash,
		&scopesJSON, &createdNs, &agent, &runID, &rotated, &retiredNs, &expiresNs, &restrict); err != nil {
		return store.OperatorTokenDefRow{}, err
	}
	if err := json.Unmarshal([]byte(scopesJSON), &row.AllowedScopes); err != nil {
		return store.OperatorTokenDefRow{}, fmt.Errorf("operator_token_def: decode allowed_scopes: %w", err)
	}
	row.CreatedAt = time.Unix(0, createdNs)
	row.CreatedByAgentID = agent.String
	row.CreatedByRunID = runID.String
	row.RotatedFrom = rotated.String
	if retiredNs.Valid {
		row.RetiredAt = time.Unix(0, retiredNs.Int64)
	}
	if expiresNs.Valid {
		row.ExpiresAt = time.Unix(0, expiresNs.Int64)
	}
	row.RestrictionsJSON = restrict.String
	return row, nil
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
		row.CreatedAt = time.Now()
	}
	var retired, expires any
	if !row.RetiredAt.IsZero() {
		retired = row.RetiredAt.UnixNano()
	}
	if !row.ExpiresAt.IsZero() {
		expires = row.ExpiresAt.UnixNano()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.OperatorTokenDefRow{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO operator_token_defs (
			def_id, name, tenant_id, subject, token_hash, allowed_scopes,
			created_at, created_by_agent_id, created_by_run_id, rotated_from, retired_at, expires_at, restrictions
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.TenantID, row.Subject, row.TokenHash, string(scopesJSON),
		row.CreatedAt.UnixNano(), nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		nilIfEmpty(row.RotatedFrom), retired, expires, nilIfEmpty(row.RestrictionsJSON),
	); err != nil {
		return store.OperatorTokenDefRow{}, fmt.Errorf("operator_token_def insert: %w", err)
	}
	return row, nil
}

func (s *Store) OperatorTokenDefGet(ctx context.Context, defID string) (store.OperatorTokenDefRow, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.OperatorTokenDefRow{}, err
	}
	defer conn.Close()
	row, err := scanOperatorTokenDef(conn.QueryRowContext(ctx, operatorTokenDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.OperatorTokenDefRow{}, &store.ErrNotFound{Kind: "operator_token_def", ID: defID}
	}
	return row, err
}

func (s *Store) OperatorTokenDefGetByTokenHash(ctx context.Context, tokenHash string) (store.OperatorTokenDefRow, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.OperatorTokenDefRow{}, err
	}
	defer conn.Close()
	row, err := scanOperatorTokenDef(conn.QueryRowContext(ctx, operatorTokenDefSelect+` WHERE token_hash = ?`, tokenHash))
	if err == sql.ErrNoRows {
		return store.OperatorTokenDefRow{}, &store.ErrNotFound{Kind: "operator_token_def", ID: "<hash>"}
	}
	return row, err
}

func (s *Store) OperatorTokenDefGetCurrentByName(ctx context.Context, name string) (store.OperatorTokenDefRow, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.OperatorTokenDefRow{}, err
	}
	defer conn.Close()
	row, err := scanOperatorTokenDef(conn.QueryRowContext(ctx,
		operatorTokenDefSelect+` WHERE name = ? AND retired_at IS NULL
			AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at DESC LIMIT 1`, name, time.Now().UnixNano()))
	if err == sql.ErrNoRows {
		return store.OperatorTokenDefRow{}, &store.ErrNotFound{Kind: "operator_token_def", ID: name}
	}
	return row, err
}

func (s *Store) OperatorTokenDefListByName(ctx context.Context, name string) ([]store.OperatorTokenDefRow, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, operatorTokenDefSelect+` WHERE name = ? ORDER BY created_at DESC`, name)
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
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx,
		operatorTokenDefSelect+` WHERE tenant_id = ? AND subject = ? ORDER BY created_at DESC`, tenantID, subject)
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
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// One summary per name. tenant_id/subject taken from the newest row;
	// has_current = any non-retired, non-expired row exists.
	rows, err := conn.QueryContext(ctx, `
		SELECT name,
		       COUNT(*) AS token_count,
		       MAX(created_at) AS last_updated,
		       SUM(CASE WHEN retired_at IS NULL AND (expires_at IS NULL OR expires_at > ?) THEN 1 ELSE 0 END) AS current_count
		FROM operator_token_defs GROUP BY name ORDER BY name`, time.Now().UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorTokenDefNameSummary
	for rows.Next() {
		var (
			sum          store.OperatorTokenDefNameSummary
			lastNs       int64
			currentCount int
		)
		if err := rows.Scan(&sum.Name, &sum.TokenCount, &lastNs, &currentCount); err != nil {
			return nil, err
		}
		sum.LastUpdated = time.Unix(0, lastNs)
		sum.HasCurrent = currentCount > 0
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Backfill tenant_id/subject from the newest row per name (cheap; the
	// name set is small — operator-managed, not per-request).
	for i := range out {
		r, err := scanOperatorTokenDef(conn.QueryRowContext(ctx,
			operatorTokenDefSelect+` WHERE name = ? ORDER BY created_at DESC LIMIT 1`, out[i].Name))
		if err == nil {
			out[i].TenantID = r.TenantID
			out[i].Subject = r.Subject
		}
	}
	return out, nil
}

func (s *Store) OperatorTokenDefSetRetiredAt(ctx context.Context, defID string, retiredAt time.Time) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var retired any
	if !retiredAt.IsZero() {
		retired = retiredAt.UnixNano()
	}
	res, err := conn.ExecContext(ctx, `UPDATE operator_token_defs SET retired_at = ? WHERE def_id = ?`, retired, defID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &store.ErrNotFound{Kind: "operator_token_def", ID: defID}
	}
	return nil
}

func (s *Store) OperatorTokenDefCountActiveAdmin(ctx context.Context) (int, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	// Non-retired (retired_at NULL or in the future) tokens whose
	// allowed_scopes JSON array contains "substrate:admin". SQLite has no
	// JSON array containment operator portable across builds, so match the
	// quoted element textually — scopes are a closed catalog with no
	// substrings of each other, so "substrate:admin" can't false-match.
	now := time.Now().UnixNano()
	var n int
	err = conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM operator_token_defs
		WHERE (retired_at IS NULL OR retired_at > ?)
		  AND (expires_at IS NULL OR expires_at > ?)
		  AND allowed_scopes LIKE '%"substrate:admin"%'`, now, now).Scan(&n)
	return n, err
}

func (s *Store) OperatorTokenDefDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := before.UnixNano()
	// A rotated token names its predecessor, and both carry the same expiry,
	// but the predecessor's row may be the only one past the cutoff by a
	// clock tick. Detach the reference first so the delete can never be
	// refused by it.
	if _, err := tx.ExecContext(ctx, `
		UPDATE operator_token_defs SET rotated_from = NULL
		WHERE rotated_from IN (
			SELECT def_id FROM operator_token_defs WHERE expires_at IS NOT NULL AND expires_at < ?)`, cutoff); err != nil {
		return 0, err
	}
	// The token rows are also the registry of tenant spellings a new tenant
	// is checked against for a case-fold collision (tokenTenants), so the
	// last row naming a tenant is kept — expired, it authenticates nothing.
	// A tenant with a row the sweep leaves loses every expired one.
	res, err := tx.ExecContext(ctx, `
		DELETE FROM operator_token_defs
		WHERE expires_at IS NOT NULL AND expires_at < ?
		  AND (EXISTS (SELECT 1 FROM operator_token_defs k
		               WHERE k.tenant_id = operator_token_defs.tenant_id
		                 AND (k.expires_at IS NULL OR k.expires_at >= ?))
		       OR def_id <> (SELECT MAX(k.def_id) FROM operator_token_defs k
		                     WHERE k.tenant_id = operator_token_defs.tenant_id))`, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}
