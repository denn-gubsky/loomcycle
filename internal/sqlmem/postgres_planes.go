package sqlmem

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// postgres_planes.go — moving a scope's Document tables from its agent plane,
// where every scope created before the planes split keeps them, to its doc plane
// (see scopePlane).
//
// WHY ROWS ARE COPIED, NOT THE TABLES MOVED. Before the split the agent's role
// owned these tables, so an agent could have added a trigger, a rule, a row
// policy, a column DEFAULT, a CHECK or an index expression calling a function of
// its own. ALTER TABLE … SET SCHEMA keeps all of that, and once the Document
// tool's role writes the table, those functions run with its rights: every
// statement an agent could not run itself. So the doc plane gets fresh tables
// from the Document tool's own DDL, the rows are read as the agent's role (its
// own data, its own rights) and written as the doc role cast to the doc table's
// column types, and the agent tables are dropped with everything attached.

// pgAdoptTimeout bounds one scope's move. It is detached from the op that
// triggered it: a move abandoned by a short request deadline would only be
// retried by the next one.
const pgAdoptTimeout = 10 * time.Minute

// pgAdoptBatchParams bounds the bind parameters of one INSERT (postgres allows
// 65535).
const pgAdoptBatchParams = 30000

// pgTableNameRe is the shape a table name must have before it is quoted into a
// statement. The caller passes its own table names; this is belt-and-braces.
var pgTableNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// agentTables returns which of tables exist in the scope's agent schema.
func (b *postgresBackend) agentTables(ctx context.Context, key ScopeKey, tables []string) ([]string, error) {
	schema, _, err := pgScopeNames(key.wholeScope())
	if err != nil {
		return nil, err
	}
	rows, err := b.admin.QueryContext(ctx,
		`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relkind IN ('r','p')`, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: list agent-plane tables: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tables {
		if have[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// adoptTables moves the named tables of the scope's agent plane to its doc plane
// (Manager.AdoptAgentTables). One mover per scope at a time, across replicas:
// the others wait on the lock and then find nothing left to move.
func (b *postgresBackend) adoptTables(ctx context.Context, key ScopeKey, tables []string) error {
	for _, t := range tables {
		if !pgTableNameRe.MatchString(t) {
			return fmt.Errorf("sqlmem: %q is not a table name", t)
		}
	}
	agentKey := key.wholeScope()
	docKey := key
	docKey.plane = planeDoc
	aSchema, aRole, err := pgScopeNames(agentKey)
	if err != nil {
		return err
	}
	dSchema, dRole, err := pgScopeNames(docKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pgAdoptTimeout)
	defer cancel()

	lock, err := b.admin.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlmem: move document tables: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if _, err := lock.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, aSchema); err != nil {
		return fmt.Errorf("sqlmem: move document tables: lock: %w", err)
	}
	defer func() {
		_, _ = lock.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, aSchema)
	}()

	held, err := b.agentTables(ctx, agentKey, tables)
	if err != nil {
		return err
	}
	for _, t := range held {
		if err := b.adoptTable(ctx, aSchema, aRole, dSchema, dRole, t); err != nil {
			return fmt.Errorf("sqlmem: move %s to the document plane: %w", t, err)
		}
	}
	return nil
}

// pgColumn is one column of a table, with its type as format_type renders it.
type pgColumn struct{ name, typ string }

// pgColumns lists a table's live, non-generated columns in order. Empty when the
// table does not exist.
func pgColumns(ctx context.Context, db *sql.DB, schema, table string) ([]pgColumn, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT a.attname, format_type(a.atttypid, a.atttypmod)
		   FROM pg_attribute a
		   JOIN pg_class c ON c.oid = a.attrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0
		    AND NOT a.attisdropped AND a.attgenerated = ''
		  ORDER BY a.attnum`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pgColumn
	for rows.Next() {
		var c pgColumn
		if err := rows.Scan(&c.name, &c.typ); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// adoptTable moves one table: copy its rows into the doc plane's table of the same
// name unless that one already holds rows, then drop the agent copy.
func (b *postgresBackend) adoptTable(ctx context.Context, aSchema, aRole, dSchema, dRole, table string) error {
	b.logPlanted(ctx, aSchema, table)
	docCols, err := pgColumns(ctx, b.admin, dSchema, table)
	if err != nil {
		return err
	}
	if len(docCols) == 0 {
		// Not a table the Document tool creates any more (a migration's scratch
		// table, say): nothing on the doc plane takes its rows.
		slog.Warn("sqlmem: dropped a document table with no doc-plane counterpart", "schema", aSchema, "table", table)
		return b.dropAgentTable(ctx, aSchema, table)
	}
	agentCols, err := pgColumns(ctx, b.admin, aSchema, table)
	if err != nil {
		return err
	}
	inAgent := map[string]bool{}
	for _, c := range agentCols {
		inAgent[c.name] = true
	}
	var cols []pgColumn
	for _, c := range docCols {
		if inAgent[c.name] {
			cols = append(cols, c)
		}
	}
	inDoc := map[string]bool{}
	for _, c := range docCols {
		inDoc[c.name] = true
	}
	for _, c := range agentCols {
		if !inDoc[c.name] {
			slog.Warn("sqlmem: dropped a column an agent added to a document table", "schema", aSchema, "table", table, "column", c.name)
		}
	}

	dsc, err := b.acquireScope(dRole)
	if err != nil {
		return err
	}
	defer b.releaseScope(dsc)
	empty, err := pgTableIsEmpty(ctx, dsc.db, dSchema, table)
	if err != nil {
		return err
	}
	if !empty {
		var n int64
		_ = b.admin.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.%s`, q(aSchema), q(table))).Scan(&n)
		slog.Warn("sqlmem: discarded an agent-plane copy of a document table; the document plane already holds rows",
			"schema", aSchema, "table", table, "rows", n)
		return b.dropAgentTable(ctx, aSchema, table)
	}
	if len(cols) > 0 {
		if err := b.copyRows(ctx, aSchema, aRole, dSchema, dsc, table, cols); err != nil {
			return err
		}
	}
	return b.dropAgentTable(ctx, aSchema, table)
}

// copyRows streams a table's rows from the agent plane, read as the agent's role,
// into the doc plane's table in one transaction as the doc role. Every value
// crosses as text and is cast to the doc column's type, so only built-in
// conversions run.
func (b *postgresBackend) copyRows(ctx context.Context, aSchema, aRole, dSchema string, dsc *scopeConn, table string, cols []pgColumn) error {
	asc, err := b.acquireScope(aRole)
	if err != nil {
		return err
	}
	defer b.releaseScope(asc)
	rtx, err := asc.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = rtx.Rollback() }()
	wtx, err := dsc.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = wtx.Rollback() }()
	// The roles' baked statement_timeout bounds an agent's statement; a one-time
	// move of a large table is bounded by pgAdoptTimeout instead.
	for _, tx := range []*sql.Tx{rtx, wtx} {
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
			return err
		}
	}

	sel := make([]string, len(cols))
	names := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = q(c.name) + "::text"
		names[i] = q(c.name)
	}
	rows, err := rtx.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s.%s`, strings.Join(sel, ", "), q(aSchema), q(table)))
	if err != nil {
		return err
	}
	defer rows.Close()

	perBatch := max(1, pgAdoptBatchParams/len(cols))
	head := fmt.Sprintf(`INSERT INTO %s.%s (%s) VALUES `, q(dSchema), q(table), strings.Join(names, ", "))
	var tuples []string
	var args []any
	flush := func() error {
		if len(tuples) == 0 {
			return nil
		}
		_, err := wtx.ExecContext(ctx, head+strings.Join(tuples, ", "), args...)
		tuples, args = tuples[:0], args[:0]
		return err
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		ph := make([]string, len(cols))
		for i, c := range cols {
			args = append(args, nullable(vals[i]))
			ph[i] = fmt.Sprintf("$%d::%s", len(args), c.typ)
		}
		tuples = append(tuples, "("+strings.Join(ph, ", ")+")")
		if len(tuples) >= perBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return wtx.Commit()
}

func nullable(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// dropAgentTable drops the agent plane's copy of a document table, with whatever
// was attached to it. The admin owns the scope schema, which is what lets it drop
// a table the agent's role owns.
func (b *postgresBackend) dropAgentTable(ctx context.Context, aSchema, table string) error {
	_, err := b.admin.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s CASCADE`, q(aSchema), q(table)))
	return err
}

// logPlanted logs each trigger, rule, row policy and view an agent attached to a
// document table before the planes split. The drop removes them all; the log is
// the operator's record of what was there.
func (b *postgresBackend) logPlanted(ctx context.Context, aSchema, table string) {
	rel := q(aSchema) + "." + q(table)
	byRel, byName := []any{rel}, []any{aSchema, table}
	for _, probe := range []struct {
		kind, sql string
		args      []any
	}{
		{"trigger", `SELECT tgname FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal`, byRel},
		{"rule", `SELECT rulename FROM pg_rules WHERE schemaname = $1 AND tablename = $2`, byName},
		{"policy", `SELECT policyname FROM pg_policies WHERE schemaname = $1 AND tablename = $2`, byName},
		{"view", `SELECT DISTINCT v.relname FROM pg_depend d
		            JOIN pg_rewrite r ON r.oid = d.objid
		            JOIN pg_class v ON v.oid = r.ev_class
		           WHERE d.refobjid = $1::regclass AND v.oid <> $1::regclass`, byRel},
	} {
		rows, err := b.admin.QueryContext(ctx, probe.sql, probe.args...)
		if err != nil {
			slog.Warn("sqlmem: could not list what an agent attached to a document table", "schema", aSchema, "table", table, "kind", probe.kind, "err", err)
			continue
		}
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				slog.Warn("sqlmem: dropped an object an agent attached to a document table",
					"schema", aSchema, "table", table, "kind", probe.kind, "name", name)
			}
		}
		rows.Close()
	}
}
