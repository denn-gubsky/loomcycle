package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

func testJPEG(t *testing.T) []byte { return testJPEGOf(t, 200) }

func testJPEGOf(t *testing.T, red uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(2, 2, color.RGBA{R: red, A: 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// boardMinutesPDF is a two-page protocol whose picture is on page 2.
func boardMinutesPDF(t *testing.T, pic []byte) []byte {
	return pdftest.Doc{Pages: []pdftest.Page{
		{Lines: []pdftest.Line{
			{Text: "Протокол засідання правління", X: 72, Y: 770, Size: 16},
			{Text: "Рішення: затвердити бюджет на 2027 рік.", X: 72, Y: 740, Size: 11},
		}},
		{
			Lines:  []pdftest.Line{{Text: "Додаток: схема приміщення.", X: 72, Y: 770, Size: 11}},
			Images: []pdftest.Image{{JPEG: pic, W: 8, H: 8, X: 72, Y: 500, Width: 100, Height: 100}},
		},
	}}.Bytes()
}

// chunkFields reads a chunk's fields by its title.
func chunkFields(t *testing.T, d *Document, ctx context.Context, docID, title string) map[string]any {
	t.Helper()
	c, _, _ := importedChunk(t, d, ctx, docID, title)
	g, _ := json.Marshal(map[string]any{"op": "get_chunk", "scope": "user", "id": c["id"]})
	got, r := docExec(t, d, ctx, string(g))
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	f, _ := got["fields"].(map[string]any)
	return f
}

// A PDF the runtime kept is extracted to Markdown in the inbox, its JPEG
// kept as the bytes it was stored as, and imported: the picture becomes an
// image chunk that says which page it was on.
func TestFileExtract_PDFEndToEnd(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	pic := testJPEG(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "Протокол.pdf", Origin: "mcp:ms365/get-attachment"}, boardMinutesPDF(t, pic))
	if !kept.Kept || kept.Record.MediaType != "application/pdf" {
		t.Fatalf("capture = %+v", kept)
	}
	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if ex["format"] != "pdf" || asInt(ex["pages"]) != 2 || asInt(ex["images"]) != 1 {
		t.Fatalf("extract = %v", ex)
	}
	if y, _ := ex["yield_chars_per_page"].(float64); y < 20 {
		t.Errorf("yield = %v", ex["yield_chars_per_page"])
	}
	if ex_, _ := ex["excerpt"].(string); !strings.Contains(ex_, "Рішення: затвердити бюджет на 2027 рік.") || !strings.HasPrefix(ex_, "<file_content ") {
		t.Errorf("excerpt = %q", ex["excerpt"])
	}
	const img = "volume:inbox:extracted/Protokol/img-1.jpg"
	if refs, _ := ex["image_refs"].([]any); len(refs) != 1 || refs[0] != img {
		t.Fatalf("image_refs = %v", ex["image_refs"])
	}
	r, _ := inboxRun(ctx)
	got, err := os.ReadFile(inboxFile(t, r, img))
	if err != nil || !bytes.Equal(got, pic) {
		t.Fatalf("the JPEG was not kept as its bytes: %v", err)
	}
	st, _ := fileOp(t, f, ctx, map[string]any{"op": "stat", "path": img})
	if asInt(st["page"]) != 2 {
		t.Errorf("stat of the picture = %v, want page 2", st)
	}

	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"]})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if asInt(out["image_chunks"]) != 1 || asInt(out["skipped"]) != 0 {
		t.Fatalf("import = %v", out)
	}
	docID := asStr(out["document_id"])
	c, _, asset := importedChunk(t, d, ctx, docID, "Image 1")
	if c["type"] != "image" || asset == nil || asInt(asset["size"]) != len(pic) || asset["media_type"] != "image/jpeg" {
		t.Fatalf("the picture is not an image chunk: %v %v", c, asset)
	}
	fields := chunkFields(t, d, ctx, docID, "Image 1")
	if asInt(fields["page"]) != 2 || fields["kind"] != "image" {
		t.Errorf("image chunk fields = %v, want page 2", fields)
	}
	if _, ok := fields["slide"]; ok {
		t.Errorf("a PDF picture has a slide: %v", fields)
	}
	if _, body, _ := importedChunk(t, d, ctx, docID, "Page 1"); !strings.Contains(body, "затвердити бюджет") {
		t.Errorf("page 1 body = %q", body)
	}
}

// A picture's page is taken from its capture record only while the file is
// still the bytes captured: one rewritten since says nothing.
func TestFileImport_PageFromRecordOnlyWhileBytesMatch(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "p.pdf"}, boardMinutesPDF(t, testJPEG(t)))
	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	r, _ := inboxRun(ctx)
	other := testJPEGOf(t, 10)
	if err := os.WriteFile(inboxFile(t, r, "volume:inbox:extracted/p/img-1.jpg"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"]})
	if res.IsError || asInt(out["image_chunks"]) != 1 {
		t.Fatalf("import = %v %s", out, res.Text)
	}
	if fields := chunkFields(t, d, ctx, asStr(out["document_id"]), "Image 1"); fields["page"] != nil {
		t.Errorf("a rewritten picture kept its page: %v", fields)
	}
}

// inboxRun is the run tree's ephemeral volume set, for reading inbox files.
func inboxRun(ctx context.Context) (*tools.EphemeralVolumeSet, bool) {
	s := tools.EphemeralVolumes(ctx)
	return s, s != nil
}

// TestFileExtract_PagesReadsOnlyThosePages — pages reads only the PDF pages
// named; the result says which were read, and the kept Markdown holds no
// other page.
func TestFileExtract_PagesReadsOnlyThosePages(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "Протокол.pdf"}, boardMinutesPDF(t, testJPEG(t)))
	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref, "pages": "2"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if ex["pages_read"] != "2" || asInt(ex["pages"]) != 2 {
		t.Fatalf("extract = %v", ex)
	}
	excerpt, _ := ex["excerpt"].(string)
	if strings.Contains(excerpt, "бюджет") || !strings.Contains(excerpt, "Додаток") {
		t.Errorf("excerpt = %q, want page 2 alone", excerpt)
	}
}

// TestFileExtract_MaxCharsStopsTheExtraction — max_chars bounds what is
// extracted and kept, not only what is returned: the kept Markdown is cut at
// the limit, the converter stops (page 2 is never read), and truncated_at
// says so.
func TestFileExtract_MaxCharsStopsTheExtraction(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "Протокол.pdf"}, boardMinutesPDF(t, testJPEG(t)))
	ex, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref, "max_chars": 40})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if asInt(ex["truncated_at"]) != 40 {
		t.Fatalf("truncated_at = %v in %v", ex["truncated_at"], ex)
	}
	excerpt, _ := ex["excerpt"].(string)
	if strings.Contains(excerpt, "Додаток") {
		t.Errorf("page 2 was read: %q", excerpt)
	}
	if notes, _ := json.Marshal(ex["notes"]); !strings.Contains(string(notes), "as max_chars asked") {
		t.Errorf("notes = %v", ex["notes"])
	}
	// Without the options the same file is read whole, as before.
	ex, res = fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
	if res.IsError || ex["truncated_at"] != nil || ex["pages_read"] != nil {
		t.Fatalf("plain extract = %v (%s)", ex, res.Text)
	}
}

// TestFileExtract_PagesIsChecked — a malformed or oversized pages argument
// is refused before anything is read.
func TestFileExtract_PagesIsChecked(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "Протокол.pdf"}, boardMinutesPDF(t, testJPEG(t)))
	for _, spec := range []string{"0", "3-1", "a", "1,,2", "1-5000"} {
		if _, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref, "pages": spec}); !res.IsError || !strings.Contains(res.Text, "pages") {
			t.Errorf("pages %q: %s", spec, res.Text)
		}
	}
}

func TestParsePageSpec_SortsAndDeduplicates(t *testing.T) {
	got, err := parsePageSpec(" 7, 1-3 ,2")
	if err != nil || len(got) != 4 || got[0] != 1 || got[3] != 7 {
		t.Fatalf("parsePageSpec = %v, %v", got, err)
	}
}
