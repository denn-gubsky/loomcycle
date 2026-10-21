package sqlshared

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// VolumeDef* implements the RFC AH Phase 2a persistent dynamic volume
// substrate for both stores. Flat (tenant_id, name) table — no version, no
// parent, no content hash (a Volume points at mutable on-disk state outside
// the def). See internal/store/store.go for the interface contract.

func volumeDefSelect(d Dialect) string {
	return `SELECT tenant_id, name, ` + d.JSONText("definition") + `, created_at, updated_at FROM volume_defs`
}

func volumeDefInsert(d Dialect) string {
	return `INSERT INTO volume_defs (tenant_id, name, definition, created_at, updated_at)
		 VALUES (?, ?, ` + d.JSONArg() + `, ?, ?)`
}

// VolumeDefCreate UPSERTs the (tenant_id, name) row. Re-creating an existing
// name UPDATES the definition (so the tool's "different mode updates"
// semantics hold) and bumps updated_at, leaving created_at intact. The
// timestamps are stamped here (server clock) so the caller never supplies
// wall-clock fields. On Postgres the definition is jsonb, so it reads back
// jsonb-normalized — fine: the tool re-derives it, never byte-compares.
func VolumeDefCreate(ctx context.Context, q Querier, row store.VolumeDefRow) (store.VolumeDefRow, error) {
	d := q.Dialect()
	now := d.TimeArg(time.Now().UTC())
	if _, err := q.Exec(ctx,
		volumeDefInsert(d)+`
		 ON CONFLICT (tenant_id, name) DO UPDATE SET
		     definition = excluded.definition,
		     updated_at = excluded.updated_at`,
		row.TenantID, row.Name, string(row.Definition), now, now,
	); err != nil {
		return store.VolumeDefRow{}, err
	}
	// Re-read so created_at reflects the original create on an update (the
	// ON CONFLICT branch does not touch created_at).
	return VolumeDefGetByName(ctx, q, row.TenantID, row.Name)
}

func scanVolumeDef(d Dialect, sc Row) (store.VolumeDefRow, error) {
	var (
		r          store.VolumeDefRow
		definition string
	)
	if err := sc.Scan(&r.TenantID, &r.Name, &definition,
		d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt)); err != nil {
		return store.VolumeDefRow{}, err
	}
	r.Definition = json.RawMessage(definition)
	return r, nil
}

func collectVolumeDefs(d Dialect, rows Rows) ([]store.VolumeDefRow, error) {
	defer rows.Close()
	var out []store.VolumeDefRow
	for rows.Next() {
		r, err := scanVolumeDef(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func VolumeDefGetByName(ctx context.Context, q Querier, tenantID, name string) (store.VolumeDefRow, error) {
	d := q.Dialect()
	r, err := scanVolumeDef(d, q.QueryRow(ctx,
		volumeDefSelect(d)+` WHERE tenant_id = ? AND name = ?`, tenantID, name))
	if err != nil {
		return store.VolumeDefRow{}, NotFound(err, "volume_def", name)
	}
	return r, nil
}

func VolumeDefList(ctx context.Context, q Querier, tenantID string) ([]store.VolumeDefRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, volumeDefSelect(d)+` WHERE tenant_id = ? ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	return collectVolumeDefs(d, rows)
}

func VolumeDefDelete(ctx context.Context, q Querier, tenantID, name string) (bool, error) {
	n, err := q.Exec(ctx, `DELETE FROM volume_defs WHERE tenant_id = ? AND name = ?`, tenantID, name)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SnapshotReadVolumeDefs returns every tenant's volume rows, ordered by
// (tenant_id, name).
func SnapshotReadVolumeDefs(ctx context.Context, q Querier) ([]store.VolumeDefRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, volumeDefSelect(d)+` ORDER BY tenant_id, name`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read volume_defs: %w", err)
	}
	out, err := collectVolumeDefs(d, rows)
	if err != nil {
		return nil, fmt.Errorf("snapshot read volume_defs: %w", err)
	}
	return out, nil
}

// SnapshotRestoreVolumeDef inserts one volume row keeping both timestamps; a
// volume already on (tenant_id, name) is left alone. The conflict target is
// the key, not SQLite's INSERT OR IGNORE, which would also swallow a NOT NULL
// failure as "already here".
func SnapshotRestoreVolumeDef(ctx context.Context, q Querier, row store.VolumeDefRow) (bool, error) {
	if row.Name == "" {
		return false, fmt.Errorf("snapshot restore volume_defs: name required")
	}
	d := q.Dialect()
	created, updated := row.CreatedAt, row.UpdatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if updated.IsZero() {
		updated = created
	}
	n, err := q.Exec(ctx,
		volumeDefInsert(d)+`
		 ON CONFLICT (tenant_id, name) DO NOTHING`,
		row.TenantID, row.Name, string(row.Definition), d.TimeArg(created), d.TimeArg(updated),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore volume_defs: %w", err)
	}
	return n > 0, nil
}

// ---- RFC AH Phase 2b ephemeral (run-tree-scoped) volumes ----

func ephemeralSelect(d Dialect) string {
	return `SELECT root_run_id, name, tenant_id, ` + d.JSONText("definition") + `, created_at FROM ephemeral_volume_defs`
}

// jsonModeIsRO is the SQL test "the JSON document expr has mode ro".
func jsonModeIsRO(d Dialect, expr string) string {
	if d == Postgres {
		return expr + `->>'mode' = 'ro'`
	}
	return `json_extract(` + expr + `, '$.mode') = 'ro'`
}

// jsonSetModeRO is the JSON document expr with its mode set to ro.
func jsonSetModeRO(d Dialect, expr string) string {
	if d == Postgres {
		return `jsonb_set(` + expr + `, '{mode}', '"ro"')`
	}
	return `json_set(` + expr + `, '$.mode', 'ro')`
}

// EphemeralVolumeCreate UPSERTs the (root_run_id, name) row. Re-creating an
// existing name within the same run updates the definition and leaves
// created_at intact (mirrors VolumeDefCreate). created_at is stamped here
// (server clock).
//
// One thing an update never does: turn a read-only volume writable. Read-only
// is one-way (EphemeralVolumeSetReadOnly), and this is the only other writer
// of the row, so the rule is kept here and not left to each caller.
func EphemeralVolumeCreate(ctx context.Context, q Querier, row store.EphemeralVolumeDefRow) (store.EphemeralVolumeDefRow, error) {
	d := q.Dialect()
	if _, err := q.Exec(ctx,
		`INSERT INTO ephemeral_volume_defs (root_run_id, name, tenant_id, definition, created_at)
		 VALUES (?, ?, ?, `+d.JSONArg()+`, ?)
		 ON CONFLICT (root_run_id, name) DO UPDATE SET
		     tenant_id  = excluded.tenant_id,
		     definition = CASE
		         WHEN `+jsonModeIsRO(d, "ephemeral_volume_defs.definition")+`
		         THEN `+jsonSetModeRO(d, "excluded.definition")+`
		         ELSE excluded.definition END`,
		row.RootRunID, row.Name, row.TenantID, string(row.Definition), d.TimeArg(time.Now().UTC()),
	); err != nil {
		return store.EphemeralVolumeDefRow{}, err
	}
	return ephemeralVolumeGet(ctx, q, row.RootRunID, row.Name)
}

func scanEphemeralVolume(d Dialect, sc Row) (store.EphemeralVolumeDefRow, error) {
	var (
		r          store.EphemeralVolumeDefRow
		definition string
	)
	if err := sc.Scan(&r.RootRunID, &r.Name, &r.TenantID, &definition, d.TimeDest(&r.CreatedAt)); err != nil {
		return store.EphemeralVolumeDefRow{}, err
	}
	r.Definition = json.RawMessage(definition)
	return r, nil
}

func collectEphemeralVolumes(d Dialect, rows Rows) ([]store.EphemeralVolumeDefRow, error) {
	defer rows.Close()
	var out []store.EphemeralVolumeDefRow
	for rows.Next() {
		r, err := scanEphemeralVolume(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ephemeralVolumeGet is a by-PK read used to re-read after an upsert (so
// created_at reflects the original create on an update).
func ephemeralVolumeGet(ctx context.Context, q Querier, rootRunID, name string) (store.EphemeralVolumeDefRow, error) {
	d := q.Dialect()
	r, err := scanEphemeralVolume(d, q.QueryRow(ctx,
		ephemeralSelect(d)+` WHERE root_run_id = ? AND name = ?`, rootRunID, name))
	if err != nil {
		return store.EphemeralVolumeDefRow{}, NotFound(err, "ephemeral_volume_def", name)
	}
	return r, nil
}

func EphemeralVolumeListByRun(ctx context.Context, q Querier, rootRunID string) ([]store.EphemeralVolumeDefRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, ephemeralSelect(d)+` WHERE root_run_id = ? ORDER BY name`, rootRunID)
	if err != nil {
		return nil, err
	}
	return collectEphemeralVolumes(d, rows)
}

// EphemeralVolumeSetReadOnly sets one row's mode to "ro" and reports whether
// the row exists.
func EphemeralVolumeSetReadOnly(ctx context.Context, q Querier, rootRunID, name string) (bool, error) {
	n, err := q.Exec(ctx,
		`UPDATE ephemeral_volume_defs SET definition = `+jsonSetModeRO(q.Dialect(), "definition")+`
		  WHERE root_run_id = ? AND name = ?`, rootRunID, name)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// EphemeralVolumeListByTenant returns the tenant's LIVE ephemeral rows (RFC
// AH Phase 4 Web UI), ordered by (root_run_id, name) for a stable view.
// Tenant-scoped at the query, so a tenant never sees another's rows.
func EphemeralVolumeListByTenant(ctx context.Context, q Querier, tenantID string) ([]store.EphemeralVolumeDefRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, ephemeralSelect(d)+` WHERE tenant_id = ? ORDER BY root_run_id, name`, tenantID)
	if err != nil {
		return nil, err
	}
	return collectEphemeralVolumes(d, rows)
}

func EphemeralVolumeDeleteByRun(ctx context.Context, q Querier, rootRunID string) (int, error) {
	n, err := q.Exec(ctx, `DELETE FROM ephemeral_volume_defs WHERE root_run_id = ?`, rootRunID)
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
//
// A tree with a run still running beneath its root is not a candidate either:
// a detached team walk keeps its starter's root run id and outlives the root,
// so its members use the tree after the root ends. live walks UP from every
// running run through parent_run_id (point reads on the primary key) and
// yields one (id, parent) row per link, so a root with a run beneath it is
// some row's parent. UNION, not UNION ALL: a parent chain that loops back on
// itself repeats a row and the recursion stops. Its seed reads the running
// runs, the read SweepStaleRuns makes on every tick of its own.
func EphemeralVolumeSweepCandidates(ctx context.Context, q Querier) ([]store.EphemeralVolumeSweepRow, error) {
	args := append([]any{string(store.RunRunning)}, store.TerminalRunStatusArgs()...)
	rows, err := q.Query(ctx,
		`WITH RECURSIVE live(id, parent) AS (
		     SELECT id, parent_run_id FROM runs
		      WHERE status = ? AND parent_run_id IS NOT NULL
		   UNION
		     SELECT p.id, p.parent_run_id FROM runs p JOIN live l ON p.id = l.parent
		      WHERE p.parent_run_id IS NOT NULL
		 )
		 SELECT DISTINCT e.root_run_id, e.tenant_id
		 FROM ephemeral_volume_defs e
		 JOIN runs r ON r.id = e.root_run_id
		 WHERE r.status IN (`+Placeholders(len(store.TerminalRunStatuses))+`)
		   AND COALESCE(r.pause_state, 'running') NOT IN ('paused', 'pausing')
		   AND NOT EXISTS (SELECT 1 FROM live WHERE live.parent = e.root_run_id)`, args...)
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
