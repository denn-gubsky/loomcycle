package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// RFC GC P2: a name that lands under a document name succeeds, and its result
// says so — the document now also has children, which a reader of the tree
// can miss.

const parentIsDocumentAB = "parent_is_document: /a/b names a document; this name is listed under it"

func resultWarnings(t *testing.T, text string) []string {
	t.Helper()
	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode result: %v (%s)", err, text)
	}
	return out.Warnings
}

func wantParentIsDocument(t *testing.T, what, text string) {
	t.Helper()
	if w := resultWarnings(t, text); len(w) != 1 || w[0] != parentIsDocumentAB {
		t.Errorf("%s: want warnings [%q], got %v (%s)", what, parentIsDocumentAB, w, text)
	}
}

func wantNoWarnings(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, `"warnings"`) {
		t.Errorf("%s: want no warnings, got %s", what, text)
	}
}

func createDocAt(t *testing.T, d *Document, ctx context.Context, path string) tools.Result {
	t.Helper()
	_, res := docExec(t, d, ctx, `{"op":"create_document","title":"t","path":"`+path+`"}`)
	if res.IsError {
		t.Fatalf("create_document %s: %s", path, res.Text)
	}
	return res
}

func TestDocumentSetPath_UnderADocumentWarnsParentIsDocument(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	createDocAt(t, d, ctx, "/a/b")
	out, _ := docExec(t, d, ctx, `{"op":"create_document","title":"other"}`)
	id := asStr(out["document_id"])

	_, res := docExec(t, d, ctx, `{"op":"set_path","id":"`+id+`","path":"/a/b/d"}`)
	if res.IsError {
		t.Fatalf("set_path under a document must succeed: %s", res.Text)
	}
	wantParentIsDocument(t, "set_path /a/b/d", res.Text)

	_, res = docExec(t, d, ctx, `{"op":"set_path","id":"`+id+`","path":"/a/plain/d"}`)
	if res.IsError {
		t.Fatalf("set_path under a folder: %s", res.Text)
	}
	wantNoWarnings(t, "set_path under a plain folder", res.Text)
}

func TestDocumentCreateAndImport_UnderADocumentWarnParentIsDocument(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	createDocAt(t, d, ctx, "/a/b")

	wantParentIsDocument(t, "create_document /a/b/c", createDocAt(t, d, ctx, "/a/b/c").Text)
	wantNoWarnings(t, "create_document /a/x/c", createDocAt(t, d, ctx, "/a/x/c").Text)
	// The default name /documents/<title> sits under a folder.
	_, res := docExec(t, d, ctx, `{"op":"create_document","title":"plain"}`)
	wantNoWarnings(t, "create_document with the default path", res.Text)

	_, res = docExec(t, d, ctx, `{"op":"import_md","path":"/a/b/m","markdown":"# M\n\ntext\n"}`)
	if res.IsError {
		t.Fatalf("import_md under a document must succeed: %s", res.Text)
	}
	wantParentIsDocument(t, "import_md /a/b/m", res.Text)
	_, res = docExec(t, d, ctx, `{"op":"import_md","path":"/a/x/m","markdown":"# M\n\ntext\n"}`)
	wantNoWarnings(t, "import_md under a folder", res.Text)
}

func TestFileImport_UnderADocumentWarnsParentIsDocument(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	createDocAt(t, d, ctx, "/a/b")
	kept := inbox.Capture(ctx, inbox.Item{Name: "n.md", MediaType: "text/markdown", Origin: "mcp:trusted/get"}, []byte("# Notes\n\ntext\n"))

	_, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "doc_path": "/a/b/n"})
	if res.IsError {
		t.Fatalf("import under a document must succeed: %s", res.Text)
	}
	wantParentIsDocument(t, "File import doc_path /a/b/n", res.Text)

	_, res = fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "doc_path": "/a/x/n"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	wantNoWarnings(t, "File import under a folder", res.Text)
}

func TestPathMv_UnderADocumentWarnsParentIsDocument(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/a/", "b", "document")
	seedAgent(t, s, "/", "x", "document")
	seedAgent(t, s, "/", "y", "document")
	seedAgent(t, s, "/a/", "dir", "directory")

	_, res := pathExec(t, p, ctx, `{"op":"mv","path":"/x","to":"/a/b/x"}`)
	if res.IsError {
		t.Fatalf("mv under a document must succeed: %s", res.Text)
	}
	wantParentIsDocument(t, "mv /x /a/b/x", res.Text)

	_, res = pathExec(t, p, ctx, `{"op":"mv","path":"/y","to":"/a/dir/y"}`)
	if res.IsError {
		t.Fatalf("mv under a directory: %s", res.Text)
	}
	wantNoWarnings(t, "mv under a mkdir'd directory", res.Text)
}

// The warning names the parent, so it is given only to a caller that may read
// that name — otherwise it would say a hidden document exists.
func TestDocumentSetPath_ParentIsDocumentWarningNeedsTheParentReadable(t *testing.T) {
	d, base, _ := documentFixture(t)
	seed := tenantDocCtx(base, nil)
	if _, res := docExec(t, d, seed, `{"op":"create_document","scope":"tenant","title":"t","path":"/a/b"}`); res.IsError {
		t.Fatal(res.Text)
	}
	hidden := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/a/b"}}))
	_, res := docExec(t, d, hidden, `{"op":"create_document","scope":"tenant","title":"c","path":"/a/b/c"}`)
	if res.IsError {
		t.Fatalf("create_document: %s", res.Text)
	}
	wantNoWarnings(t, "parent unreadable", res.Text)
}
