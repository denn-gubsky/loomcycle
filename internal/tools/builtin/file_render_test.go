package builtin

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// renderFixtureDoc imports a small document with a section, a picture and a
// link from one section to another, and returns its id and root chunk.
func renderFixtureDoc(t *testing.T, d *Document, ctx context.Context) (docID, rootID string) {
	t.Helper()
	pic := base64.StdEncoding.EncodeToString(testPNG(t))
	md := "# Протокол\n\nВступ <script>alert(1)</script>.\n\n## Порядок денний\n\nПерше питання.\n\n## Рішення\n\n![Схема](data:image/png;base64," + pic + ")\n"
	out, res := docExec(t, d, ctx, `{"op":"import_md","scope":"user","path":"/board/protokol","markdown":`+jsonString(md)+`}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	docID = asStr(out["document_id"])
	got, res := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	return docID, asStr(got["root_chunk_id"])
}

func TestFileRender_DocumentToInboxAsHTMLIsLogged(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, rootID := renderFixtureDoc(t, d, ctx)
	out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html", "toc": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	ref := asStr(out["ref"])
	if !strings.HasPrefix(ref, "volume:inbox:rendered/") || !strings.HasSuffix(ref, ".html") {
		t.Fatalf("ref = %q", ref)
	}
	b, err := os.ReadFile(inboxFile(t, tools.EphemeralVolumes(ctx), ref))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{"<title>Протокол</title>", "Порядок денний", "&lt;script&gt;alert(1)&lt;/script&gt;", `<img src="data:image/png;base64,`, `href="#chunk-`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Error("a body's script tag reached the page")
	}
	renderID := asStr(out["render_id"])
	if renderID == "" || out["reused"] != false {
		t.Fatalf("result = %v", out)
	}

	got, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	renders, _ := got["renders"].(map[string]any)
	html, _ := renders["html"].(map[string]any)
	if asStr(html["render_id"]) != renderID || asStr(html["output_sha256"]) != asStr(out["sha256"]) || asStr(html["destination"]) != "inbox" {
		t.Fatalf("get_document renders = %v", got["renders"])
	}
	hist, r := docExec(t, d, ctx, `{"op":"history","scope":"user","id":"`+rootID+`","include":["renders"]}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if rs, _ := hist["renders"].([]any); len(rs) != 1 {
		t.Fatalf("history renders = %v", hist["renders"])
	}
}

func TestFileRender_MarkdownLinksPicturesWrittenBesideIt(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	for i := 0; i < 2; i++ { // the second render's names collide with the first's
		out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "doc_path": "/board/protokol", "format": "md"})
		if res.IsError {
			t.Fatal(res.Text)
		}
		refs, _ := out["image_refs"].([]any)
		if len(refs) != 1 {
			t.Fatalf("image_refs = %v", out["image_refs"])
		}
		mdPath := inboxFile(t, tools.EphemeralVolumes(ctx), asStr(out["ref"]))
		md, _ := os.ReadFile(mdPath)
		imgPath := inboxFile(t, tools.EphemeralVolumes(ctx), asStr(refs[0]))
		rel, _ := filepath.Rel(filepath.Dir(mdPath), imgPath)
		if !strings.Contains(string(md), "](<"+filepath.ToSlash(rel)+">)") {
			t.Fatalf("render %d: the Markdown does not link %s:\n%s", i, rel, md)
		}
		if asStr(out["document_id"]) != docID {
			t.Fatalf("document_id = %v", out["document_id"])
		}
	}
}

func TestFileRender_ReusesAnUnchangedVolumeOutput(t *testing.T) {
	f, ctx, d, docs := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	first, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html", "to": "volume:docs:minutes.html"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if first["reused"] != false || asStr(first["ref"]) != "volume:docs:minutes.html" {
		t.Fatalf("first = %v", first)
	}
	again, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if again["reused"] != true || asStr(again["ref"]) != "volume:docs:minutes.html" {
		t.Fatalf("an unchanged document was rendered again: %v", again)
	}
	copied, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html", "to": "volume:docs:copy.html"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if copied["reused"] != true {
		t.Fatalf("copy = %v", copied)
	}
	a, _ := os.ReadFile(filepath.Join(docs, "minutes.html"))
	b, _ := os.ReadFile(filepath.Join(docs, "copy.html"))
	if string(a) != string(b) {
		t.Fatal("the reused copy differs")
	}
	// A different option is different content; a changed file is not reused.
	if out, _ := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html", "toc": true}); out["reused"] != false {
		t.Fatalf("a render with other options was reused: %v", out)
	}
	if err := os.WriteFile(filepath.Join(docs, "minutes.html"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "copy.html"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html"}); out["reused"] != false {
		t.Fatalf("an edited file was reused: %v", out)
	}
	// An existing file is not replaced without overwrite.
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "md", "to": "volume:docs:minutes.html"}); !res.IsError {
		t.Fatal("an existing file was replaced without overwrite")
	}
}

func TestFileRender_SubtreeIsTitledByItsChunk(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	q, res := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","document_id":"`+docID+`"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	var chunk string
	for _, c := range q["chunks"].([]any) {
		if m := c.(map[string]any); asStr(m["title"]) == "Рішення" {
			chunk = asStr(m["id"])
		}
	}
	if chunk == "" {
		t.Fatalf("no section: %v", q)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "chunk": chunk, "format": "md"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	md, _ := os.ReadFile(inboxFile(t, tools.EphemeralVolumes(ctx), asStr(out["ref"])))
	if !strings.HasPrefix(string(md), "# Рішення\n") || strings.Contains(string(md), "Порядок") {
		t.Fatalf("subtree render:\n%s", md)
	}
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "chunk": "nope", "format": "md"}); !res.IsError {
		t.Fatal("a chunk of no document was rendered")
	}
}

func TestFileRender_NeedsTheDocumentTool(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = []string{"File", "Read", "Write"} })
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html"}); !res.IsError || !strings.Contains(res.Text, "Document tool") {
		t.Fatalf("res = %+v", res)
	}
}

func TestFileRender_RefusesBadArguments(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	for name, in := range map[string]map[string]any{
		"later format":   {"op": "render", "document": docID, "format": "pdf"},
		"unknown format": {"op": "render", "document": docID, "format": "rtf"},
		"no format":      {"op": "render", "document": docID},
		"two sources":    {"op": "render", "document": docID, "doc_path": "/board/protokol", "format": "md"},
		"no source":      {"op": "render", "format": "md"},
		"inbox to":       {"op": "render", "document": docID, "format": "md", "to": "volume:inbox:x.md"},
		"read-only to":   {"op": "render", "document": docID, "format": "md", "to": "volume:ref:x.md"},
		"bad page size":  {"op": "render", "document": docID, "format": "md", "page_size": "a3"},
		"no such doc":    {"op": "render", "document": "0123", "format": "md"},
	} {
		if _, res := fileOp(t, f, ctx, in); !res.IsError {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestFileRender_MarkdownFileSource(t *testing.T) {
	f, ctx, _, docs := fileDocRun(t)
	if err := os.WriteFile(filepath.Join(docs, "notes.md"), []byte("# Нотатки\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "path": "volume:docs:notes.md", "format": "html"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	page, _ := os.ReadFile(inboxFile(t, tools.EphemeralVolumes(ctx), asStr(out["ref"])))
	if !strings.Contains(string(page), "<title>Нотатки</title>") || !strings.Contains(string(page), "<td>2</td>") {
		t.Fatalf("page:\n%s", page)
	}
	if _, ok := out["render_id"]; ok {
		t.Error("a file render was logged as a document's")
	}
}

func TestFileRender_DeletingTheDocumentDropsItsRenders(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "html"}); res.IsError {
		t.Fatal(res.Text)
	}
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM document_renders WHERE document_id = ?`, docID)
		if err != nil {
			t.Fatal(err)
		}
		return asInt(res.Rows[0][0])
	}
	if count() != 1 {
		t.Fatal("no render logged")
	}
	if _, res := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+docID+`"}`); res.IsError {
		t.Fatal(res.Text)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d renders left behind", n)
	}
}

// export_md is unchanged by the walk it now shares with render: a document's
// export is byte-for-byte what it was.
func TestExportMD_SharedWalkKeepsTheOutput(t *testing.T) {
	_, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	out, res := docExec(t, d, ctx, `{"op":"export_md","scope":"user","id":"`+docID+`","include_metadata":false}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	md := asStr(out["markdown"])
	pic := base64.StdEncoding.EncodeToString(testPNG(t))
	want := "# Протокол\n\nВступ <script>alert(1)</script>.\n\n## Порядок денний\n\nПерше питання.\n\n## Рішення\n\n![Схема](data:image/png;base64," + pic + ")\n\n"
	if md != want {
		t.Fatalf("export_md =\n%q\nwant\n%q", md, want)
	}
}

// A document's render log moves with it; the scope it left keeps none.
func TestMoveDocuments_CarriesTheRenderLog(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	ukey, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	pc := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`)
	row := renderRow{DocumentID: s.planDoc, RootChunkID: asStr(pc["root_chunk_id"]), SourceSHA256: "s", Format: "html",
		Options: "{}", OutputSHA256: "o", Size: 1, Destination: "inbox", Actor: "r"}
	if err := (&renderLog{d: d, key: ukey}).record(ctx, &row); err != nil {
		t.Fatal(err)
	}
	moveSet(t, d, ctx, `"under_path":"/proj"`)
	moved := copyExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.planDoc+`"}`)
	html, _ := moved["renders"].(map[string]any)["html"].(map[string]any)
	if asStr(html["render_id"]) != row.ID {
		t.Fatalf("the render log did not move: %v", moved["renders"])
	}
	res, err := d.query(ctx, ukey, `SELECT count(*) FROM document_renders`)
	if err != nil || asInt(res.Rows[0][0]) != 0 {
		t.Fatalf("renders left in the source scope: %v %v", res, err)
	}
}

// A Document rendered as Word and extracted again keeps its sections and
// its picture: the round trip RFC GR §7 asks for, through the tool.
func TestFileRender_DOCXRoundTripsThroughExtract(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "docx"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.HasSuffix(asStr(out["ref"]), ".docx") || asStr(out["media_type"]) != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("result = %v", out)
	}
	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": out["ref"]})
	if res.IsError {
		t.Fatal(res.Text)
	}
	text := asStr(ex["excerpt"])
	for _, want := range []string{"# Протокол", "## Порядок денний", "Перше питання", "## Рішення", "alert(1)"} {
		if !strings.Contains(text, want) {
			t.Errorf("the extracted render lacks %q:\n%s", want, text)
		}
	}
	if refs, _ := ex["image_refs"].([]any); len(refs) != 1 {
		t.Errorf("image_refs = %v", ex["image_refs"])
	}
}
