package sqlmem

import (
	"context"
	"fmt"
	"testing"
)

// A virtual table (FTS5, R-tree) keeps its data in shadow tables the engine
// creates and owns. On a defensive connection a CREATE of a shadow table's
// name is refused as reserved, so a dump that replays shadow-table DDL failed
// the whole scope's restore, the Document plane's tables with it.

func vtabScope(t *testing.T, m *Manager, key ScopeKey) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`,
		`CREATE VIRTUAL TABLE notes_fts USING fts5(body)`,
		`CREATE VIRTUAL TABLE box USING rtree(id, minx, maxx, miny, maxy)`,
		`INSERT INTO notes (body) VALUES ('alpha beta'), ('gamma delta')`,
		`INSERT INTO notes_fts (rowid, body) VALUES (1, 'alpha beta'), (2, 'gamma delta')`,
		`INSERT INTO box VALUES (1, 0, 10, 0, 10), (2, 20, 30, 20, 30)`,
	} {
		if _, err := m.Exec(ctx, key, s, nil, 0); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func assertVtabScope(t *testing.T, m *Manager, key ScopeKey) {
	t.Helper()
	ctx := context.Background()
	for q, want := range map[string]string{
		`SELECT count(*) FROM notes`:                                   "2",
		`SELECT rowid FROM notes_fts WHERE notes_fts MATCH 'gamma'`:    "2",
		`SELECT id FROM box WHERE minx >= 15`:                          "2",
		`SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'alpha'`: "1",
		`SELECT count(*) FROM box WHERE maxx <= 10 AND maxy <= 10`:     "1",
	} {
		res, err := m.Query(ctx, key, q, nil)
		if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != want {
			t.Errorf("restored %s = %v, %v; want %s", q, res, err, want)
		}
	}
}

func TestDump_SQLiteVirtualTablesRoundTrip(t *testing.T) {
	ctx := context.Background()
	key := agentKey("t1", "vtabs")
	src := newTestManager(t, Config{})
	vtabScope(t, src, key)

	dump, err := src.ExportScope(ctx, key)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	dst := newTestManager(t, Config{})
	if err := dst.RestoreScope(ctx, key, dump); err != nil {
		t.Fatalf("RestoreScope: %v", err)
	}
	assertVtabScope(t, dst, key)
	// A second restore of the same archive stays a no-op.
	if err := dst.RestoreScope(ctx, key, dump); err != nil {
		t.Fatalf("second RestoreScope: %v", err)
	}
	assertVtabScope(t, dst, key)
}

// An archive taken before export left shadow tables out carries their DDL and
// rows. It restores too: the reserved-name refusal of a shadow table's CREATE is
// tolerated, and rows are never written into a table the target engine owns.
func TestDump_SQLiteOldArchiveWithShadowTablesRestores(t *testing.T) {
	ctx := context.Background()
	key := agentKey("t1", "vtabs")
	src := newTestManager(t, Config{})
	vtabScope(t, src, key)
	b := src.backend.(*sqliteBackend)
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	// The archive as the earlier export wrote it: every object with DDL, every
	// table's rows, shadow tables included.
	old := &ScopeDump{}
	rows, err := db.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		old.DDL = append(old.DDL, s)
	}
	rows.Close()
	var names []string
	rows, err = db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		td, err := b.exportTableSQLite(ctx, db, n)
		if err != nil {
			t.Fatal(err)
		}
		old.Tables = append(old.Tables, *td)
	}
	b.release(path)
	if len(names) < 8 {
		t.Fatalf("the source scope has tables %v; want the shadow tables in the archive for this check to mean anything", names)
	}

	dst := newTestManager(t, Config{})
	if err := dst.RestoreScope(ctx, key, old); err != nil {
		t.Fatalf("RestoreScope of an archive carrying shadow tables: %v", err)
	}
	assertVtabScope(t, dst, key)
}
