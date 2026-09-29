package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"slices"
	"strings"
)

// tenantBlindLegacyKeys lists every table whose key gained tenant_id AFTER the
// table first shipped, with the tenant-blind key it had before.
//
// SQLite cannot alter a PRIMARY KEY or a table UNIQUE constraint, so the tenant
// migrations only added the column plus a (tenant_id, …) unique index. On a
// database created before those migrations the old key is still enforced, and it
// fires first: a second tenant writing the same name/cursor/memory key fails with
// "UNIQUE constraint failed" even though the tenant-leading index would admit it
// (or, on an INSERT OR IGNORE restore path, is silently dropped). The rebuild
// below replaces such a table with its current CREATE TABLE.
//
// The legacy keys come from the CREATE TABLE text each table had before its
// tenant migration (git history of sqlite.go). Order is parents before the
// tables that reference them; with foreign keys off during the rebuild it is
// not load-bearing, but it keeps the copy in dependency order.
var tenantBlindLegacyKeys = []struct {
	table string
	key   []string
}{
	{"agent_defs", []string{"name", "version"}},
	{"agent_def_active", []string{"name"}},
	{"dynamic_agents", []string{"name"}},
	{"skill_defs", []string{"name", "version"}},
	{"skill_def_active", []string{"name"}},
	{"mcp_server_defs", []string{"name", "version"}},
	{"mcp_server_def_active", []string{"name"}},
	{"memory_backend_defs", []string{"name", "version"}},
	{"memory_backend_def_active", []string{"name"}},
	{"a2a_agent_defs", []string{"name", "version"}},
	{"a2a_agent_def_active", []string{"name"}},
	{"schedule_defs", []string{"name", "version"}},
	{"schedule_def_active", []string{"name"}},
	{"a2a_server_card_defs", []string{"name", "version"}},
	{"a2a_server_card_def_active", []string{"name"}},
	{"webhook_defs", []string{"name", "version"}},
	{"webhook_def_active", []string{"name"}},
	{"channel_messages", []string{"channel", "scope", "scope_id", "id"}},
	{"channel_cursors", []string{"channel", "scope", "scope_id"}},
	{"channels", []string{"name"}},
	{"memory", []string{"scope", "scope_id", "key"}},
}

// sqlQueryer is the read surface shared by *sql.DB, *sql.Conn and *sql.Tx.
type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// tenantKeyRebuildCandidates returns the tables that still enforce their
// tenant-blind legacy key. It inspects the live schema rather than trusting a
// version marker, so a fresh database and an already-rebuilt one both yield
// nothing — which is what makes the rebuild a no-op on every later boot.
func tenantKeyRebuildCandidates(ctx context.Context, q sqlQueryer) ([]string, error) {
	var out []string
	for _, lk := range tenantBlindLegacyKeys {
		has, err := hasConstraintKey(ctx, q, lk.table, lk.key)
		if err != nil {
			return nil, err
		}
		if has {
			out = append(out, lk.table)
		}
	}
	return out, nil
}

// hasConstraintKey reports whether table has a PRIMARY KEY or UNIQUE constraint
// on exactly cols. Only constraint-backed indexes (origin pk/u) count: the
// tenant-leading CREATE UNIQUE INDEX the migrations added has origin c and is
// not a key the table was declared with.
func hasConstraintKey(ctx context.Context, q sqlQueryer, table string, cols []string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM pragma_index_list(?) WHERE "unique" = 1 AND origin IN ('pk', 'u')`, table)
	if err != nil {
		return false, fmt.Errorf("index_list %s: %w", table, err)
	}
	var indexes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return false, err
		}
		indexes = append(indexes, n)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, idx := range indexes {
		got, err := queryStrings(ctx, q, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, idx)
		if err != nil {
			return false, fmt.Errorf("index_info %s: %w", idx, err)
		}
		if slices.Equal(got, cols) {
			return true, nil
		}
	}
	return false, nil
}

// rebuildTenantBlindKeys replaces every table that still carries a tenant-blind
// legacy key with its current CREATE TABLE, following SQLite's documented
// procedure for schema changes ALTER TABLE cannot make: foreign keys off, then
// in ONE transaction create the new table, copy every row, drop the old one,
// rename the new one into place and recreate the old table's indexes, check that
// no foreign-key violation was introduced, commit.
//
// It must run after the ADD COLUMN block (so every current column exists on the
// old table and can be copied) and before addIndexes (whose indexes then land on
// the rebuilt table). No row can collide under the new key: it is the old key
// plus tenant_id, strictly wider, and every pre-tenant row carries tenant_id ”.
//
// The kept uniq_*_tenant indexes become redundant with the rebuilt key; they are
// recreated anyway (as on a fresh DB, where they duplicate the key too) so the
// ON CONFLICT targets behave the same whatever a database's history.
func (s *Store) rebuildTenantBlindKeys(ctx context.Context, schema []string) error {
	tables, err := tenantKeyRebuildCandidates(ctx, s.db)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil
	}

	// PRAGMA foreign_keys is per connection and a no-op inside a transaction, so
	// pin one connection for the whole procedure. With it on, DROP TABLE would
	// run the implicit DELETE and fire ON DELETE CASCADE (schedule_run_state →
	// schedule_defs) — deleting the rows the rebuild exists to keep.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	rebuildErr := rebuildTables(ctx, conn, tables, schema)
	// The connection returns to the pool afterwards, so enforcement must come
	// back on whether or not the rebuild succeeded.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil && rebuildErr == nil {
		rebuildErr = fmt.Errorf("re-enable foreign keys: %w", err)
	}
	return rebuildErr
}

func rebuildTables(ctx context.Context, conn *sql.Conn, tables, schema []string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The tables whose foreign keys the rebuild could disturb: the rebuilt ones
	// and every table referencing one of them.
	checked := map[string]bool{}
	for _, t := range tables {
		checked[t] = true
		refs, err := queryStrings(ctx, tx,
			`SELECT DISTINCT m.name FROM sqlite_master m, pragma_foreign_key_list(m.name) f
			  WHERE m.type = 'table' AND f."table" = ?`, t)
		if err != nil {
			return fmt.Errorf("list tables referencing %s: %w", t, err)
		}
		for _, r := range refs {
			checked[r] = true
		}
	}
	// Compare against the count before, not zero: a violation that predates the
	// rebuild is not this migration's to judge, and refusing on it would keep
	// the store from opening at all.
	before, err := foreignKeyViolations(ctx, tx, checked)
	if err != nil {
		return err
	}

	kept := make([]int64, len(tables))
	for i, t := range tables {
		n, err := rebuildTable(ctx, tx, t, schema)
		if err != nil {
			return fmt.Errorf("rebuild %s: %w", t, err)
		}
		kept[i] = n
	}

	after, err := foreignKeyViolations(ctx, tx, checked)
	if err != nil {
		return err
	}
	if after > before {
		return fmt.Errorf("rebuild introduced %d foreign key violation(s); rolled back", after-before)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for i, t := range tables {
		log.Printf("sqlite migrate: rebuilt %s with its tenant-scoped key (%d rows kept)", t, kept[i])
	}
	return nil
}

// rebuildTable swaps table for a copy created from its current CREATE TABLE and
// returns the number of rows carried over.
func rebuildTable(ctx context.Context, tx *sql.Tx, table string, schema []string) (int64, error) {
	create, ok := freshCreateTable(schema, table)
	if !ok {
		return 0, fmt.Errorf("no CREATE TABLE for %s in the schema", table)
	}
	tmp := table + "__tenant_rebuild"
	if _, err := tx.ExecContext(ctx, `CREATE TABLE "`+tmp+`"`+create); err != nil {
		return 0, fmt.Errorf("create %s: %w", tmp, err)
	}

	oldCols, err := queryStrings(ctx, tx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return 0, err
	}
	newCols, err := queryStrings(ctx, tx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, tmp)
	if err != nil {
		return 0, err
	}
	inNew := map[string]bool{}
	for _, c := range newCols {
		inNew[c] = true
	}
	// Every existing column must have a home: copying only the shared ones would
	// quietly drop a column the current schema no longer declares. That cannot
	// happen for a schema this code wrote, so refuse rather than guess.
	for _, c := range oldCols {
		if !inNew[c] {
			return 0, fmt.Errorf("column %q is not in the current schema; refusing to drop it", c)
		}
	}

	// Indexes and triggers go with the dropped table; keep their SQL to recreate
	// them. Autoindexes (sql IS NULL) belong to the old constraints and are
	// replaced by the new table's own.
	objects, err := queryStrings(ctx, tx,
		`SELECT sql FROM sqlite_master WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL`, table)
	if err != nil {
		return 0, err
	}

	var want int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&want); err != nil {
		return 0, err
	}
	cols := `"` + strings.Join(oldCols, `", "`) + `"`
	res, err := tx.ExecContext(ctx,
		`INSERT INTO "`+tmp+`" (`+cols+`) SELECT `+cols+` FROM "`+table+`"`)
	if err != nil {
		return 0, fmt.Errorf("copy rows: %w", err)
	}
	got, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if got != want {
		return 0, fmt.Errorf("copied %d of %d rows", got, want)
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE "`+table+`"`); err != nil {
		return 0, fmt.Errorf("drop: %w", err)
	}
	// Drop-then-rename, never rename-then-drop: renaming the old table first
	// would rewrite the other tables' REFERENCES clauses to follow it, and the
	// drop would then leave them pointing at nothing.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE "`+tmp+`" RENAME TO "`+table+`"`); err != nil {
		return 0, fmt.Errorf("rename: %w", err)
	}
	for _, q := range objects {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, fmt.Errorf("recreate %q: %w", q, err)
		}
	}
	return got, nil
}

// freshCreateTable returns the body of table's CREATE TABLE statement in schema
// — everything after the table name — so the rebuild creates the new table from
// the one definition a fresh database gets, not from a second copy that could
// drift from it.
func freshCreateTable(schema []string, table string) (string, bool) {
	const prefix = "CREATE TABLE IF NOT EXISTS "
	for _, q := range schema {
		q = strings.TrimSpace(q)
		if !strings.HasPrefix(q, prefix) {
			continue
		}
		rest := q[len(prefix):]
		end := strings.IndexAny(rest, " (")
		if end < 0 || rest[:end] != table {
			continue
		}
		return rest[end:], true
	}
	return "", false
}

// foreignKeyViolations counts the foreign-key violations whose child row lives in
// one of tables.
func foreignKeyViolations(ctx context.Context, q sqlQueryer, tables map[string]bool) (int, error) {
	n := 0
	for t := range tables {
		rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check("`+t+`")`)
		if err != nil {
			return 0, fmt.Errorf("foreign_key_check %s: %w", t, err)
		}
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func queryStrings(ctx context.Context, q sqlQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
