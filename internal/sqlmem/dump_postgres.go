package sqlmem

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// dump_postgres.go — the postgres tier of the snapshot dump seam (RFC AA Phase
// 3e). listScopes reads the identity registry; export reconstructs a logical
// dump (a mini pg_dump from the catalog) running as the SCOPE ROLE; restore
// replays it as the scope role through the normal provisioned path.
//
// Fidelity scope: tables (columns with type/DEFAULT/NOT NULL/generated-stored),
// enum types, owned sequences (serial round-trips: CREATE SEQUENCE + nextval
// default + setval; custom increment/min/max are NOT preserved),
// PK/UNIQUE/CHECK/exclusion + standalone indexes, and FOREIGN KEY constraints
// (applied after data). Data is read column-by-column as ::text and re-inserted
// with per-column ::type casts — the one universal text↔type bridge that handles
// vector / jsonb / timestamptz / numeric uniformly. Documented non-goals (a
// scope using one of these yields a per-scope restore WARNING, not silent loss):
// other user-defined types (domains, composite, functions, aggregates,
// operators, collations, casts), views/triggers, GENERATED-ALWAYS IDENTITY
// (restored as a plain column; data values preserved), and custom sequence
// parameters.

// listScopes reads the durable scopes from the identity registry, skipping any
// whose schema has since been dropped (a dangling row that a missed cleanup
// left) by joining against information_schema.schemata.
func (b *postgresBackend) listScopes(ctx context.Context) ([]ScopeKey, error) {
	rows, err := b.admin.QueryContext(ctx,
		`SELECT r.tenant, r.scope, r.scope_id
		   FROM sqlmem_meta.scope_registry r
		   JOIN information_schema.schemata s ON s.schema_name = r.schema_name
		  WHERE r.scope <> $1
		  ORDER BY r.schema_name`, runScope)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: list scopes: %w", err)
	}
	defer rows.Close()
	var out []ScopeKey
	for rows.Next() {
		var k ScopeKey
		if err := rows.Scan(&k.Tenant, &k.Scope, &k.ScopeID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// exportScope reconstructs the scope's schema DDL + data from the catalog,
// running as the scope role inside a read-only transaction for a consistent view.
func (b *postgresBackend) exportScope(ctx context.Context, key ScopeKey) (*ScopeDump, error) {
	schema, role, err := pgScopeNames(key)
	if err != nil {
		return nil, err
	}
	if err := b.provision(ctx, schema, role, key); err != nil {
		return nil, err
	}
	sc, err := b.acquireScope(role)
	if err != nil {
		return nil, err
	}
	defer b.releaseScope(sc)

	tx, err := sc.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, b.healAuth(schema, role, err)
	}
	defer func() { _ = tx.Rollback() }()

	dump := &ScopeDump{}

	// Enum types first (a column may reference one).
	enumDDL, err := pgEnumDDL(ctx, tx, schema)
	if err != nil {
		return nil, err
	}
	dump.DDL = append(dump.DDL, enumDDL...)

	// Sequences next (a serial column's DEFAULT references them).
	seqDDL, err := pgSequenceDDL(ctx, tx, schema)
	if err != nil {
		return nil, err
	}
	dump.DDL = append(dump.DDL, seqDDL...)

	tables, err := pgTableNames(ctx, tx, schema)
	if err != nil {
		return nil, err
	}
	var constraintDDL, fkDDL, indexDDL []string
	for _, t := range tables {
		createDDL, td, err := pgExportTable(ctx, tx, schema, t)
		if err != nil {
			return nil, err
		}
		dump.DDL = append(dump.DDL, createDDL) // CREATE TABLE before constraints/indexes
		dump.Tables = append(dump.Tables, *td)

		cons, fks, err := pgConstraintDDL(ctx, tx, schema, t)
		if err != nil {
			return nil, err
		}
		constraintDDL = append(constraintDDL, cons...)
		fkDDL = append(fkDDL, fks...)

		idx, err := pgIndexDDL(ctx, tx, schema, t)
		if err != nil {
			return nil, err
		}
		indexDDL = append(indexDDL, idx...)
	}
	// All tables exist before their (non-FK) constraints + indexes; FKs go last.
	dump.DDL = append(dump.DDL, constraintDDL...)
	dump.DDL = append(dump.DDL, indexDDL...)
	dump.PostDDL = fkDDL
	return dump, nil
}

// pgEnumDDL reconstructs CREATE TYPE … AS ENUM for the scope's own enum types,
// emitted before tables so an enum column resolves on restore. Enums are the
// common agent-created custom type; other user-defined kinds (domains,
// functions, aggregates, operators, composite types) are NOT captured — a scope
// using one yields a per-scope restore warning (see exportScope's header).
// Re-restore re-runs CREATE TYPE → 42710, tolerated by isAlreadyExists.
func pgEnumDDL(ctx context.Context, tx *sql.Tx, schema string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT t.typname, e.enumlabel
		   FROM pg_catalog.pg_type t
		   JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
		   JOIN pg_catalog.pg_enum e ON e.enumtypid = t.oid
		  WHERE n.nspname = $1 AND t.typtype = 'e'
		  ORDER BY t.typname, e.enumsortorder`, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read enum types: %w", err)
	}
	defer rows.Close()
	var order []string
	labels := map[string][]string{}
	for rows.Next() {
		var name, label string
		if err := rows.Scan(&name, &label); err != nil {
			return nil, err
		}
		if _, ok := labels[name]; !ok {
			order = append(order, name)
		}
		labels[name] = append(labels[name], lit(label)) // enum labels are agent text → quoted
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, name := range order {
		out = append(out, fmt.Sprintf(`CREATE TYPE %s.%s AS ENUM (%s)`,
			q(schema), q(name), strings.Join(labels[name], ", ")))
	}
	return out, nil
}

// pgSequenceDDL emits CREATE SEQUENCE IF NOT EXISTS + a setval to restore each
// owned sequence's current position (serial counters survive the round-trip).
func pgSequenceDDL(ctx context.Context, tx *sql.Tx, schema string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.relname FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relkind = 'S' ORDER BY c.relname`, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read sequences: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		qualified := q(schema) + "." + q(n)
		out = append(out, fmt.Sprintf(`CREATE SEQUENCE IF NOT EXISTS %s`, qualified))
		var last int64
		var called bool
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT last_value, is_called FROM %s`, qualified)).Scan(&last, &called); err != nil {
			return nil, fmt.Errorf("sqlmem: read sequence %q: %w", n, err)
		}
		// setval's regclass arg is the quoted qualified name as a string literal
		// (regclass parses the embedded double-quotes, so a sequence whose name
		// has uppercase/special chars still resolves).
		out = append(out, fmt.Sprintf(`SELECT pg_catalog.setval(%s, %d, %t)`, lit(qualified), last, called))
	}
	return out, nil
}

// pgTableNames lists the ordinary tables (and partitioned roots) in the schema.
func pgTableNames(ctx context.Context, tx *sql.Tx, schema string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.relname FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relkind IN ('r','p') ORDER BY c.relname`, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read table list: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// pgExportTable reconstructs CREATE TABLE from pg_attribute + reads the data
// (every column as ::text). Generated-stored columns are kept in the DDL but
// excluded from the data (they can't be inserted).
func pgExportTable(ctx context.Context, tx *sql.Tx, schema, table string) (string, *TableDump, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT a.attname,
		        pg_catalog.format_type(a.atttypid, a.atttypmod) AS coltype,
		        a.attnotnull,
		        a.attgenerated,
		        a.atthasdef,
		        pg_catalog.pg_get_expr(ad.adbin, ad.adrelid) AS coldefault
		   FROM pg_catalog.pg_attribute a
		   JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		   LEFT JOIN pg_catalog.pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		  WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		  ORDER BY a.attnum`, schema, table)
	if err != nil {
		return "", nil, fmt.Errorf("sqlmem: read columns of %q: %w", table, err)
	}
	var defs []string
	td := &TableDump{Name: table}
	for rows.Next() {
		var name, coltype, generated string
		var notnull, hasdef bool
		var coldefault sql.NullString
		if err := rows.Scan(&name, &coltype, &notnull, &generated, &hasdef, &coldefault); err != nil {
			rows.Close()
			return "", nil, err
		}
		def := q(name) + " " + coltype
		switch {
		case generated == "s": // STORED generated column — keep the expression
			def += " GENERATED ALWAYS AS (" + coldefault.String + ") STORED"
		case hasdef && coldefault.Valid && coldefault.String != "":
			def += " DEFAULT " + coldefault.String
		}
		if notnull {
			def += " NOT NULL"
		}
		defs = append(defs, def)
		if generated != "s" { // generated columns are not insertable
			td.Columns = append(td.Columns, name)
			td.ColumnTypes = append(td.ColumnTypes, coltype)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	createDDL := fmt.Sprintf("CREATE TABLE %s.%s (\n  %s\n)", q(schema), q(table), strings.Join(defs, ",\n  "))

	if err := pgReadTableData(ctx, tx, schema, td); err != nil {
		return "", nil, err
	}
	return createDDL, td, nil
}

// pgReadTableData fills td.Rows by selecting every (insertable) column as ::text,
// so each value is a string or nil regardless of its postgres type.
func pgReadTableData(ctx context.Context, tx *sql.Tx, schema string, td *TableDump) error {
	if len(td.Columns) == 0 {
		return nil
	}
	sel := make([]string, len(td.Columns))
	for i, c := range td.Columns {
		sel[i] = q(c) + "::text"
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s.%s`,
		strings.Join(sel, ", "), q(schema), q(td.Name)))
	if err != nil {
		return fmt.Errorf("sqlmem: read data of %q: %w", td.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		cells := make([]sql.NullString, len(td.Columns))
		ptrs := make([]any, len(td.Columns))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([]any, len(cells))
		for i, c := range cells {
			if c.Valid {
				row[i] = c.String
			} else {
				row[i] = nil
			}
		}
		td.Rows = append(td.Rows, row)
	}
	return rows.Err()
}

// pgConstraintDDL returns the non-FK constraints (PK/UNIQUE/CHECK/exclusion) and
// the FK constraints separately — FKs are applied after data so insert order is
// irrelevant. pg_get_constraintdef yields a safely-quoted definition.
func pgConstraintDDL(ctx context.Context, tx *sql.Tx, schema, table string) (nonFK, fk []string, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT con.conname, con.contype, pg_catalog.pg_get_constraintdef(con.oid, true) AS def
		   FROM pg_catalog.pg_constraint con
		   JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2 AND con.contype IN ('p','u','c','x','f')
		  ORDER BY con.contype, con.conname`, schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlmem: read constraints of %q: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, ctype, def string
		if err := rows.Scan(&name, &ctype, &def); err != nil {
			return nil, nil, err
		}
		stmt := fmt.Sprintf("ALTER TABLE %s.%s ADD CONSTRAINT %s %s", q(schema), q(table), q(name), def)
		if ctype == "f" {
			fk = append(fk, stmt)
		} else {
			nonFK = append(nonFK, stmt)
		}
	}
	return nonFK, fk, rows.Err()
}

// pgIndexDDL returns the standalone indexes (those NOT backing a constraint —
// PK/UNIQUE indexes are recreated by their ADD CONSTRAINT). pg_get_indexdef
// yields a safely-quoted, schema-qualified CREATE INDEX.
func pgIndexDDL(ctx context.Context, tx *sql.Tx, schema, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT pg_catalog.pg_get_indexdef(i.indexrelid, 0, true) AS def
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class ct ON ct.oid = i.indrelid
		   JOIN pg_catalog.pg_class ci ON ci.oid = i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid = ct.relnamespace
		  WHERE n.nspname = $1 AND ct.relname = $2
		    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint con WHERE con.conindid = i.indexrelid)
		  ORDER BY ci.relname`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: read indexes of %q: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, rows.Err()
}

// restoreScope provisions the scope, replays the DDL as the scope role
// (already-exists tolerated → idempotent schema), loads each empty table, then
// applies the deferred FK constraints.
func (b *postgresBackend) restoreScope(ctx context.Context, key ScopeKey, dump *ScopeDump) error {
	schema, role, err := pgScopeNames(key)
	if err != nil {
		return err
	}
	if err := b.provision(ctx, schema, role, key); err != nil {
		return err
	}
	sc, err := b.acquireScope(role)
	if err != nil {
		return err
	}
	defer b.releaseScope(sc)

	for _, stmt := range dump.DDL {
		if _, err := sc.db.ExecContext(ctx, stmt); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("sqlmem: restore DDL: %w", b.healAuth(schema, role, err))
		}
	}
	for _, t := range dump.Tables {
		empty, err := pgTableIsEmpty(ctx, sc.db, schema, t.Name)
		if err != nil {
			return err
		}
		if !empty {
			continue // already populated → idempotent skip
		}
		if err := insertRowsPG(ctx, sc.db, schema, t); err != nil {
			return err
		}
	}
	for _, stmt := range dump.PostDDL {
		if _, err := sc.db.ExecContext(ctx, stmt); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("sqlmem: restore FK: %w", err)
		}
	}
	return nil
}

// pgTableIsEmpty reports whether the scope-schema table currently has no rows.
func pgTableIsEmpty(ctx context.Context, db *sql.DB, schema, table string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT 1 FROM %s.%s LIMIT 1`, q(schema), q(table))).Scan(&one)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlmem: check table %q: %w", table, err)
	}
	return false, nil
}

// insertRowsPG inserts the dumped rows with per-column ::type casts so each text
// value is coerced back into its column's real type (int / vector / jsonb /
// timestamptz / …). A nil cell binds SQL NULL.
func insertRowsPG(ctx context.Context, db *sql.DB, schema string, t TableDump) error {
	if len(t.Rows) == 0 {
		return nil
	}
	if len(t.ColumnTypes) != len(t.Columns) {
		return fmt.Errorf("sqlmem: table %q dump has %d columns but %d types", t.Name, len(t.Columns), len(t.ColumnTypes))
	}
	cols := make([]string, len(t.Columns))
	marks := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = q(c)
		marks[i] = fmt.Sprintf("$%d::%s", i+1, t.ColumnTypes[i])
	}
	stmt := fmt.Sprintf(`INSERT INTO %s.%s (%s) VALUES (%s)`,
		q(schema), q(t.Name), strings.Join(cols, ", "), strings.Join(marks, ", "))
	for _, row := range t.Rows {
		if _, err := db.ExecContext(ctx, stmt, row...); err != nil {
			return fmt.Errorf("sqlmem: restore insert into %q: %w", t.Name, err)
		}
	}
	return nil
}
