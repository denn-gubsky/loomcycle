package builtin

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// An import from the inbox says where the document came from: the runtime's
// record of the file captured (through the extraction that read it), and the
// link the agent gives. Its text is stamped as imported, so it is searched
// as a document and never served as a fact.
func TestFileImport_RootCarriesTheCaptureAndTheSource(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "Протокол.docx", Origin: "mcp:ms365/get-attachment"}, minutesDOCX(t, testPNG(t)))
	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	const secret = "SIGSECRET0123456789"
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"], "source": map[string]any{
		"url":   "https://tenant.sharepoint.com/sites/board/Shared%20Documents/p.docx?web=1&sig=" + secret,
		"kind":  "sharepoint",
		"title": "Протокол\nзасідання",
	}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if strings.Contains(res.Text, secret) {
		t.Errorf("the import's result shows the source URL's credential: %s", res.Text)
	}
	docID := asStr(out["document_id"])
	got, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	p, _ := got["provenance"].(map[string]any)
	rec := kept.Record
	for k, want := range map[string]any{
		"origin": store.OriginImport, "file": "Protokol.docx", "media_type": rec.MediaType, "sha256": rec.SHA256,
		"captured_from": "mcp:ms365/get-attachment", "run_id": rec.RunID,
		"source_kind": "sharepoint", "source_title": "Протокол засідання",
	} {
		if p[k] != want {
			t.Errorf("provenance %s = %v, want %v", k, p[k], want)
		}
	}
	if asStr(p["captured_at"]) == "" {
		t.Error("provenance has no captured_at")
	}
	if u := asStr(p["source_url"]); strings.Contains(u, secret) || !strings.HasPrefix(u, "https://tenant.sharepoint.com/sites/board/Shared%20Documents/p.docx?web=1&sig=[redacted") {
		t.Errorf("source_url = %q", u)
	}
	if tags, _ := got["tags"].([]any); !slices.Contains(tags, any("source/sharepoint")) || !slices.Contains(tags, any("source/mcp/ms365")) {
		t.Errorf("tags = %v", got["tags"])
	}
	for _, id := range importChunkIDs(t, d, ctx, docID) {
		if o := bodyOriginOf(t, d, ctx, d.Store, id); o != store.OriginImport {
			t.Errorf("chunk %s body origin = %q, want %q", id, o, store.OriginImport)
		}
	}
}

// A downloaded file links back to its download when the agent gives no
// source: the URL as the record keeps it, without its query. A document
// whose text has no heading is titled after the file, not its inbox folder.
func TestFileImport_DownloadDefaultsTheSourceURL(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "board-notes.md", MediaType: "text/markdown", Origin: "download:files.example",
		URL: "https://files.example/dl/board-notes.md?k=SECRETK123456"}, []byte("No heading here.\n\nJust notes.\n"))
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+asStr(out["document_id"])+`"}`)
	p, _ := got["provenance"].(map[string]any)
	if p["source_url"] != "https://files.example/dl/board-notes.md?…" || p["source_kind"] != nil || p["captured_from"] != "download:files.example" {
		t.Errorf("provenance = %v", p)
	}
	if got["title"] != "board-notes" {
		t.Errorf("title = %v, want the file's name without its extension", got["title"])
	}
}

// The capture record describes the file only while its bytes are the
// captured ones: an import of a file changed since records none of it,
// though the import is still an import.
func TestFileImport_ChangedFileCarriesNoCaptureFields(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "n.md", MediaType: "text/markdown", Origin: "mcp:trusted/get"}, []byte("# Notes\n\ntext\n"))
	if err := os.WriteFile(inboxFile(t, tools.EphemeralVolumes(ctx), kept.Record.Ref), []byte("# Notes\n\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+asStr(out["document_id"])+`"}`)
	p, _ := got["provenance"].(map[string]any)
	if len(p) != 1 || p["origin"] != store.OriginImport {
		t.Errorf("provenance = %v, want the origin alone", p)
	}
}

func TestFileImport_RefusesABadSource(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "n.md", MediaType: "text/markdown", Origin: "http"}, []byte("# Notes\n"))
	for _, src := range []map[string]any{
		{"url": "file:///etc/passwd"},
		{"url": "https://u:p@files.example/a"},
		{"kind": "fax"},
	} {
		if _, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "source": src}); !res.IsError {
			t.Errorf("source %v accepted", src)
		}
	}
}

// The File tool's schema offers source on import, with the kinds it takes.
func TestFile_SchemaOffersImportSource(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&File{}).InputSchema(), &s); err != nil {
		t.Fatal(err)
	}
	src, ok := s.Properties["source"]
	if !ok || src.Type != "object" {
		t.Fatalf("source = %+v", src)
	}
	if !slices.Equal(src.Properties["kind"].Enum, ImportSourceKinds) {
		t.Errorf("kind enum = %v, want %v", src.Properties["kind"].Enum, ImportSourceKinds)
	}
}
