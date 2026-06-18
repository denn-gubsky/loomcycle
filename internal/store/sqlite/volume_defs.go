package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// VolumeDef* implements the RFC AH Phase 2a persistent dynamic volume
// substrate. Flat (tenant_id, name) table — no version, no parent, no
// content hash (a Volume points at mutable on-disk state outside the def).
// See internal/store/store.go for the interface contract.

// VolumeDefCreate UPSERTs the (tenant_id, name) row. Re-creating an
// existing name UPDATES the definition (so the tool's "different mode
// updates" semantics hold) and bumps updated_at, leaving created_at
// intact. created_at/updated_at are stamped here (server clock) so the
// caller never supplies wall-clock fields.
func (s *Store) VolumeDefCreate(ctx context.Context, row store.VolumeDefRow) (store.VolumeDefRow, error) {
	now := time.Now()
	row.CreatedAt = now
	row.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO volume_defs (tenant_id, name, definition, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, name) DO UPDATE SET
		     definition = excluded.definition,
		     updated_at = excluded.updated_at`,
		row.TenantID, row.Name, string(row.Definition), now.UnixNano(), now.UnixNano(),
	); err != nil {
		return store.VolumeDefRow{}, err
	}
	// Re-read so created_at reflects the original create on an update
	// (the ON CONFLICT branch does not touch created_at).
	return s.VolumeDefGetByName(ctx, row.TenantID, row.Name)
}

func (s *Store) VolumeDefGetByName(ctx context.Context, tenantID, name string) (store.VolumeDefRow, error) {
	var (
		out        store.VolumeDefRow
		definition string
		createdAt  int64
		updatedAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, name, definition, created_at, updated_at
		 FROM volume_defs WHERE tenant_id = ? AND name = ?`, tenantID, name,
	).Scan(&out.TenantID, &out.Name, &definition, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return store.VolumeDefRow{}, &store.ErrNotFound{Kind: "volume_def", ID: name}
	}
	if err != nil {
		return store.VolumeDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.UpdatedAt = time.Unix(0, updatedAt)
	return out, nil
}

func (s *Store) VolumeDefList(ctx context.Context, tenantID string) ([]store.VolumeDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, name, definition, created_at, updated_at
		 FROM volume_defs WHERE tenant_id = ? ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.VolumeDefRow
	for rows.Next() {
		var (
			r          store.VolumeDefRow
			definition string
			createdAt  int64
			updatedAt  int64
		)
		if err := rows.Scan(&r.TenantID, &r.Name, &definition, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		r.UpdatedAt = time.Unix(0, updatedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) VolumeDefDelete(ctx context.Context, tenantID, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM volume_defs WHERE tenant_id = ? AND name = ?`, tenantID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
