package sqlshared

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FL P0b: runtime-registered app clients (store.ClientDefStore).

const clientDefCols = `def_id, name, tenant_id, version, definition, content_sha256, description, created_at,
	created_by, created_by_tenant, retired_at`

func scanClientDef(d Dialect, r Row) (store.ClientDefRow, error) {
	var out store.ClientDefRow
	err := r.Scan(&out.DefID, &out.Name, &out.TenantID, &out.Version, &out.Definition, &out.ContentSHA256,
		&out.Description, d.TimeDest(&out.CreatedAt), &out.CreatedBy, &out.CreatedByTenant, &nullTimeDest{&out.RetiredAt})
	return out, err
}

// ClientDefCreate appends a version, refusing a name another tenant holds.
// The check and the insert share one transaction, and UNIQUE (name, version)
// stops two creates racing to version 1 for different tenants.
func ClientDefCreate(ctx context.Context, db DB, row store.ClientDefRow) (store.ClientDefRow, error) {
	if row.DefID == "" || row.Name == "" || row.Definition == "" || row.ContentSHA256 == "" {
		return store.ClientDefRow{}, errors.New("client_def: def_id, name, definition and content_sha256 are required")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	err := db.InTx(ctx, func(tx Querier) error {
		d := tx.Dialect()
		var (
			version int
			other   int
		)
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0), COUNT(*) FILTER (WHERE tenant_id <> ?)
			FROM client_defs WHERE name = ?`, row.TenantID, row.Name).Scan(&version, &other); err != nil {
			return err
		}
		if other > 0 {
			return &store.ErrConflict{Kind: "client_def", ID: row.Name}
		}
		row.Version = version + 1
		_, err := tx.Exec(ctx, `INSERT INTO client_defs (`+clientDefCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			row.DefID, row.Name, row.TenantID, row.Version, row.Definition, row.ContentSHA256, row.Description,
			d.TimeArg(row.CreatedAt), row.CreatedBy, row.CreatedByTenant)
		if IsUniqueViolation(err) {
			return &store.ErrConflict{Kind: "client_def", ID: row.Name}
		}
		return err
	})
	if err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			return store.ClientDefRow{}, err
		}
		return store.ClientDefRow{}, fmt.Errorf("client_def insert: %w", err)
	}
	row.RetiredAt = time.Time{}
	return row, nil
}

// ClientDefGet returns one version.
func ClientDefGet(ctx context.Context, db DB, defID string) (store.ClientDefRow, error) {
	out, err := scanClientDef(db.Dialect(), db.QueryRow(ctx, `SELECT `+clientDefCols+` FROM client_defs WHERE def_id = ?`, defID))
	if err != nil {
		return store.ClientDefRow{}, NotFound(err, "client_def", defID)
	}
	return out, nil
}

// ClientDefGetActive returns the newest version of name when it is live.
func ClientDefGetActive(ctx context.Context, db DB, name string) (store.ClientDefRow, error) {
	out, err := scanClientDef(db.Dialect(), db.QueryRow(ctx, `SELECT `+clientDefCols+` FROM client_defs
		WHERE name = ? ORDER BY version DESC LIMIT 1`, name))
	if err != nil {
		return store.ClientDefRow{}, NotFound(err, "client_def", name)
	}
	if !out.RetiredAt.IsZero() {
		return store.ClientDefRow{}, &store.ErrNotFound{Kind: "client_def", ID: name}
	}
	return out, nil
}

// ClientDefList returns every live client's newest version, by name.
func ClientDefList(ctx context.Context, db DB) ([]store.ClientDefRow, error) {
	d := db.Dialect()
	rows, err := db.Query(ctx, `SELECT `+clientDefCols+` FROM client_defs c
		WHERE retired_at IS NULL AND version = (SELECT MAX(version) FROM client_defs m WHERE m.name = c.name)
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ClientDefRow
	for rows.Next() {
		r, err := scanClientDef(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClientDefRetire retires every live version of name.
func ClientDefRetire(ctx context.Context, db DB, name string, at time.Time) (bool, error) {
	n, err := db.Exec(ctx, `UPDATE client_defs SET retired_at = ? WHERE name = ? AND retired_at IS NULL`,
		db.Dialect().TimeArg(at), name)
	return n > 0, err
}
