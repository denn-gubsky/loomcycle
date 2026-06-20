package sqlmem

import (
	"context"
	"fmt"
)

// dump.go — RFC AA Phase 3e: logical export/import of a scope's schema + data,
// the seam the snapshot/backup subsystem rides on. A dump is DDL (CREATE
// statements) plus per-table data; restoring it replays the DDL + INSERTs
// through the SAME validated/provisioned scope path a live agent uses, so a
// restored scope is byte-for-byte a scope the runtime created itself. We dump
// LOGICALLY (not raw .db bytes / a pg basebackup) so a snapshot is portable
// across tiers' storage layouts and survives the cross-version migration
// registry the snapshot envelope already provides.
//
// Only DURABLE scopes (agent/user) are dumpable: the ephemeral run scope is
// dropped at run-end and has no place in a portable archive. ListScopes never
// returns a run scope.

// ScopeDump is the logical export of one scope database: its schema DDL plus
// every user table's data. DDL is the pre-data schema (sequences → tables →
// non-FK constraints → indexes), applied before the rows. PostDDL holds the
// foreign-key constraints, applied AFTER data so cross-table insert order is
// irrelevant (postgres tier; sqlite defers FK checks inside the load txn and
// leaves PostDDL empty). Tables is the data, in table dependency-free order.
type ScopeDump struct {
	DDL     []string
	PostDDL []string
	Tables  []TableDump
}

// TableDump is one table's data. ColumnTypes is populated only by the postgres
// tier (the per-column type used to cast text values back on restore); the
// sqlite tier leaves it nil and binds native Go values directly.
//
// Rows hold JSON-round-trippable values: the postgres tier reads every column
// as ::text so each value is a string or nil; the sqlite tier keeps native
// values (string / float64 / nil) and wraps a non-UTF-8 BLOB as the tagged
// form map[string]any{"$b64": <base64>} so it survives JSON.
type TableDump struct {
	Name        string
	Columns     []string
	ColumnTypes []string
	Rows        [][]any
}

// b64Key is the marshal tag for a binary (non-UTF-8) value inside Rows. A
// JSON object with exactly this one key round-trips a sqlite BLOB.
const b64Key = "$b64"

// Tier reports the storage tier backing this Manager — "sqlite" or "postgres".
// The snapshot stamps it on the section so Restore can skip a cross-tier
// archive (the logical shapes differ: postgres carries per-column cast types).
func (m *Manager) Tier() string {
	if m.dialect == dialectPostgres {
		return "postgres"
	}
	return "sqlite"
}

// ListScopes enumerates every DURABLE (agent/user) scope the tier currently
// holds, for snapshot capture. The run scope is never included.
func (m *Manager) ListScopes(ctx context.Context) ([]ScopeKey, error) {
	return m.backend.listScopes(ctx)
}

// ExportScope produces the logical dump (DDL + data) for one durable scope.
// It pins the scope for the read (an in-flight op / open transaction is
// respected), so a concurrent eviction or GC drop cannot tear it out mid-dump.
func (m *Manager) ExportScope(ctx context.Context, key ScopeKey) (*ScopeDump, error) {
	if key.Scope == runScope {
		return nil, fmt.Errorf("sqlmem: the run scope is not exportable")
	}
	return m.backend.exportScope(ctx, key)
}

// RestoreScope materializes a scope from a dump: it provisions the scope (so
// the schema/role/file exist exactly as a live op would create them), replays
// the DDL (already-exists tolerated → idempotent schema), and loads each
// table's data — skipping any table that is already non-empty so a second
// Restore on the same archive is a clean no-op. The dump's statements run
// through the same per-scope isolation a live agent's SQL does.
func (m *Manager) RestoreScope(ctx context.Context, key ScopeKey, dump *ScopeDump) error {
	if dump == nil {
		return nil
	}
	if key.Scope == runScope {
		return fmt.Errorf("sqlmem: the run scope is not restorable")
	}
	return m.backend.restoreScope(ctx, key, dump)
}
