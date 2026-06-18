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

// ---- RFC AH Phase 2b ephemeral (run-tree-scoped) volumes ----

// EphemeralVolumeCreate UPSERTs the (root_run_id, name) row. Re-creating an
// existing name within the same run updates the definition + leaves
// created_at intact (mirrors VolumeDefCreate). created_at is stamped here
// (server clock).
func (s *Store) EphemeralVolumeCreate(ctx context.Context, row store.EphemeralVolumeDefRow) (store.EphemeralVolumeDefRow, error) {
	now := time.Now()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ephemeral_volume_defs (root_run_id, name, tenant_id, definition, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(root_run_id, name) DO UPDATE SET
		     tenant_id  = excluded.tenant_id,
		     definition = excluded.definition`,
		row.RootRunID, row.Name, row.TenantID, string(row.Definition), now.UnixNano(),
	); err != nil {
		return store.EphemeralVolumeDefRow{}, err
	}
	return s.ephemeralVolumeGet(ctx, row.RootRunID, row.Name)
}

// ephemeralVolumeGet is an internal by-PK read used to re-read after an
// upsert (so created_at reflects the original create on an update).
func (s *Store) ephemeralVolumeGet(ctx context.Context, rootRunID, name string) (store.EphemeralVolumeDefRow, error) {
	var (
		out        store.EphemeralVolumeDefRow
		definition string
		createdAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT root_run_id, name, tenant_id, definition, created_at
		 FROM ephemeral_volume_defs WHERE root_run_id = ? AND name = ?`, rootRunID, name,
	).Scan(&out.RootRunID, &out.Name, &out.TenantID, &definition, &createdAt)
	if err == sql.ErrNoRows {
		return store.EphemeralVolumeDefRow{}, &store.ErrNotFound{Kind: "ephemeral_volume_def", ID: name}
	}
	if err != nil {
		return store.EphemeralVolumeDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	return out, nil
}

func (s *Store) EphemeralVolumeListByRun(ctx context.Context, rootRunID string) ([]store.EphemeralVolumeDefRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT root_run_id, name, tenant_id, definition, created_at
		 FROM ephemeral_volume_defs WHERE root_run_id = ? ORDER BY name`, rootRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.EphemeralVolumeDefRow
	for rows.Next() {
		var (
			r          store.EphemeralVolumeDefRow
			definition string
			createdAt  int64
		)
		if err := rows.Scan(&r.RootRunID, &r.Name, &r.TenantID, &definition, &createdAt); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) EphemeralVolumeDeleteByRun(ctx context.Context, rootRunID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ephemeral_volume_defs WHERE root_run_id = ?`, rootRunID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// EphemeralVolumeSweepCandidates returns DISTINCT (root_run_id, tenant_id)
// whose owning run is TERMINAL and NOT paused/pausing — the crash-recovery
// backstop's work list. The terminal-and-not-paused filter mirrors
// SweepStaleRuns: a paused run is parked (no heartbeat by design), not
// crashed, so its ephemeral volumes must survive to be reused on resume.
func (s *Store) EphemeralVolumeSweepCandidates(ctx context.Context) ([]store.EphemeralVolumeSweepRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT e.root_run_id, e.tenant_id
		 FROM ephemeral_volume_defs e
		 JOIN runs r ON r.id = e.root_run_id
		 WHERE r.status IN ('completed', 'failed', 'cancelled')
		   AND COALESCE(r.pause_state, 'running') NOT IN ('paused', 'pausing')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.EphemeralVolumeSweepRow
	for rows.Next() {
		var r store.EphemeralVolumeSweepRow
		if err := rows.Scan(&r.RootRunID, &r.TenantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
