package sqlmem

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// dump_sqlite.go — the sqlite tier of the snapshot dump seam (RFC AA Phase 3e).
// listScopes recovers scope identity from the on-disk path layout; export/
// restore move a scope's schema + data via sqlite_master + plain SELECT/INSERT.

// listScopes walks the durable scope tree and recovers each scope's ScopeKey
// from its path: <root>/<tenant>/<scope>/<id>.db. The run subtree (<root>/run,
// which is NOT tenant-keyed) is skipped — run scopes are never snapshotted.
//
// Identity recovery from the path is exact for the validated key space: tenant/
// scope/scope_id are charset-validated upstream ([A-Za-z0-9_-]), and sanitize is
// a no-op on that charset — so the path segments ARE the original components. A
// component that did carry an out-of-charset character would be recovered in its
// sanitized form, which is still a STABLE identity (sanitize is idempotent on
// its own output: a restore re-derives the same file), so the scope round-trips
// regardless; only the literal pre-sanitize string would differ.
func (b *sqliteBackend) listScopes(ctx context.Context) ([]ScopeKey, error) {
	runDir := filepath.Join(b.cfg.Root, runScope)
	var out []ScopeKey
	err := filepath.WalkDir(b.cfg.Root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if path == runDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".db") {
			return nil // skip -wal/-shm and non-db files
		}
		rel, rerr := filepath.Rel(b.cfg.Root, path)
		if rerr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 {
			return nil // a durable scope is exactly <tenant>/<scope>/<id>.db
		}
		out = append(out, ScopeKey{
			Tenant:  parts[0],
			Scope:   parts[1],
			ScopeID: strings.TrimSuffix(parts[2], ".db"),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlmem: list scopes: %w", err)
	}
	return out, nil
}

// exportScope dumps the scope's schema (CREATE statements from sqlite_master,
// tables before indexes) and every user table's rows.
func (b *sqliteBackend) exportScope(ctx context.Context, key ScopeKey) (*ScopeDump, error) {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return nil, err
	}
	db, err := b.acquire(path)
	if err != nil {
		return nil, err
	}
	defer b.release(path)

	dump := &ScopeDump{}

	// Schema DDL: the verbatim CREATE statements. Tables first (type ordering),
	// then explicit indexes; auto-indexes from PK/UNIQUE have sql IS NULL and are
	// excluded (the table DDL recreates them). sqlite_% internal objects skipped.
	ddlRows, err := db.QueryContext(ctx,
		`SELECT sql FROM sqlite_master
		   WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		   ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END, name`)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read schema: %w", err)
	}
	for ddlRows.Next() {
		var s string
		if err := ddlRows.Scan(&s); err != nil {
			ddlRows.Close()
			return nil, err
		}
		dump.DDL = append(dump.DDL, s)
	}
	if err := ddlRows.Err(); err != nil {
		ddlRows.Close()
		return nil, err
	}
	ddlRows.Close()

	// Table list (data order = name order; FK-free within a single agent scope).
	var tables []string
	tblRows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read table list: %w", err)
	}
	for tblRows.Next() {
		var n string
		if err := tblRows.Scan(&n); err != nil {
			tblRows.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	if err := tblRows.Err(); err != nil {
		tblRows.Close()
		return nil, err
	}
	tblRows.Close()

	for _, t := range tables {
		td, err := b.exportTableSQLite(ctx, db, t)
		if err != nil {
			return nil, err
		}
		dump.Tables = append(dump.Tables, *td)
	}
	return dump, nil
}

// exportTableSQLite reads one table's columns + rows, JSON-normalizing each
// value (UTF-8 []byte → string; binary []byte → the $b64 tagged form).
func (b *sqliteBackend) exportTableSQLite(ctx context.Context, db *sql.DB, table string) (*TableDump, error) {
	rows, err := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read table %q: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	td := &TableDump{Name: table, Columns: cols}
	for rows.Next() {
		scan := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range scan {
			ptrs[i] = &scan[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range scan {
			if bs, ok := v.([]byte); ok {
				if utf8.Valid(bs) {
					scan[i] = string(bs)
				} else {
					scan[i] = map[string]any{b64Key: base64.StdEncoding.EncodeToString(bs)}
				}
			}
		}
		td.Rows = append(td.Rows, scan)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return td, nil
}

// restoreScope materializes the scope: acquire (creates the .db file), replay
// the DDL (already-exists tolerated), then load each table that is currently
// empty (so a re-restore is a no-op). The data load runs in one transaction
// with PRAGMA defer_foreign_keys=ON so cross-table insert order can't trip a
// foreign-key check before its parent row lands (verified at COMMIT instead).
func (b *sqliteBackend) restoreScope(ctx context.Context, key ScopeKey, dump *ScopeDump) error {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return err
	}
	db, err := b.acquire(path)
	if err != nil {
		return err
	}
	defer b.release(path)

	for _, stmt := range append(append([]string{}, dump.DDL...), dump.PostDDL...) {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isSQLiteAlreadyExists(err) {
			return fmt.Errorf("sqlmem: restore DDL: %w", err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return fmt.Errorf("sqlmem: defer FK: %w", err)
	}
	for _, t := range dump.Tables {
		empty, err := tableIsEmpty(ctx, tx, t.Name)
		if err != nil {
			return err
		}
		if !empty {
			continue // already populated → idempotent skip
		}
		if err := insertRows(ctx, tx, t, "?"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlmem: restore commit: %w", err)
	}
	committed = true
	return nil
}

// isSQLiteAlreadyExists reports the modernc "table/index already exists" error
// string (the pure-Go driver surfaces it as text, not a typed code).
func isSQLiteAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// tableIsEmpty reports whether the named table currently has zero rows.
func tableIsEmpty(ctx context.Context, q rowQueryer, table string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" LIMIT 1`).Scan(&one)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlmem: check table %q: %w", table, err)
	}
	return false, nil
}

// insertRows inserts every dumped row into the table. placeholder is "?"
// (sqlite) or "" to request $N positional placeholders (postgres, via
// insertRowsPG). Values are run through bindValue so a $b64-tagged binary value
// is decoded back to []byte.
func insertRows(ctx context.Context, q execer, t TableDump, placeholder string) error {
	if len(t.Rows) == 0 {
		return nil
	}
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
	}
	marks := make([]string, len(t.Columns))
	for i := range marks {
		marks[i] = placeholder
	}
	stmt := fmt.Sprintf(`INSERT INTO "%s" (%s) VALUES (%s)`,
		strings.ReplaceAll(t.Name, `"`, `""`), strings.Join(cols, ", "), strings.Join(marks, ", "))
	for _, row := range t.Rows {
		args := make([]any, len(row))
		for i, v := range row {
			b, err := bindValue(v)
			if err != nil {
				return fmt.Errorf("sqlmem: restore row in %q: %w", t.Name, err)
			}
			args[i] = b
		}
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("sqlmem: restore insert into %q: %w", t.Name, err)
		}
	}
	return nil
}

// execer is satisfied by *sql.DB and *sql.Tx — insertRows runs on either.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// bindValue converts a dumped (possibly JSON-round-tripped) value into a driver
// argument. The only special case is the $b64 tagged binary form, decoded to
// []byte; everything else (string / float64 / int64 / bool / nil) binds as-is.
func bindValue(v any) (any, error) {
	if m, ok := v.(map[string]any); ok {
		if enc, ok := m[b64Key].(string); ok {
			raw, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				return nil, fmt.Errorf("decode binary value: %w", err)
			}
			return raw, nil
		}
	}
	return v, nil
}
