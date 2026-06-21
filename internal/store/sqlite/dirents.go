package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Dirent* implements the RFC AL Path-tree substrate. The PK is the full
// coordinate (tenant_id, scope, scope_id, parent_path, name) so each
// (tenant, scope, scope_id) tree is independent and a name is unique within
// its parent directory. Paths arrive pre-normalized (canonical, no "..") from
// the tool layer. See internal/store/store.go for the interface contract.

const direntCols = `tenant_id, scope, scope_id, parent_path, name, kind, resource_ref, created_at, updated_at`

// likeEscape escapes the LIKE metacharacters (%, _, \) in a prefix so a
// path segment containing '_' (a valid dirent char) can't act as a wildcard
// in a recursive prefix match. Used with `LIKE ? ESCAPE '\'`.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *Store) DirentCreate(ctx context.Context, row store.DirentRow) (store.DirentRow, error) {
	now := time.Now()
	// resource_ref is NOT NULL and must be valid JSON (postgres stores it as
	// jsonb, which rejects ""); default an empty ref to "{}" for backend
	// parity.
	if len(row.ResourceRef) == 0 {
		row.ResourceRef = json.RawMessage("{}")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dirents (`+direntCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, scope, scope_id, parent_path, name) DO UPDATE SET
		     kind = excluded.kind,
		     resource_ref = excluded.resource_ref,
		     updated_at = excluded.updated_at`,
		row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name,
		row.Kind, string(row.ResourceRef), now.UnixNano(), now.UnixNano(),
	); err != nil {
		return store.DirentRow{}, err
	}
	return s.DirentGet(ctx, row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name)
}

func scanDirent(sc interface{ Scan(...any) error }) (store.DirentRow, error) {
	var (
		r         store.DirentRow
		ref       string
		createdAt int64
		updatedAt int64
	)
	if err := sc.Scan(&r.TenantID, &r.Scope, &r.ScopeID, &r.ParentPath, &r.Name,
		&r.Kind, &ref, &createdAt, &updatedAt); err != nil {
		return store.DirentRow{}, err
	}
	r.ResourceRef = json.RawMessage(ref)
	r.CreatedAt = time.Unix(0, createdAt)
	r.UpdatedAt = time.Unix(0, updatedAt)
	return r, nil
}

func (s *Store) DirentGet(ctx context.Context, tenantID, scope, scopeID, parentPath, name string) (store.DirentRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+direntCols+` FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ? AND name = ?`,
		tenantID, scope, scopeID, parentPath, name)
	r, err := scanDirent(row)
	if err == sql.ErrNoRows {
		return store.DirentRow{}, &store.ErrNotFound{Kind: "dirent", ID: parentPath + name}
	}
	if err != nil {
		return store.DirentRow{}, err
	}
	return r, nil
}

func (s *Store) DirentList(ctx context.Context, tenantID, scope, scopeID, parentPath string) ([]store.DirentRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+direntCols+` FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ?
		 ORDER BY name`, tenantID, scope, scopeID, parentPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDirents(rows)
}

// DirentListUnder returns every dirent at or under prefix (a trailing-slashed
// directory path, e.g. "/docs/"): rows whose parent_path == prefix or begins
// with prefix. Used for recursive ls.
func (s *Store) DirentListUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) ([]store.DirentRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+direntCols+` FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
		   AND (parent_path = ? OR parent_path LIKE ? ESCAPE '\')
		 ORDER BY parent_path, name`,
		tenantID, scope, scopeID, prefix, likeEscape(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDirents(rows)
}

func collectDirents(rows *sql.Rows) ([]store.DirentRow, error) {
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
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ? AND name = ?`,
		tenantID, scope, scopeID, parentPath, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DirentDeleteUnder removes every descendant at or under prefix (a
// trailing-slashed directory path). It does NOT remove the directory's own
// entry (which lives at its parent) — the tool removes that separately.
func (s *Store) DirentDeleteUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
		   AND (parent_path = ? OR parent_path LIKE ? ESCAPE '\')`,
		tenantID, scope, scopeID, prefix, likeEscape(prefix)+"%")
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// DirentMove relocates the dirent at (fromParent, fromName) to (toParent,
// toName) AND rewrites the parent_path prefix of every descendant, in one
// transaction (recursive rename). found=false if the source doesn't exist.
func (s *Store) DirentMove(ctx context.Context, tenantID, scope, scopeID, fromParent, fromName, toParent, toName string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixNano()
	// 1. Move the entry itself.
	res, err := tx.ExecContext(ctx,
		`UPDATE dirents SET parent_path = ?, name = ?, updated_at = ?
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ? AND name = ?`,
		toParent, toName, now, tenantID, scope, scopeID, fromParent, fromName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil // source absent; nothing moved
	}

	// 2. Rewrite descendants. The moved node's old full path is fromFull; its
	// descendants have parent_path starting with fromFull+"/". Replace that
	// leading prefix with toFull+"/" (toFull = toParent+toName). SUBSTR drops
	// the old prefix and keeps the tail.
	fromFull := fromParent + fromName + "/"
	toFull := toParent + toName + "/"
	if _, err := tx.ExecContext(ctx,
		`UPDATE dirents
		 SET parent_path = ? || SUBSTR(parent_path, ?), updated_at = ?
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
		   AND (parent_path = ? OR parent_path LIKE ? ESCAPE '\')`,
		toFull, len(fromFull)+1, now,
		tenantID, scope, scopeID, fromFull, likeEscape(fromFull)+"%"); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
