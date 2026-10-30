package sqlmem

import (
	"context"
	"fmt"
	"strings"
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

// b64Key tags a binary (non-UTF-8) value inside Rows; intKey tags a sqlite
// INTEGER as a decimal string. Both are needed because JSON has no byte type and
// no int64 type — a bare JSON number decodes to float64, which silently loses
// precision above 2^53 and re-binds an integer as REAL (changing typeof). A
// {"$int": "<decimal>"} / {"$b64": "<base64>"} object round-trips exactly and
// preserves the storage class. Used only by the sqlite tier (postgres reads
// every column as ::text, so its values are already plain strings).
const (
	b64Key = "$b64"
	intKey = "$int"
)

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
	// A colliding key would dump ANOTHER scope's rows into this scope's archive,
	// which is worse than a failed export: the wrong data ends up in a backup and
	// is replayed on restore.
	if err := key.validate(); err != nil {
		return nil, err
	}
	dump, err := m.backend.exportScope(ctx, key.wholeScope())
	if err != nil || m.dialect != dialectPostgres {
		return dump, err
	}
	// On postgres the Document tool's tables live on their own plane. They go into
	// the same dump, so an archive restores on a runtime of any version: restored
	// into the agent plane, they are moved over by the Document tool's first use of
	// the scope, exactly like a scope from before the split.
	docKey := key
	docKey.plane = planeDoc
	held, err := m.backend.scopeExists(ctx, docKey)
	if err != nil || !held {
		return dump, err
	}
	doc, err := m.backend.exportScope(ctx, docKey)
	if err != nil {
		return nil, err
	}
	// The export qualifies every statement with its schema. The doc plane's DDL
	// is replayed into the agent plane, so it names that schema instead: the two
	// names differ only in their prefix around one 128-bit hash, so the rename
	// cannot touch anything else.
	agentSchema, _, err := pgScopeNames(key.wholeScope())
	if err != nil {
		return nil, err
	}
	docSchema, _, err := pgScopeNames(docKey)
	if err != nil {
		return nil, err
	}
	for _, stmts := range [][]string{doc.DDL, doc.PostDDL} {
		for i, stmt := range stmts {
			stmts[i] = strings.ReplaceAll(stmt, docSchema, agentSchema)
		}
	}
	dump.DDL = append(dump.DDL, doc.DDL...)
	dump.Tables = append(dump.Tables, doc.Tables...)
	dump.PostDDL = append(dump.PostDDL, doc.PostDDL...)
	return dump, nil
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
	// Restore provisions the scope before replaying DDL, so a key that arrived in a
	// snapshot is untrusted input to the same derivation Begin/Exec guard.
	if err := key.validate(); err != nil {
		return err
	}
	if key.Scope == runScope {
		return fmt.Errorf("sqlmem: the run scope is not restorable")
	}
	return m.backend.restoreScope(ctx, key.wholeScope(), dump)
}
