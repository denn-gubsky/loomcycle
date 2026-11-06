package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_plane_file_test.go — on sqlite the Document tool's tables live in a
// file of their own per scope, <scope>.doc.db, beside the agent's <scope>.db.

// planeFileFixture is documentFixture with the SQL Memory root in hand, so a test
// can look at the files.
func planeFileFixture(t *testing.T) (*Document, context.Context, string) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	root := t.TempDir()
	mgr, err := sqlmem.New(sqlmem.Config{Root: root})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	return &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}, ctx, root
}

// scopeFiles lists the scope files under root (no -wal/-shm), relative.
func scopeFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
		if err == nil && !de.IsDir() && strings.HasSuffix(p, ".db") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// oldScope is a user scope with a document, chunks, a proposed statement and an
// image asset, laid out as every sqlite scope was before the split: the document
// tables in the agent file, no document file. It is built the way an archive
// from then restores — the dump without its document tail.
type oldScope struct {
	key            sqlmem.ScopeKey
	docID, stmtID  string
	imageID        string
	oldLayoutDump  *sqlmem.ScopeDump
	wantChunkTexts []string
}

func buildOldScope(t *testing.T, d *Document, ctx context.Context, root string) oldScope {
	t.Helper()
	docID, a, b, _ := stmtDoc(t, d, ctx, "Old Layout")
	out, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","evidence":[{"chunk_id":%q,"quote":"comply with Article 12"}]}`, a, b, a))
	if r.IsError {
		t.Fatalf("propose_statement: %s", r.Text)
	}
	stmtID, _ := out["id"].(string)
	setAsset(t, d, ctx, b)
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SqlMem.Exec(ctx, key, `CREATE TABLE notes (body TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SqlMem.Exec(ctx, key, `INSERT INTO notes VALUES ('the agent''s own row')`, nil, 0); err != nil {
		t.Fatal(err)
	}

	dump, err := d.SqlMem.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if dump.DocTail == nil {
		t.Fatal("a scope the Document tool used exported no document tail")
	}
	dump.DocTail = nil
	if _, err := d.SqlMem.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := d.SqlMem.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	for _, f := range scopeFiles(t, root) {
		if strings.HasSuffix(f, ".doc.db") {
			t.Fatalf("the old layout has a document file: %v", scopeFiles(t, root))
		}
	}
	if held, err := d.docSQL().AgentPlaneTables(ctx, key, documentTableNames); err != nil || len(held) == 0 {
		t.Fatalf("the old layout's agent file holds no document tables (%v)", err)
	}
	return oldScope{key: key, docID: docID, stmtID: stmtID, imageID: b, oldLayoutDump: dump,
		wantChunkTexts: []string{"The licensee shall comply with Article 12", "Article 12 sets the reporting duty"}}
}

// assertMoved checks a scope after its move: every Document op sees the data, the
// agent file holds no document table and its SQL cannot name one, and the agent's
// own table is untouched.
func assertMoved(t *testing.T, d *Document, ctx context.Context, root string, s oldScope) {
	t.Helper()
	got, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","scope":"user","id":%q}`, s.docID))
	if r.IsError {
		t.Fatalf("export_md after the move: %s", r.Text)
	}
	md, _ := got["markdown"].(string)
	for _, want := range s.wantChunkTexts {
		if !strings.Contains(md, want) {
			t.Errorf("export_md lacks %q:\n%s", want, md)
		}
	}
	st, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, s.stmtID))
	if r.IsError {
		t.Fatalf("get_edge after the move: %s", r.Text)
	}
	if stm, _ := st["statement"].(map[string]any); stm["status"] != "proposed" {
		t.Errorf("statement after the move = %v, want proposed", st["statement"])
	}
	if a, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_asset","scope":"user","id":%q}`, s.imageID)); r.IsError || a["media_type"] != "image/png" {
		t.Errorf("get_asset after the move = %s", r.Text)
	}
	if _, _, ok, err := d.ReadAsset(ctx, "user", s.imageID); err != nil || !ok {
		t.Errorf("ReadAsset after the move: ok=%v err=%v", ok, err)
	}

	if held, err := d.docSQL().AgentPlaneTables(ctx, s.key, documentTableNames); err != nil || len(held) != 0 {
		t.Errorf("the agent file still holds %v (%v)", held, err)
	}
	if _, err := d.SqlMem.Query(ctx, s.key, `SELECT count(*) FROM chunks`, nil); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("agent SQL naming chunks: err = %v, want no such table", err)
	}
	if res, err := d.SqlMem.Query(ctx, s.key, `SELECT body FROM notes`, nil); err != nil || len(res.Rows) != 1 {
		t.Errorf("the agent's own table after the move = %v, %v", res, err)
	}
	files := scopeFiles(t, root)
	var doc, agent bool
	for _, f := range files {
		doc = doc || strings.HasSuffix(f, filepath.Join("user", "u1.doc.db"))
		agent = agent || strings.HasSuffix(f, filepath.Join("user", "u1.db"))
	}
	if !doc || !agent {
		t.Errorf("scope files after the move = %v; want u1.db and u1.doc.db", files)
	}
	if ok, err := d.docSQL().PlaneExists(ctx, s.key); err != nil || !ok {
		t.Errorf("PlaneExists(doc) after the move = %v, %v", ok, err)
	}
}

// A scope from before the split is moved into its own document file by the
// Document tool's next provisioning, from every point a move can stop at.
func TestSqlmem_UpgradeMovesDocumentTablesToTheirOwnFile(t *testing.T) {
	t.Run("first use", func(t *testing.T) {
		d, ctx, root := planeFileFixture(t)
		s := buildOldScope(t, d, ctx, root)
		d.SqlMem.ForgetEnsured(s.key)
		assertMoved(t, d, ctx, root, s)
	})

	// Stopped after the document file's tables were provisioned, before anything
	// moved: the document file is empty and unmarked, and does not count.
	t.Run("document file provisioned, nothing moved", func(t *testing.T) {
		d, ctx, root := planeFileFixture(t)
		s := buildOldScope(t, d, ctx, root)
		if err := d.provisionTables(provisioning(ctx), s.key); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.docSQL().PlaneExists(ctx, s.key); err != nil || ok {
			t.Fatalf("an unmoved document file counts as the plane (%v, %v)", ok, err)
		}
		d.SqlMem.ForgetEnsured(s.key)
		assertMoved(t, d, ctx, root, s)
	})

	// Stopped after the move committed, before the agent file's tables were
	// dropped: they are dropped unread, so a row an agent wrote to them in between
	// never crosses.
	t.Run("moved, old tables not dropped", func(t *testing.T) {
		d, ctx, root := planeFileFixture(t)
		s := buildOldScope(t, d, ctx, root)
		d.SqlMem.ForgetEnsured(s.key)
		if err := d.ensureSchema(ctx, s.key); err != nil {
			t.Fatal(err)
		}
		// The leftovers: the old tables back in the agent file, plus a forged
		// statement ruling.
		if err := d.SqlMem.RestoreScope(ctx, s.key, s.oldLayoutDump); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SqlMem.Exec(ctx, s.key.AgentPlane(), `UPDATE chunk_edges SET status = 'asserted'`, nil, 0); err != nil {
			t.Fatal(err)
		}
		d.SqlMem.ForgetEnsured(s.key)
		assertMoved(t, d, ctx, root, s)
	})
}

// The boot pass moves every scope from before the split without waiting for the
// Document tool's first use of each, so a reader that does not provision — the
// ontology resolver, the change feed — finds the documents where they now live.
func TestDocument_SplitDocumentPlanesMovesEveryOldScope(t *testing.T) {
	d, ctx, root := planeFileFixture(t)
	s := buildOldScope(t, d, ctx, root)
	d.SqlMem.ForgetEnsured(s.key)
	n, err := d.SplitDocumentPlanes(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("SplitDocumentPlanes = %d, %v; want 1 scope moved", n, err)
	}
	if ok, err := d.docSQL().PlaneExists(ctx, s.key); err != nil || !ok {
		t.Fatalf("PlaneExists(doc) after the boot pass = %v, %v", ok, err)
	}
	if held, _ := d.docSQL().AgentPlaneTables(ctx, s.key, documentTableNames); len(held) != 0 {
		t.Errorf("the agent file still holds %v", held)
	}
	if n, err := d.SplitDocumentPlanes(context.Background()); err != nil || n != 0 {
		t.Errorf("a second pass = %d, %v; want nothing to move", n, err)
	}
	assertMoved(t, d, ctx, root, s)
}

// The dead-link GC on a scope from before the split moves it first (its read of
// the live chunk ids provisions), so no live body looks orphaned; after the move
// it reaps an orphan in the document file.
func TestDeadLink_AgainstTheTwoFiles(t *testing.T) {
	d, ctx, root := planeFileFixture(t)
	docID := newEntityDoc(t, d, ctx)
	key, mscope := deadLinkScope(t, d, ctx)
	live := upsert(t, d, ctx, docID, "live", "live", "kept body", "")
	doomed := upsert(t, d, ctx, docID, "doomed", "doomed", "orphaned body", "")

	// The old layout, via an archive without its document tail.
	dump, err := d.SqlMem.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	dump.DocTail = nil
	if _, err := d.SqlMem.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := d.SqlMem.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	d.SqlMem.ForgetEnsured(key)

	rep, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile on the old layout: %v", err)
	}
	if rep.Total() != 0 || rep.Skipped != "" {
		t.Errorf("reconcile on the old layout = %+v; want nothing collected, nothing skipped", rep)
	}
	for _, id := range []string{live, doomed} {
		if !bodyExists(t, d, ctx, mscope, key, id) {
			t.Errorf("live body %s was deleted", id)
		}
	}
	if held, _ := d.docSQL().AgentPlaneTables(ctx, key, documentTableNames); len(held) != 0 {
		t.Errorf("the reconcile did not move the scope: agent file holds %v", held)
	}

	// An out-of-band delete in the document file leaves an orphan body and
	// sidecar, which the next pass collects there.
	if err := d.exec(ctx, key, `DELETE FROM chunks WHERE id = ?`, doomed); err != nil {
		t.Fatal(err)
	}
	rep, err = d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bodies != 1 || rep.Sidecars != 1 {
		t.Errorf("reconcile after the move = %+v; want the orphan body and sidecar", rep)
	}
	if !bodyExists(t, d, ctx, mscope, key, live) || bodyExists(t, d, ctx, mscope, key, doomed) {
		t.Error("the reconcile reaped the wrong body")
	}
	if files := scopeFiles(t, root); len(files) != 2 {
		t.Errorf("scope files = %v; want the agent and document files", files)
	}
}
