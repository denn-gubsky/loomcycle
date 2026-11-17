package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// editDOCX rewrites the text of a DOCX's body as a person editing it in Word
// would, every other part kept.
func editDOCX(t *testing.T, data []byte, from, to []string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "word/document.xml" {
			for i := range from {
				if !bytes.Contains(b, []byte(from[i])) {
					t.Fatalf("the document has no %q", from[i])
				}
				b = bytes.ReplaceAll(b, []byte(from[i]), []byte(to[i]))
			}
		}
		w, _ := zw.Create(f.Name)
		_, _ = w.Write(b)
	}
	_ = zw.Close()
	return out.Bytes()
}

// A file loomcycle rendered with metadata embed, edited in Word and brought
// back, re-imports onto its document (RFC GR P4b, D22): a part whose heading
// was renamed and whose text was edited is matched by the id its bookmark
// carries and superseded, keeping its lineage, not closed beside a new one.
func TestFileImport_ARenderedFileReimportsOntoItsDocument(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"user","path":"/decks/levels","markdown":"# Services\n\n## Basic\n\nNo test plan.\n\n## Standard\n\nA test plan.\n"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	docID := asStr(out["document_id"])
	standard, _, _ := importedChunk(t, d, ctx, docID, "Standard")

	rr, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "docx", "metadata": "embed"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	orig, err := os.ReadFile(inboxFile(t, tools.EphemeralVolumes(ctx), asStr(rr["ref"])))
	if err != nil {
		t.Fatal(err)
	}
	edited := editDOCX(t, orig, []string{">Standard<", "A test plan."}, []string{">Standard (MVP)<", "A detailed test plan."})
	kept := inbox.Capture(ctx, inbox.Item{Name: "levels.docx", Origin: "http"}, edited)

	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	from, _ := ex["rendered_from"].(map[string]any)
	if asStr(from["document_id"]) != docID || asStr(from["scope"]) != "user" || asInt(from["headings_matched"]) != 2 {
		t.Fatalf("rendered_from = %v", ex["rendered_from"])
	}
	if strings.Contains(asStr(ex["excerpt"]), "loomcycle_render") {
		t.Error("the record reached the extracted text")
	}

	imp, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"], "reimport": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if asStr(imp["document_id"]) != docID {
		t.Fatalf("re-imported onto %v, want %s", imp["document_id"], docID)
	}
	rep, _ := imp["reimport"].(map[string]any)
	if asInt(rep["superseded"]) != 1 || asInt(rep["created"]) != 0 || asInt(rep["closed"]) != 0 {
		t.Fatalf("reimport = %v, want the edited part superseded and nothing created or closed", rep)
	}
	now, _, _ := importedChunk(t, d, ctx, docID, "Standard (MVP)")
	if now == nil || asStr(now["id"]) == asStr(standard["id"]) {
		t.Fatalf("the edited part = %v", now)
	}

	// Refused where it would mean something else.
	for name, in := range map[string]map[string]any{
		"another scope": {"op": "import", "path": ex["ref"], "reimport": true, "scope": "tenant"},
		"headings":      {"op": "import", "path": ex["ref"], "reimport": true, "headings": []any{map[string]any{"line": 1, "as": "drop"}}},
	} {
		if _, res := fileOp(t, f, ctx, in); !res.IsError {
			t.Errorf("%s: was not refused", name)
		}
	}

	// A record is someone's file: one naming a document the caller does not
	// have re-imports nowhere, and nothing is written.
	forged := forgeRecord(t, orig, docID, "0123456789abcdef0123456789abcdef")
	fk := inbox.Capture(ctx, inbox.Item{Name: "forged.docx", Origin: "http"}, forged)
	fex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": fk.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": fex["ref"], "reimport": true}); !res.IsError {
		t.Error("a record naming a document the caller does not have was re-imported")
	}

	// Without reimport it is a new document that says what it came from.
	nw, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"], "doc_path": "/decks/levels-copy"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if asStr(nw["document_id"]) == docID {
		t.Fatal("an import without reimport went onto the rendered document")
	}
	g, _ := json.Marshal(map[string]any{"op": "get_document", "scope": "user", "id": nw["document_id"]})
	doc, r := docExec(t, d, ctx, string(g))
	if r.IsError {
		t.Fatal(r.Text)
	}
	prov, _ := doc["provenance"].(map[string]any)
	rf, _ := prov["rendered_from"].(map[string]any)
	if asStr(rf["document_id"]) != docID {
		t.Errorf("the new document's provenance = %v, want rendered_from %s", doc["provenance"], docID)
	}
	_ = context.Background
}

// forgeRecord rewrites the document id in a DOCX's embedded record.
func forgeRecord(t *testing.T, data []byte, from, to string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "customXml/item1.xml" {
			b = bytes.ReplaceAll(b, []byte(from), []byte(to))
		}
		w, _ := zw.Create(f.Name)
		_, _ = w.Write(b)
	}
	_ = zw.Close()
	return out.Bytes()
}
