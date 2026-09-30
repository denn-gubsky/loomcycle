package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A document written through the real Document tool, snapshotted with its SQL
// Memory, and restored through each transport comes back reachable by the
// path it was named at — the explicit one and the default one alike — and is
// listed by the Path tool under its parent, in its own tree only. Without SQL
// Memory on the target no document can be there, so no document name is
// restored, and the restore says so.

// direntDocCtx is the identity a run carries, with the grants a tenant-scope
// document needs.
func direntDocCtx(tenant, user string) context.Context {
	ctx := tools.WithAgentName(context.Background(), "writer")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: user, TenantID: tenant})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}})
	return tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}})
}

type restoredDoc struct {
	ctx   context.Context
	scope string
	path  string
	id    string
}

func docCall(t *testing.T, d *builtin.Document, ctx context.Context, in map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := d.Execute(ctx, raw)
	if err != nil || res.IsError {
		t.Fatalf("Document %v: %v %s", in["op"], err, res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("Document %v: %v", in["op"], err)
	}
	return out
}

func TestRestore_EveryCallSiteBringsDocumentsBackAtTheirPaths(t *testing.T) {
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	srcMem, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srcMem.Close() })
	author := &builtin.Document{Store: src, SqlMem: srcMem}

	docs := []restoredDoc{
		{ctx: direntDocCtx("", "u1"), scope: "user", path: "/projects/q3/plan"}, // explicit, nested
		{ctx: direntDocCtx("acme", "alice"), scope: "user"},                     // default /documents/<title>
		{ctx: direntDocCtx("acme", "alice"), scope: "tenant", path: "/handbook/onboarding"},
	}
	for i := range docs {
		in := map[string]any{"op": "create_document", "scope": docs[i].scope, "title": "Launch plan"}
		if docs[i].path != "" {
			in["path"] = docs[i].path
		}
		out := docCall(t, author, docs[i].ctx, in)
		docs[i].id, _ = out["document_id"].(string)
		docs[i].path, _ = out["path"].(string)
		if docs[i].id == "" || docs[i].path == "" {
			t.Fatalf("create_document returned %v", out)
		}
	}
	if !strings.HasPrefix(docs[1].path, "/documents/") {
		t.Fatalf("default path = %q; this test assumes create_document's /documents/<title> default", docs[1].path)
	}
	_, envelope, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{SqlMem: srcMem})
	if err != nil {
		t.Fatal(err)
	}

	for _, via := range []string{"connector", "http"} {
		t.Run(via, func(t *testing.T) {
			srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
			mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mgr.Close() })
			srv.sqlMem = mgr

			restored, warnings := restoreWarnings(t, srv, via, envelope)
			if restored["dirents"] != len(docs) {
				t.Errorf("restored dirents = %d, want %d; warnings %v", restored["dirents"], len(docs), warnings)
			}
			for _, w := range warnings {
				if strings.HasPrefix(w, "dirent") {
					t.Errorf("a clean restore warned about a name: %s", w)
				}
			}
			reader := &builtin.Document{Store: srv.store, SqlMem: mgr}
			pathTool := &builtin.Path{Store: srv.store}
			for _, d := range docs {
				got := docCall(t, reader, d.ctx, map[string]any{"op": "get_document", "scope": d.scope, "path": d.path})
				if got["document_id"] != d.id {
					t.Errorf("get_document %s = %v, want document %s", d.path, got["document_id"], d.id)
				}
				parent := d.path[:strings.LastIndex(d.path, "/")]
				raw, _ := json.Marshal(map[string]any{"op": "ls", "scope": d.scope, "path": parent})
				res, err := pathTool.Execute(d.ctx, raw)
				if err != nil || res.IsError {
					t.Fatalf("ls %s: %v %s", parent, err, res.Text)
				}
				leaf := d.path[strings.LastIndex(d.path, "/")+1:]
				if !strings.Contains(res.Text, `"name":"`+leaf+`","kind":"document"`) {
					t.Errorf("ls %s does not list %s: %s", parent, leaf, res.Text)
				}
			}
			// The ancestors of the nested name list as directories.
			raw, _ := json.Marshal(map[string]any{"op": "ls", "scope": "user", "path": "/projects"})
			if res, _ := pathTool.Execute(docs[0].ctx, raw); !strings.Contains(res.Text, `"name":"q3","kind":"directory"`) {
				t.Errorf("ls /projects = %s, want the implicit directory q3", res.Text)
			}
			// acme's names are in acme's trees only.
			other := direntDocCtx("beta", "alice")
			raw, _ = json.Marshal(map[string]any{"op": "resolve", "scope": "user", "path": docs[1].path})
			if res, _ := pathTool.Execute(other, raw); !res.IsError {
				t.Errorf("tenant beta resolves acme's %s: %s", docs[1].path, res.Text)
			}
		})
	}

	t.Run("no SQL Memory on the target", func(t *testing.T) {
		srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
		restored, warnings := restoreWarnings(t, srv, "http", envelope)
		if restored["dirents"] != 0 {
			t.Errorf("restored %d document names with no SQL Memory to hold the documents", restored["dirents"])
		}
		skipped := 0
		for _, w := range warnings {
			if strings.HasPrefix(w, "dirents: ") && strings.Contains(w, "SQL Memory is not enabled") {
				skipped++
			}
		}
		if skipped != 3 { // one per tree: "" u1, acme alice, acme tenant
			t.Errorf("warnings about unrestorable document names = %d, want one per tree: %v", skipped, warnings)
		}
		if rows, _ := srv.store.SnapshotReadDirents(context.Background()); len(rows) != 0 {
			t.Errorf("names left pointing at nothing: %+v", rows)
		}
	})
}

// A document named twice keeps both names on restore: set_path adds a name, it
// does not move the first one.
func TestRestore_DocumentKeepsEveryNameItHad(t *testing.T) {
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	srcMem, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srcMem.Close() })
	author := &builtin.Document{Store: src, SqlMem: srcMem}
	ctx := direntDocCtx("acme", "alice")
	out := docCall(t, author, ctx, map[string]any{"op": "create_document", "scope": "user", "title": "Spec", "path": "/a/spec"})
	id, _ := out["document_id"].(string)
	docCall(t, author, ctx, map[string]any{"op": "set_path", "scope": "user", "id": id, "path": "/b/also-spec"})
	_, envelope, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{SqlMem: srcMem})
	if err != nil {
		t.Fatal(err)
	}

	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv.sqlMem = mgr
	if restored, warnings := restoreWarnings(t, srv, "connector", envelope); restored["dirents"] != 2 {
		t.Fatalf("restored dirents = %d, want both names; warnings %v", restored["dirents"], warnings)
	}
	reader := &builtin.Document{Store: srv.store, SqlMem: mgr}
	for _, p := range []string{"/a/spec", "/b/also-spec"} {
		if got := docCall(t, reader, ctx, map[string]any{"op": "get_document", "scope": "user", "path": p}); got["document_id"] != id {
			t.Errorf("%s resolves to %v, want %s", p, got["document_id"], id)
		}
	}
	if rows, _ := srv.store.SnapshotReadDirents(context.Background()); len(rows) != 2 {
		t.Errorf("target names = %+v, want exactly the two", rows)
	}
}
