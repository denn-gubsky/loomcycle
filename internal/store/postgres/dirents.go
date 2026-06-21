package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Dirent* implements the RFC AL Path-tree substrate on Postgres. PK is the
// full (tenant_id, scope, scope_id, parent_path, name) coordinate. Paths
// arrive pre-normalized (canonical, no "..") from the tool layer. See
// internal/store/store.go for the interface contract.

const direntCols = `tenant_id, scope, scope_id, parent_path, name, kind, resource_ref::text, created_at, updated_at`

// likeEscape escapes the LIKE metacharacters (%, _, \) in a prefix so a path
// segment containing '_' (a valid dirent char) can't act as a wildcard in a
// recursive prefix match. Used with `LIKE $n ESCAPE '\'`.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *Store) DirentCreate(ctx context.Context, row store.DirentRow) (store.DirentRow, error) {
	now := time.Now().UTC()
	// resource_ref is jsonb NOT NULL — an empty string is not valid JSON and
	// the cast would fail; default to "{}" (matches the sqlite backend).
	if len(row.ResourceRef) == 0 {
		row.ResourceRef = json.RawMessage("{}")
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO dirents (tenant_id, scope, scope_id, parent_path, name, kind, resource_ref, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
		 ON CONFLICT (tenant_id, scope, scope_id, parent_path, name) DO UPDATE SET
		     kind = EXCLUDED.kind,
		     resource_ref = EXCLUDED.resource_ref,
		     updated_at = EXCLUDED.updated_at`,
		row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name,
		row.Kind, string(row.ResourceRef), now, now,
	); err != nil {
		return store.DirentRow{}, err
	}
	return s.DirentGet(ctx, row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name)
}

func scanDirent(sc interface{ Scan(...any) error }) (store.DirentRow, error) {
	var (
		r   store.DirentRow
		ref string
	)
	if err := sc.Scan(&r.TenantID, &r.Scope, &r.ScopeID, &r.ParentPath, &r.Name,
		&r.Kind, &ref, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return store.DirentRow{}, err
	}
	r.ResourceRef = json.RawMessage(ref)
	return r, nil
}

func (s *Store) DirentGet(ctx context.Context, tenantID, scope, scopeID, parentPath, name string) (store.DirentRow, error) {
	r, err := scanDirent(s.pool.QueryRow(ctx,
		`SELECT `+direntCols+` FROM dirents
		 WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 AND parent_path = $4 AND name = $5`,
		tenantID, scope, scopeID, parentPath, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.DirentRow{}, &store.ErrNotFound{Kind: "dirent", ID: parentPath + name}
	}
	if err != nil {
		return store.DirentRow{}, err
	}
	return r, nil
}

func (s *Store) DirentList(ctx context.Context, tenantID, scope, scopeID, parentPath string) ([]store.DirentRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+direntCols+` FROM dirents
		 WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 AND parent_path = $4
		 ORDER BY name`, tenantID, scope, scopeID, parentPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDirents(rows)
}

func (s *Store) DirentListUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) ([]store.DirentRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+direntCols+` FROM dirents
		 WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3
		   AND (parent_path = $4 OR parent_path LIKE $5 ESCAPE '\')
		 ORDER BY parent_path, name`,
		tenantID, scope, scopeID, prefix, likeEscape(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDirents(rows)
}

func collectDirents(rows pgx.Rows) ([]store.DirentRow, error) {
	var out []store.DirentRow
	for rows.Next() {
		r, err := scanDirent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DirentDelete(ctx context.Context, tenantID, scope, scopeID, parentPath, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM dirents
		 WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 AND parent_path = $4 AND name = $5`,
		tenantID, scope, scopeID, parentPath, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) DirentDeleteUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM dirents
		 WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3
		   AND (parent_path = $4 OR parent_path LIKE $5 ESCAPE '\')`,
		tenantID, scope, scopeID, prefix, likeEscape(prefix)+"%")
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DirentMove relocates the dirent at (fromParent, fromName) to (toParent,
// toName) AND rewrites every descendant's parent_path prefix, in one
// transaction (recursive rename). found=false if the source doesn't exist.
func (s *Store) DirentMove(ctx context.Context, tenantID, scope, scopeID, fromParent, fromName, toParent, toName string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()
	tag, err := tx.Exec(ctx,
		`UPDATE dirents SET parent_path = $1, name = $2, updated_at = $3
		 WHERE tenant_id = $4 AND scope = $5 AND scope_id = $6 AND parent_path = $7 AND name = $8`,
		toParent, toName, now, tenantID, scope, scopeID, fromParent, fromName)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	// Rewrite descendants: old full path of the moved node is fromFull; its
	// descendants' parent_path begins with fromFull. SUBSTRING drops that
	// leading prefix; concat the new prefix.
	fromFull := fromParent + fromName + "/"
	toFull := toParent + toName + "/"
	if _, err := tx.Exec(ctx,
		// $2::int — without the cast Postgres infers $2 as text (it can't tell
		// SUBSTRING's FROM arg is an integer from a bare placeholder) and pgx
		// fails to encode the Go int. SQLite is untyped so its contract run
		// passed; the go-postgres CI job caught this parity gap.
		`UPDATE dirents
		 SET parent_path = $1 || SUBSTRING(parent_path FROM $2::int), updated_at = $3
		 WHERE tenant_id = $4 AND scope = $5 AND scope_id = $6
		   AND (parent_path = $7 OR parent_path LIKE $8 ESCAPE '\')`,
		toFull, len(fromFull)+1, now,
		tenantID, scope, scopeID, fromFull, likeEscape(fromFull)+"%"); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
