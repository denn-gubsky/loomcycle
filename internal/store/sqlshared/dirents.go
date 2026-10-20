package sqlshared

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Dirent* implements the RFC AL Path-tree substrate for both stores. The PK
// is the full coordinate (tenant_id, scope, scope_id, parent_path, name), so
// each (tenant, scope, scope_id) tree is independent and a name is unique
// within its parent directory. Paths arrive pre-normalized (canonical, no
// "..") from the tool layer. See internal/store/store.go for the contract.

const direntInsert = `INSERT INTO dirents (tenant_id, scope, scope_id, parent_path, name, kind, resource_ref, created_at, updated_at)`

func direntSelect(d Dialect) string {
	return `SELECT tenant_id, scope, scope_id, parent_path, name, kind, ` + d.JSONText("resource_ref") + `, created_at, updated_at FROM dirents`
}

func direntValues(d Dialect) string {
	return `VALUES (?, ?, ?, ?, ?, ?, ` + d.JSONArg() + `, ?, ?)`
}

// DirentCreate upserts the entry: an existing coordinate takes the new kind
// and resource_ref and keeps its created_at.
func DirentCreate(ctx context.Context, q Querier, row store.DirentRow) (store.DirentRow, error) {
	d := q.Dialect()
	now := time.Now().UTC()
	// resource_ref is NOT NULL and must be valid JSON (Postgres stores it as
	// jsonb, which rejects ""), so an empty ref is stored as "{}".
	if len(row.ResourceRef) == 0 {
		row.ResourceRef = json.RawMessage("{}")
	}
	if _, err := q.Exec(ctx,
		direntInsert+` `+direntValues(d)+`
		 ON CONFLICT (tenant_id, scope, scope_id, parent_path, name) DO UPDATE SET
		     kind = excluded.kind,
		     resource_ref = excluded.resource_ref,
		     updated_at = excluded.updated_at`,
		row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name,
		row.Kind, string(row.ResourceRef), d.TimeArg(now), d.TimeArg(now),
	); err != nil {
		return store.DirentRow{}, err
	}
	return DirentGet(ctx, q, row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name)
}

func scanDirent(d Dialect, sc Row) (store.DirentRow, error) {
	var (
		r   store.DirentRow
		ref string
	)
	if err := sc.Scan(&r.TenantID, &r.Scope, &r.ScopeID, &r.ParentPath, &r.Name,
		&r.Kind, &ref, d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt)); err != nil {
		return store.DirentRow{}, err
	}
	r.ResourceRef = json.RawMessage(ref)
	return r, nil
}

func collectDirents(d Dialect, rows Rows) ([]store.DirentRow, error) {
	defer rows.Close()
	var out []store.DirentRow
	for rows.Next() {
		r, err := scanDirent(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func DirentGet(ctx context.Context, q Querier, tenantID, scope, scopeID, parentPath, name string) (store.DirentRow, error) {
	d := q.Dialect()
	r, err := scanDirent(d, q.QueryRow(ctx,
		direntSelect(d)+`
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ? AND name = ?`,
		tenantID, scope, scopeID, parentPath, name))
	if err != nil {
		return store.DirentRow{}, NotFound(err, "dirent", parentPath+name)
	}
	return r, nil
}

func DirentList(ctx context.Context, q Querier, tenantID, scope, scopeID, parentPath string) ([]store.DirentRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx,
		direntSelect(d)+`
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ?
		 ORDER BY name`, tenantID, scope, scopeID, parentPath)
	if err != nil {
		return nil, err
	}
	return collectDirents(d, rows)
}

// DirentListUnder returns every dirent at or under prefix (a trailing-slashed
// directory path, e.g. "/docs/"): rows whose parent_path is prefix or begins
// with it. Used for recursive ls.
func DirentListUnder(ctx context.Context, q Querier, tenantID, scope, scopeID, prefix string) ([]store.DirentRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx,
		direntSelect(d)+`
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
		   AND (parent_path = ? OR parent_path LIKE ? ESCAPE '\')
		 ORDER BY parent_path, name`,
		tenantID, scope, scopeID, prefix, EscapeLike(prefix)+"%")
	if err != nil {
		return nil, err
	}
	return collectDirents(d, rows)
}

func DirentDelete(ctx context.Context, q Querier, tenantID, scope, scopeID, parentPath, name string) (bool, error) {
	n, err := q.Exec(ctx,
		`DELETE FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ? AND name = ?`,
		tenantID, scope, scopeID, parentPath, name)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DirentDeleteUnder removes every descendant at or under prefix (a
// trailing-slashed directory path). It does NOT remove the directory's own
// entry (which lives at its parent); the tool removes that separately.
func DirentDeleteUnder(ctx context.Context, q Querier, tenantID, scope, scopeID, prefix string) (int, error) {
	n, err := q.Exec(ctx,
		`DELETE FROM dirents
		 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
		   AND (parent_path = ? OR parent_path LIKE ? ESCAPE '\')`,
		tenantID, scope, scopeID, prefix, EscapeLike(prefix)+"%")
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// DirentMove relocates the dirent at (fromParent, fromName) to (toParent,
// toName) AND rewrites the parent_path prefix of every descendant, in one
// transaction (recursive rename). found=false if the source doesn't exist.
func DirentMove(ctx context.Context, db DB, tenantID, scope, scopeID, fromParent, fromName, toParent, toName string) (bool, error) {
	d := db.Dialect()
	now := d.TimeArg(time.Now().UTC())
	found := false
	err := db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx,
			`UPDATE dirents SET parent_path = ?, name = ?, updated_at = ?
			 WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND parent_path = ? AND name = ?`,
			toParent, toName, now, tenantID, scope, scopeID, fromParent, fromName)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil // the source is absent; nothing moved
		}
		found = true

		// The moved node's old full path is fromFull, so its descendants'
		// parent_path begins with fromFull; SUBSTR drops that prefix and the
		// new one is prepended. CAST: a bare placeholder as SUBSTR's start is
		// inferred as text by Postgres, and pgx then can't encode the Go int.
		fromFull := fromParent + fromName + "/"
		toFull := toParent + toName + "/"
		_, err = tx.Exec(ctx,
			`UPDATE dirents
			 SET parent_path = ? || SUBSTR(parent_path, CAST(? AS INTEGER)), updated_at = ?
			 WHERE tenant_id = ? AND scope = ? AND scope_id = ?
			   AND (parent_path = ? OR parent_path LIKE ? ESCAPE '\')`,
			toFull, len(fromFull)+1, now,
			tenantID, scope, scopeID, fromFull, EscapeLike(fromFull)+"%")
		return err
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// SnapshotReadDirents returns every tree's entries, every tenant's.
func SnapshotReadDirents(ctx context.Context, q Querier) ([]store.DirentRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, direntSelect(d)+`
		 ORDER BY tenant_id, scope, scope_id, parent_path, name`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read dirents: %w", err)
	}
	out, err := collectDirents(d, rows)
	if err != nil {
		return nil, fmt.Errorf("snapshot read dirents: %w", err)
	}
	return out, nil
}

// SnapshotRestoreDirent inserts one entry, keeping both timestamps; an entry
// already on the coordinate stands. The conflict target is the key, not
// SQLite's INSERT OR IGNORE, which would also swallow a NOT NULL failure as
// "already here".
func SnapshotRestoreDirent(ctx context.Context, q Querier, row store.DirentRow) (bool, error) {
	d := q.Dialect()
	ref := row.ResourceRef
	if len(ref) == 0 {
		ref = json.RawMessage("{}") // DirentCreate's default
	}
	createdAt, updatedAt := row.CreatedAt, row.UpdatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	n, err := q.Exec(ctx,
		direntInsert+` `+direntValues(d)+`
		 ON CONFLICT (tenant_id, scope, scope_id, parent_path, name) DO NOTHING`,
		row.TenantID, row.Scope, row.ScopeID, row.ParentPath, row.Name,
		row.Kind, string(ref), d.TimeArg(createdAt), d.TimeArg(updatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore dirent: %w", err)
	}
	return n > 0, nil
}
