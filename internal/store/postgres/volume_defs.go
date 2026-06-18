package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// VolumeDef* implements the RFC AH Phase 2a persistent dynamic volume
// substrate on Postgres. Flat (tenant_id, name) table — no version, no
// parent, no content hash. See internal/store/store.go for the contract.

// VolumeDefCreate UPSERTs the (tenant_id, name) row. Re-creating an
// existing name UPDATES the definition + bumps updated_at, leaving
// created_at intact (so the tool's "different mode updates" semantics
// hold). definition::jsonb so it round-trips semantically (the SELECT
// reads ::text back — byte-for-byte stable for the {path,mode} shape, but
// jsonb-normalized which is fine: the tool re-derives, never byte-compares).
func (s *Store) VolumeDefCreate(ctx context.Context, row store.VolumeDefRow) (store.VolumeDefRow, error) {
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO volume_defs (tenant_id, name, definition, created_at, updated_at)
		 VALUES ($1, $2, $3::jsonb, $4, $5)
		 ON CONFLICT (tenant_id, name) DO UPDATE SET
		     definition = EXCLUDED.definition,
		     updated_at = EXCLUDED.updated_at`,
		row.TenantID, row.Name, string(row.Definition), now, now,
	); err != nil {
		return store.VolumeDefRow{}, err
	}
	return s.VolumeDefGetByName(ctx, row.TenantID, row.Name)
}

func (s *Store) VolumeDefGetByName(ctx context.Context, tenantID, name string) (store.VolumeDefRow, error) {
	var (
		out        store.VolumeDefRow
		definition string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, name, definition::text, created_at, updated_at
		 FROM volume_defs WHERE tenant_id = $1 AND name = $2`, tenantID, name,
	).Scan(&out.TenantID, &out.Name, &definition, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.VolumeDefRow{}, &store.ErrNotFound{Kind: "volume_def", ID: name}
	}
	if err != nil {
		return store.VolumeDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	return out, nil
}

func (s *Store) VolumeDefList(ctx context.Context, tenantID string) ([]store.VolumeDefRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, name, definition::text, created_at, updated_at
		 FROM volume_defs WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.VolumeDefRow
	for rows.Next() {
		var (
			r          store.VolumeDefRow
			definition string
		)
		if err := rows.Scan(&r.TenantID, &r.Name, &definition, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) VolumeDefDelete(ctx context.Context, tenantID, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM volume_defs WHERE tenant_id = $1 AND name = $2`, tenantID, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
