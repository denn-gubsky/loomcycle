package sqlmem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// sqlite_planes.go — the sqlite tier's document plane: a file of its own per
// scope, <scope>.doc.db beside the agent's <scope>.db (RFC FZ Part A, D1 (b)).
//
// WHY A FILE. Every connection the agent plane opens is to the agent file, and an
// agent's SQL can neither ATTACH nor VACUUM INTO (validate.go), so a table in the
// document file is one Memory SQL cannot name, read through sqlite_dbpage, or
// rewrite by page: there is nothing of it in the file those connections have. The
// Memory tool's statement guard (memory_sql_doc_guard.go) stays as defence in
// depth, as it does on postgres, where the planes are a schema and role each.
//
// MOVING A SCOPE FROM BEFORE THE SPLIT mirrors postgres (postgres_planes.go): the
// Document tool brings the old tables up to date where they are, provisions fresh
// tables in the document file with its own DDL, and then adoptTables copies the
// rows across and drops the old tables. Rows are copied rather than the file
// renamed or the DDL replayed: what the agent file holds was within an agent's
// reach, so only the Document tool's own DDL defines the tables that carry rows
// forward, and a trigger, view or index an agent attached stays behind and is
// dropped with its table.
//
// CRASH SAFETY. All the copies and the split mark are ONE transaction on the
// document file, so the file goes from "tables provisioned, empty, unsplit" to
// "every row, split" atomically: no reader — the dead-link GC above all, which
// reaps a body whose chunk row it cannot see — ever sees half a move. Dropping the
// old tables is a second transaction, on the agent file, after the mark. Each step
// is safe to stop after:
//   - before the copy commits: nothing in the document file counts (unsplit), the
//     agent file still holds every row, and the next adoption copies them;
//   - after it commits, before the drop: the split is recorded, so the next
//     adoption drops the leftovers WITHOUT reading them — they are duplicates of
//     what was copied, and anything written to them since is an agent's;
//   - a failed drop is logged, not returned: the document file is complete, and
//     the next adoption (the boot pass, or the next process's first use) retries.

// docFileSplitMark is the document file's user_version once the scope's tables
// have moved into it (or it was created that way). The header field is written
// only here: an agent's connections never open this file, and the Document
// tool's statements cannot set a PRAGMA (validate.go).
const docFileSplitMark = 1

// sqliteAdoptTimeout bounds one scope's move. It is detached from the op that
// triggered it, as on postgres: a move abandoned by a short request deadline
// would only be retried by the next one.
const sqliteAdoptTimeout = 10 * time.Minute

// adoptAttachName is the schema name the agent file is attached under, on a
// connection of the move's own that no statement but the runtime's ever uses.
const adoptAttachName = "agentplane"

// adoptLock serialises the moves of one scope (keyed by its agent file). A
// sqlite data directory belongs to one process, so a mutex is the whole lock.
func (b *sqliteBackend) adoptLock(path string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.adoptLocks == nil {
		b.adoptLocks = make(map[string]*sync.Mutex)
	}
	l, ok := b.adoptLocks[path]
	if !ok {
		l = &sync.Mutex{}
		b.adoptLocks[path] = l
	}
	return l
}

// agentTables returns which of tables exist in the scope's agent file. It never
// creates the file: a scope that only ever held documents has none.
func (b *sqliteBackend) agentTables(ctx context.Context, key ScopeKey, tables []string) ([]string, error) {
	path, err := key.wholeScope().keyPath(b.cfg.Root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlmem: stat the agent file: %w", err)
	}
	db, err := b.acquire(path)
	if err != nil {
		return nil, err
	}
	defer b.release(path)
	rows, err := db.QueryContext(ctx, `SELECT lower(name) FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: list agent-file tables: %w", err)
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
		if have[strings.ToLower(t)] {
			out = append(out, t)
		}
	}
	return out, nil
}

// adoptTables moves the named tables from the scope's agent file into its
// document file (Manager.AdoptAgentTables), whose tables the caller has already
// provisioned. See the file comment for the steps and why each is safe to stop
// after.
func (b *sqliteBackend) adoptTables(ctx context.Context, key ScopeKey, tables []string) error {
	for _, t := range tables {
		if !pgTableNameRe.MatchString(t) {
			return fmt.Errorf("sqlmem: %q is not a table name", t)
		}
	}
	agentPath, docPath, err := key.planeFiles(b.cfg.Root)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sqliteAdoptTimeout)
	defer cancel()
	lk := b.adoptLock(agentPath)
	lk.Lock()
	defer lk.Unlock()

	held, err := b.agentTables(ctx, key, tables)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		// Even with nothing to move: the mark is what says the document file is
		// the scope's, after which nothing in the agent file is ever copied over.
		return b.markSplit(ctx, key)
	}

	// A connection of the move's own, closed when it is done: the agent file is
	// attached to it, and an ATTACH left on a pooled connection would outlive the
	// move. The agent's pool keeps its own connection to the agent file; WAL lets
	// both read it, and the drop below waits out (busy_timeout) an agent write.
	db, err := openScopeDB(docPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlmem: move document tables: %w", err)
	}
	defer func() { _ = conn.Close() }()
	split, err := docFileSplit(ctx, conn)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS `+adoptAttachName, agentPath); err != nil {
		return fmt.Errorf("sqlmem: move document tables: attach the agent file: %w", err)
	}
	if split {
		slog.Warn("sqlmem: dropping tables named like document tables from the agent file of a scope that already has its document file; their rows were not read",
			"path", agentPath, "tables", held)
	} else if err := b.copyIntoDocFile(ctx, conn, agentPath, held); err != nil {
		return err
	}
	if b.adoptStep != nil {
		if err := b.adoptStep(adoptStepMarked); err != nil {
			return err
		}
	}
	if err := dropAgentFileTables(ctx, conn, held); err != nil {
		slog.Warn("sqlmem: could not drop the moved document tables from the agent file; the next move retries",
			"path", agentPath, "err", err)
	}
	return nil
}

// copyIntoDocFile copies held's rows from the attached agent file into the
// document file's tables of the same names, and records the split, in ONE
// transaction on the document file.
//
// Only columns the document file's table has are copied (an agent could have
// added one before the guard existed; it is logged and left behind). A document
// table that already holds rows keeps them and the agent copy is discarded, with a
// warning, as on postgres: that is a scope restored from an older archive over a
// live document file. INSERT … SELECT runs inside the engine, so a large scope
// streams page by page rather than through Go memory.
func (b *sqliteBackend) copyIntoDocFile(ctx context.Context, conn *sql.Conn, agentPath string, held []string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlmem: move document tables: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var orphans []string
	for _, t := range held {
		docCols, err := tableColumns(ctx, tx, "main", t)
		if err != nil {
			return err
		}
		if len(docCols) == 0 {
			// Not a table the Document tool creates any more (a migration's
			// scratch table, say): nothing in the document file takes its rows.
			orphans = append(orphans, t)
			continue
		}
		agentCols, err := tableColumns(ctx, tx, adoptAttachName, t)
		if err != nil {
			return err
		}
		inAgent := map[string]bool{}
		for _, c := range agentCols {
			inAgent[strings.ToLower(c)] = true
		}
		inDoc := map[string]bool{}
		var cols []string
		for _, c := range docCols {
			inDoc[strings.ToLower(c)] = true
			if inAgent[strings.ToLower(c)] {
				cols = append(cols, quoteIdent(c))
			}
		}
		for _, c := range agentCols {
			if !inDoc[strings.ToLower(c)] {
				slog.Warn("sqlmem: dropping a column an agent added to a document table", "path", agentPath, "table", t, "column", c)
			}
		}
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM main.`+quoteIdent(t)+` LIMIT 1`).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlmem: check %s in the document file: %w", t, err)
		}
		if err == nil {
			slog.Warn("sqlmem: discarded the agent file's copy of a document table; the document file already holds rows",
				"path", agentPath, "table", t)
			continue
		}
		if len(cols) == 0 {
			continue
		}
		list := strings.Join(cols, ", ")
		// #nosec G202 -- t matched pgTableNameRe and the columns come from the files' own catalogs, quoted
		stmt := `INSERT INTO main.` + quoteIdent(t) + ` (` + list + `) SELECT ` + list + ` FROM ` + adoptAttachName + `.` + quoteIdent(t)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlmem: move %s to the document file: %w", t, err)
		}
	}
	if len(orphans) == len(held) {
		// Every table would be dropped unread for want of a place to go: the
		// document file was never provisioned. Refuse rather than lose the rows.
		return fmt.Errorf("sqlmem: the document file holds none of %v; provision it before moving the tables", held)
	}
	for _, t := range orphans {
		slog.Warn("sqlmem: dropping a document table with no document-file counterpart", "path", agentPath, "table", t)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA main.user_version = %d`, docFileSplitMark)); err != nil {
		return fmt.Errorf("sqlmem: record the scope's split: %w", err)
	}
	if b.adoptStep != nil {
		if err := b.adoptStep(adoptStepCopied); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlmem: move document tables: commit: %w", err)
	}
	return nil
}

// dropAgentFileTables drops the moved tables from the attached agent file in one
// transaction. Foreign keys are off for it: an agent table that once REFERENCED a
// document table would otherwise make the implicit delete fail and wedge the move
// for good. Dropping a table takes its indexes and triggers with it.
func dropAgentFileTables(ctx context.Context, conn *sql.Conn, held []string) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range held {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+adoptAttachName+`.`+quoteIdent(t)); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return tx.Commit()
}

// markSplit records in the scope's document file that the file is the scope's
// document plane.
func (b *sqliteBackend) markSplit(ctx context.Context, key ScopeKey) error {
	_, docPath, err := key.planeFiles(b.cfg.Root)
	if err != nil {
		return err
	}
	db, err := b.acquire(docPath)
	if err != nil {
		return err
	}
	defer b.release(docPath)
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, docFileSplitMark)); err != nil {
		return fmt.Errorf("sqlmem: record the scope's split: %w", err)
	}
	return nil
}

// docFileSplit reports whether the document file carries the split mark.
func docFileSplit(ctx context.Context, q rowQueryer) (bool, error) {
	var v int64
	if err := q.QueryRowContext(ctx, `PRAGMA main.user_version`).Scan(&v); err != nil {
		return false, fmt.Errorf("sqlmem: read the document file's mark: %w", err)
	}
	return v == docFileSplitMark, nil
}

// tableColumns lists a table's columns in one schema of the connection, in order;
// empty when the table does not exist there.
func tableColumns(ctx context.Context, tx *sql.Tx, schema, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?) ORDER BY cid`, table, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: columns of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// quoteIdent double-quotes a sqlite identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
