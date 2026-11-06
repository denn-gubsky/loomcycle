package sqlmem

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sqlite_planes_test.go — on sqlite the Document tool's tables live in a file of
// their own per scope (<scope>.doc.db), which the agent plane's connections never
// open (RFC FZ Part A, D1 (b)).

// docTableDDL stands in for one of the Document tool's tables: sqlmem does not
// know their names, the Document tool passes them in.
const docTableDDL = `CREATE TABLE IF NOT EXISTS chunks (id TEXT PRIMARY KEY, title TEXT, status TEXT, n INTEGER, raw BLOB)`

// planePaths is the scope's two files.
func planePaths(t *testing.T, m *Manager, key ScopeKey) (agent, doc string) {
	t.Helper()
	root := managerRoot(t, m)
	agent, err := key.wholeScope().keyPath(root)
	if err != nil {
		t.Fatal(err)
	}
	dk := key
	dk.plane = planeDoc
	doc, err = dk.keyPath(root)
	if err != nil {
		t.Fatal(err)
	}
	return agent, doc
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// oldLayoutScope builds a scope the way every sqlite scope was before the split:
// the document table in the agent file, beside the agent's own table, with no
// document file.
func oldLayoutScope(t *testing.T, m *Manager, key ScopeKey) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`,
		`INSERT INTO notes (body) VALUES ('agent note')`,
		docTableDDL,
		`INSERT INTO chunks VALUES ('c1', 'secret-title-c1', 'proposed', 9007199254740993, x'00ff10')`,
		`INSERT INTO chunks VALUES ('c2', 'second', 'asserted', 2, NULL)`,
	} {
		if _, err := m.Exec(ctx, key.AgentPlane(), stmt, nil, 0); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, doc := planePaths(t, m, key); fileExists(doc) {
		t.Fatal("the old layout has no document file")
	}
}

// provisionDocFile does what the Document tool's provisioning does before it
// adopts: its own DDL on the document plane.
func provisionDocFile(t *testing.T, m *Manager, key ScopeKey) {
	t.Helper()
	if _, err := m.DocPlane().Exec(context.Background(), key, docTableDDL, nil, 0); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, m *Manager, key ScopeKey, table string) int64 {
	t.Helper()
	res, err := m.Query(context.Background(), key, `SELECT count(*) FROM `+table, nil)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return res.Rows[0][0].(int64)
}

// From the agent plane, which is all Memory SQL reaches, the document tables do
// not exist: the engine answers "no such table", with no statement check of the
// Memory tool's in the way, and no page of the agent's file holds their rows.
func TestSqlmem_SQLMemoryCannotNameTheDocumentFile(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	if _, err := m.Exec(ctx, key, `CREATE TABLE notes (body TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	provisionDocFile(t, m, key)
	if _, err := m.DocPlane().Exec(ctx, key, `INSERT INTO chunks (id, title) VALUES ('c1', 'secret-title-c1')`, nil, 0); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{`SELECT title FROM chunks`, `SELECT count(*) FROM main.chunks`} {
		_, err := m.Query(ctx, key, q, nil)
		if err == nil || !strings.Contains(err.Error(), "no such table") {
			t.Errorf("agent plane %q: err = %v; want no such table", q, err)
		}
	}
	if _, err := m.Exec(ctx, key, `UPDATE chunks SET status = 'asserted'`, nil, 0); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("agent plane UPDATE chunks: err = %v; want no such table", err)
	}

	// The engine's page view of the agent file holds nothing of the document plane.
	b := m.backend.(*sqliteBackend)
	agentPath, docPath := planePaths(t, m, key)
	db, err := b.acquire(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT data FROM sqlite_dbpage`)
	if err != nil {
		b.release(agentPath)
		t.Fatal(err)
	}
	pages := 0
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		pages++
		if bytes.Contains(data, []byte("secret-title-c1")) {
			t.Errorf("page %d of the agent file holds a document row", pages)
		}
	}
	rows.Close()
	b.release(agentPath)
	if pages == 0 {
		t.Fatal("read no pages of the agent file; the check means nothing")
	}

	if !strings.HasSuffix(docPath, ".doc.db") || !fileExists(docPath) {
		t.Errorf("document file = %q (exists %v); want <scope>.doc.db on disk", docPath, fileExists(docPath))
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 1 {
		t.Errorf("document plane chunks = %d, want 1", got)
	}
}

// A scope from before the split keeps its document tables in the agent file. The
// first adoption copies their rows into the document file, records the split
// there, and drops them from the agent file; the agent's own tables stay.
func TestSqlmem_AdoptMovesAgentFileTablesIntoTheDocFile(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "tenant", ScopeID: "t1"}
	oldLayoutScope(t, m, key)

	held, err := m.AgentPlaneTables(ctx, key, []string{"chunks", "documents"})
	if err != nil || len(held) != 1 || held[0] != "chunks" {
		t.Fatalf("AgentPlaneTables = %v, %v; want [chunks]", held, err)
	}
	if ok, err := m.DocPlane().PlaneExists(ctx, key); err != nil || ok {
		t.Fatalf("PlaneExists(doc) before the move = %v, %v; want false", ok, err)
	}
	provisionDocFile(t, m, key)
	if ok, err := m.DocPlane().PlaneExists(ctx, key); err != nil || ok {
		t.Fatalf("PlaneExists(doc) with an empty, unadopted document file = %v, %v; want false", ok, err)
	}
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks", "documents"}); err != nil {
		t.Fatalf("AdoptAgentTables: %v", err)
	}

	res, err := m.DocPlane().Query(ctx, key, `SELECT id, title, status, n, raw FROM chunks ORDER BY id`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][1] != "secret-title-c1" || res.Rows[0][3] != int64(9007199254740993) ||
		res.Rows[0][4] != string([]byte{0x00, 0xff, 0x10}) || res.Rows[1][4] != nil {
		t.Errorf("document file rows = %v; want both rows, values exact", res.Rows)
	}
	if held, err := m.AgentPlaneTables(ctx, key, []string{"chunks"}); err != nil || len(held) != 0 {
		t.Errorf("agent file still holds %v (%v)", held, err)
	}
	if got := countRows(t, m, key, "notes"); got != 1 {
		t.Errorf("the agent's own table has %d rows, want 1", got)
	}
	if ok, err := m.DocPlane().PlaneExists(ctx, key); err != nil || !ok {
		t.Errorf("PlaneExists(doc) after the move = %v, %v; want true", ok, err)
	}
	// A second adoption finds nothing to move and changes nothing.
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("document file after a second adoption: %d rows, want 2", got)
	}
}

// failAt makes the adoption stop at one step, as a crash there would.
func failAt(b *sqliteBackend, step string) {
	b.adoptStep = func(s string) error {
		if s == step {
			return errors.New("injected stop at " + s)
		}
		return nil
	}
}

// Stopped inside the copy, the document file's transaction rolls back: the
// scope is still unsplit, the agent file still holds every row, and the next
// adoption moves them once.
func TestSqlmem_AdoptResumesAfterAnInterruptedCopy(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	b := m.backend.(*sqliteBackend)
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)

	failAt(b, adoptStepCopied)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err == nil {
		t.Fatal("the injected stop did not surface")
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 0 {
		t.Errorf("document file after a stopped copy: %d rows, want 0 (rolled back)", got)
	}
	if ok, _ := m.DocPlane().PlaneExists(ctx, key); ok {
		t.Error("a stopped copy recorded the split")
	}
	if got := countRows(t, m, key.AgentPlane(), "chunks"); got != 2 {
		t.Errorf("agent file after a stopped copy: %d rows, want 2", got)
	}

	b.adoptStep = nil
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("document file after the resumed move: %d rows, want 2", got)
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 0 {
		t.Errorf("agent file still holds %v", held)
	}
}

// Stopped after the copy committed but before the agent file's tables were
// dropped, the split is recorded and the leftovers are exact duplicates: the next
// adoption drops them, and quarantines nothing.
func TestSqlmem_AdoptResumesAfterAnInterruptedDrop(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	b := m.backend.(*sqliteBackend)
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)

	failAt(b, adoptStepMarked)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err == nil {
		t.Fatal("the injected stop did not surface")
	}
	if ok, _ := m.DocPlane().PlaneExists(ctx, key); !ok {
		t.Fatal("the copy committed but the split is not recorded")
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 1 {
		t.Fatalf("agent file holds %v, want the undropped chunks", held)
	}

	b.adoptStep = nil
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("document file: %d rows, want the 2 copied once", got)
	}
	if q := quarantineFiles(t, m, key); len(q) != 0 {
		t.Errorf("exact duplicates were quarantined: %v", q)
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 0 {
		t.Errorf("agent file still holds %v", held)
	}
}

// Adopting into a document file the Document tool never provisioned would drop
// every table unread for want of a counterpart; it is refused instead.
func TestSqlmem_AdoptRefusesWithoutAProvisionedDocFile(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err == nil {
		t.Fatal("adopted into an unprovisioned document file")
	}
	if got := countRows(t, m, key.AgentPlane(), "chunks"); got != 2 {
		t.Errorf("agent file: %d rows, want 2 untouched", got)
	}
}

// Two adopters of one scope take turns: the second finds the move done.
func TestSqlmem_AdoptIsSerialisedPerScope(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent adoption: %v", err)
		}
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("document file: %d rows, want 2", got)
	}
}

// EnsureOnce runs one scope's provisioning at a time, so a second caller waits
// for the first's move instead of racing it on the agent file.
func TestSqlmem_EnsureOnceSerialisesOneScope(t *testing.T) {
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	var running, peak, runs atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.DocPlane().EnsureOnce(key, func() error {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				runs.Add(1)
				running.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()
	if peak.Load() != 1 || runs.Load() != 1 {
		t.Errorf("provisioning ran %d times with %d at once; want once", runs.Load(), peak.Load())
	}
}

// A snapshot carries both files: the document file's tables travel as the dump's
// document tail and restore into the document file, recorded as split, and the
// agent file gets only the agent's tables.
func TestSqlmem_SnapshotCarriesBothPlaneFiles(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}

	dump, err := m.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if dump.DocTail == nil || dump.DocTail.Tables != 1 {
		t.Fatalf("DocTail = %+v; want the document file's one table", dump.DocTail)
	}
	if removed, err := m.DropScope(ctx, key); err != nil || !removed {
		t.Fatalf("DropScope = %v, %v", removed, err)
	}
	agentPath, docPath := planePaths(t, m, key)
	if fileExists(agentPath) || fileExists(docPath) {
		t.Fatalf("after DropScope: agent file %v, document file %v; want neither", fileExists(agentPath), fileExists(docPath))
	}

	if err := m.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("restored document file: %d chunks, want 2", got)
	}
	if got := countRows(t, m, key, "notes"); got != 1 {
		t.Errorf("restored agent file: %d notes, want 1", got)
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 0 {
		t.Errorf("the restore put %v in the agent file", held)
	}
	if ok, _ := m.DocPlane().PlaneExists(ctx, key); !ok {
		t.Error("the restored document file is not recorded as split")
	}
}

// An archive from before the split carries the document tables in its one dump.
// They restore into the agent file, the layout of a scope not yet moved, and the
// Document tool's next provisioning moves them as it moves any such scope.
func TestSqlmem_OldSingleFileSnapshotRestoresForTheMove(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	dump, err := m.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if dump.DocTail != nil {
		t.Fatalf("an unsplit scope exported a document tail: %+v", dump.DocTail)
	}
	if _, err := m.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	provisionDocFile(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("document file: %d chunks, want 2", got)
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 0 {
		t.Errorf("agent file still holds %v", held)
	}
}

// A scope is one unit across its two files: listed once, dropped whole, swept by
// the newer of its two mtimes and by their summed size, and measured against its
// quota by both.
func TestSqlmem_ScopeIsOneUnitAcrossItsFiles(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	both := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "both"}
	docOnly := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "doconly"}
	if _, err := m.Exec(ctx, both, `CREATE TABLE notes (x TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	provisionDocFile(t, m, both)
	provisionDocFile(t, m, docOnly)

	keys, err := m.ListScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, k := range keys {
		seen[k.ScopeID]++
	}
	if len(keys) != 2 || seen["both"] != 1 || seen["doconly"] != 1 {
		t.Errorf("ListScopes = %v; want each scope once", keys)
	}
	if agentPath, _ := planePaths(t, m, docOnly); fileExists(agentPath) {
		t.Error("a document-only scope grew an agent file")
	}
	if ok, err := m.ScopeExists(ctx, docOnly); err != nil || !ok {
		t.Errorf("ScopeExists(document-only scope) = %v, %v; want true", ok, err)
	}

	// GC by age: the scope is as fresh as its newer file.
	b := m.backend.(*sqliteBackend)
	agentPath, docPath := planePaths(t, m, both)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(agentPath, old, old); err != nil {
		t.Fatal(err)
	}
	_, onlyDoc := planePaths(t, m, docOnly)
	if err := os.Chtimes(onlyDoc, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := b.sweepStale(time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("sweepStale with a fresh document file = %d, %v; want 0", n, err)
	}
	if !fileExists(agentPath) || !fileExists(docPath) {
		t.Fatal("GC removed half of a live scope")
	}
	if err := os.Chtimes(docPath, old, old); err != nil {
		t.Fatal(err)
	}
	if n, err := b.sweepStale(time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweepStale with both files stale = %d, %v; want 1", n, err)
	}
	if fileExists(agentPath) || fileExists(docPath) {
		t.Errorf("after GC: agent file %v, document file %v; want neither", fileExists(agentPath), fileExists(docPath))
	}

	// DropScope takes both files.
	if removed, err := m.DropScope(ctx, docOnly); err != nil || !removed {
		t.Fatalf("DropScope(document-only) = %v, %v", removed, err)
	}
	if fileExists(onlyDoc) {
		t.Error("DropScope left the document file")
	}
}

// The quota is the scope's, wherever its tables live: the agent's writes are
// refused once the document file alone fills it.
func TestSqlmem_QuotaCountsBothPlaneFiles(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{QuotaBytes: 256 << 10})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	if _, err := m.Exec(ctx, key, `CREATE TABLE notes (x TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	provisionDocFile(t, m, key)
	big := strings.Repeat("x", 64<<10)
	for i := 0; i < 8; i++ {
		if _, err := m.DocPlane().Exec(ctx, key, `INSERT INTO chunks (id, title) VALUES (?, ?)`, []any{i, big}, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO notes VALUES ('a')`, nil, 0); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("agent write with the document file over the quota: err = %v; want refused", err)
	}
}

// moveOldScope builds the old layout and moves it, as the Document tool does.
func moveOldScope(t *testing.T, m *Manager, key ScopeKey) {
	t.Helper()
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(context.Background(), key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
}

// quarantineFiles lists the scope's quarantine files.
func quarantineFiles(t *testing.T, m *Manager, key ScopeKey) []string {
	t.Helper()
	agentPath, _ := planePaths(t, m, key)
	out, err := filepath.Glob(strings.TrimSuffix(agentPath, ".db") + ".leftover-*.db")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// After the split, an agent-file table named like a document table holds rows
// the document file does not: written by an older release the deployment was
// rolled back to, or planted past the name guard. The agent file is the plane
// Memory SQL writes, so none of it is merged into the document plane: the tables
// go to a quarantine file beside the scope for an operator to inspect, and leave
// the agent file. The document plane is unchanged.
func TestSqlmem_AdoptQuarantinesRowsWrittenToTheAgentFileAfterTheSplit(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	moveOldScope(t, m, key)

	for _, stmt := range []string{
		docTableDDL,
		`INSERT INTO chunks (id, title, status) VALUES ('c3', 'written after the move', 'asserted')`,
		`INSERT INTO chunks (id, title, status) VALUES ('c1', 'a different c1', 'asserted')`,
		`INSERT INTO chunks (id, title, status, n, raw) VALUES ('c2', 'second', 'asserted', 2, NULL)`,
	} {
		if _, err := m.Exec(ctx, key.AgentPlane(), stmt, nil, 0); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.DocPlane().Query(ctx, key, `SELECT id, title, status FROM chunks ORDER BY id`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][1] != "secret-title-c1" || res.Rows[0][2] != "proposed" || res.Rows[1][2] != "asserted" {
		t.Errorf("document plane = %v; want it unchanged (c1 proposed, c2 as copied)", res.Rows)
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 0 {
		t.Errorf("agent file still holds %v", held)
	}
	q := quarantineFiles(t, m, key)
	if len(q) != 1 {
		t.Fatalf("quarantine files = %v; want one", q)
	}
	qdb, err := sql.Open("sqlite", "file:"+q[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = qdb.QueryRow(`SELECT count(*) FROM chunks WHERE id IN ('c1', 'c3')`).Scan(&n)
	_ = qdb.Close()
	if err != nil || n != 2 {
		t.Errorf("quarantine holds %d of the late rows (%v); want c1 and c3", n, err)
	}

	// Never a scope of its own, and erased with the scope.
	if keys, err := m.ListScopes(ctx); err != nil || len(keys) != 1 {
		t.Errorf("ListScopes = %v, %v; the quarantine file must not be listed as a scope", keys, err)
	}
	if removed, err := m.DropScope(ctx, key); err != nil || !removed {
		t.Fatalf("DropScope = %v, %v", removed, err)
	}
	if q := quarantineFiles(t, m, key); len(q) != 0 {
		t.Errorf("DropScope left the quarantine file: %v", q)
	}
}

// pageStats reads a file's page counts on a connection of the test's own.
func pageStats(t *testing.T, path string) (pages, free int64) {
	t.Helper()
	db, err := openScopeDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	return pages, free
}

// fileHolds reports whether a file or its -wal holds the bytes.
func fileHolds(t *testing.T, path string, needle []byte) bool {
	t.Helper()
	for _, p := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(p)
		if err == nil && bytes.Contains(raw, needle) {
			return true
		}
	}
	return false
}

// bigOldScope is an old-layout scope with about 1.5 MB of document rows.
func bigOldScope(t *testing.T, m *Manager, key ScopeKey) {
	t.Helper()
	ctx := context.Background()
	oldLayoutScope(t, m, key)
	body := "moved-secret-" + strings.Repeat("x", 64<<10)
	for i := 0; i < 24; i++ {
		if _, err := m.Exec(ctx, key.AgentPlane(), `INSERT INTO chunks (id, title) VALUES (?, ?)`, []any{"big" + string(rune('a'+i)), body}, 0); err != nil {
			t.Fatal(err)
		}
	}
}

// The moved rows must not stay readable in the agent file's free pages (or its
// WAL), which page-level access reaches, and the file must not keep their size.
func TestSqlmem_AdoptLeavesNoDocumentBytesInTheAgentFile(t *testing.T) {
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	bigOldScope(t, m, key)
	agentPath, _ := planePaths(t, m, key)
	if !fileHolds(t, agentPath, []byte("moved-secret-")) {
		t.Fatal("the old layout's agent file does not hold the rows; the check means nothing")
	}
	provisionDocFile(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(context.Background(), key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if fileHolds(t, agentPath, []byte("moved-secret-")) {
		t.Error("the agent file (or its WAL) still holds the moved rows' bytes")
	}
	if pages, free := pageStats(t, agentPath); free > 2 || pages > 16 {
		t.Errorf("agent file after the move: %d pages, %d free; want it compacted", pages, free)
	}
}

// The quota measures what a scope holds, the same way for both files: an upgraded
// scope is not charged for the agent file's freed pages, nor its documents twice.
func TestSqlmem_QuotaOfAnUpgradedScopeIsItsData(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	bigOldScope(t, m, key)
	agentPath, _ := planePaths(t, m, key)
	before, _ := pageStats(t, agentPath)
	provisionDocFile(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	// A quota a quarter above the scope's data before the move.
	quota := int(before*4096) * 5 / 4
	if _, err := m.Exec(ctx, key, `INSERT INTO notes (body) VALUES ('after the move')`, nil, quota); err != nil {
		t.Errorf("agent write under a quota above the scope's data: %v", err)
	}
	if _, err := m.DocPlane().Exec(ctx, key, `INSERT INTO chunks (id, title) VALUES ('new', 'after the move')`, nil, quota); err != nil {
		t.Errorf("document write under a quota above the scope's data: %v", err)
	}
}

// A document file not yet marked never counts: rows found in it at the move (a
// write that skipped provisioning, landing in a failed move's empty tables) are
// replaced by the agent file's, which hold the scope's documents.
func TestSqlmem_AdoptReplacesAnUnmarkedDocFilesRows(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)
	if _, err := m.DocPlane().Exec(ctx, key, `INSERT INTO chunks (id, title) VALUES ('stray', 'stray')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.DocPlane().Query(ctx, key, `SELECT id FROM chunks ORDER BY id`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][0] != "c1" || res.Rows[1][0] != "c2" {
		t.Errorf("document file = %v; want the agent file's c1, c2", res.Rows)
	}
}

// A restore writes the document part's rows and the mark in one transaction, so
// a restore stopped before it commits leaves an unmarked, empty document file
// (and an old scope's rows where they were), never rows that outrank them.
func TestSqlmem_RestoreWritesTheDocumentRowsAndTheMarkTogether(t *testing.T) {
	ctx := context.Background()
	src := newTestManager(t, Config{})
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	provisionDocFile(t, src, key)
	if _, err := src.DocPlane().Exec(ctx, key, `INSERT INTO chunks (id, title) VALUES ('archived', 'from the archive')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := src.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	dump, err := src.ExportScope(ctx, key)
	if err != nil || dump.DocTail == nil {
		t.Fatalf("export = %+v, %v", dump, err)
	}

	m := newTestManager(t, Config{})
	b := m.backend.(*sqliteBackend)
	oldLayoutScope(t, m, key)
	failAt(b, adoptStepRestored)
	if err := m.RestoreScope(ctx, key, dump); err == nil {
		t.Fatal("the injected stop did not surface")
	}
	if ok, _ := m.DocPlane().PlaneExists(ctx, key); ok {
		t.Fatal("a stopped restore recorded the split")
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 0 {
		t.Errorf("document file after a stopped restore: %d rows, want 0", got)
	}
	b.adoptStep = nil
	provisionDocFile(t, m, key)
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, m.DocPlane(), key, "chunks"); got != 2 {
		t.Errorf("document file after the move: %d rows, want the old scope's 2", got)
	}

	// Completed, the restore leaves its rows marked.
	m2 := newTestManager(t, Config{})
	if err := m2.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m2.DocPlane().PlaneExists(ctx, key); !ok || countRows(t, m2.DocPlane(), key, "chunks") != 1 {
		t.Errorf("a completed restore: marked=%v; want marked with the archived row", ok)
	}
}

// A scope erased while a move waits does not come back: the move finds its files
// gone and does not recreate them.
func TestSqlmem_AdoptSkipsAScopeErasedMidMove(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	b := m.backend.(*sqliteBackend)
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	oldLayoutScope(t, m, key)
	provisionDocFile(t, m, key)
	b.adoptStep = func(step string) error {
		if step == adoptStepHeld {
			if _, err := m.DropScope(ctx, key); err != nil {
				t.Errorf("DropScope mid-move: %v", err)
			}
		}
		return nil
	}
	if err := m.DocPlane().AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatalf("a move of an erased scope: %v", err)
	}
	agentPath, docPath := planePaths(t, m, key)
	if fileExists(agentPath) || fileExists(docPath) {
		t.Errorf("after the move: agent file %v, document file %v; want neither recreated", fileExists(agentPath), fileExists(docPath))
	}
}
