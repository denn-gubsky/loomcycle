package builtin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// File op=extract structure.infer_headings (RFC GB §6): the extractor's
// heading recovery, its counts in the result, and the import's provenance
// saying where the structure came from.

// fieldNotesHTML is a page whose two section titles are formatting only: a
// 14pt line and a bold line. Invented text.
const fieldNotesHTML = `<!doctype html><html><head><title>Field notes</title></head><body>
<p style="font-size:14pt">Site survey</p>
<p>The survey covered the northern plot over two days and recorded every marker found on the ground.</p>
<p><b>Background</b></p>
<p>The plot was last surveyed some years ago, and most of the old markers were missing or moved.</p>
</body></html>`

// inboxRoot maps an inbox reference to its file on disk.
func inboxRoot(t *testing.T, ctx context.Context) func(ref string) string {
	t.Helper()
	return func(ref string) string { return inboxFile(t, tools.EphemeralVolumes(ctx), ref) }
}

func inferExtract(t *testing.T, f *File, ctx context.Context, structure map[string]any) map[string]any {
	t.Helper()
	kept := inbox.Capture(ctx, inbox.Item{Name: "field-notes.html", MediaType: "text/html", Origin: "mcp:notes/get"}, []byte(fieldNotesHTML))
	in := map[string]any{"op": "extract", "path": kept.Record.Ref}
	if structure != nil {
		in["structure"] = structure
	}
	out, res := fileOp(t, f, ctx, in)
	if res.IsError {
		t.Fatal(res.Text)
	}
	return out
}

func TestFileExtract_InferHeadingsPromotesAndCounts(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	r := inboxRoot(t, ctx)
	out := inferExtract(t, f, ctx, map[string]any{"infer_headings": true})
	if out["inferred_headings"] != float64(2) || out["dropped_decorative_images"] != float64(0) {
		t.Errorf("result = %v, want inferred_headings 2 and dropped_decorative_images 0", out)
	}
	md, err := os.ReadFile(r(asStr(out["ref"])))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n## Site survey\n", "\n### Background\n"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestFileExtract_WithoutStructureReportsNoCounts(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	for _, structure := range []map[string]any{nil, {"infer_headings": false}} {
		out := inferExtract(t, f, ctx, structure)
		if _, ok := out["inferred_headings"]; ok {
			t.Errorf("structure %v: result has inferred_headings: %v", structure, out)
		}
		if _, ok := out["dropped_decorative_images"]; ok {
			t.Errorf("structure %v: result has dropped_decorative_images: %v", structure, out)
		}
	}
}

// The option reads DOCX and HTML; any other format is extracted as before,
// and the result says the option was not applied.
func TestFileExtract_InferHeadingsOnOtherFormatsIsANote(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "t.csv", MediaType: "text/csv", Origin: "http"}, []byte("a,b\n1,2\n"))
	out, res := fileOp(t, f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref, "structure": map[string]any{"infer_headings": true}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if _, ok := out["inferred_headings"]; ok {
		t.Errorf("a CSV reports inferred_headings: %v", out)
	}
	if !strings.Contains(strings.ToLower(asStr(out["notes"])+anyString(out["notes"])), "infer_headings") {
		t.Errorf("notes = %v, want one saying infer_headings was not applied", out["notes"])
	}
}

func anyString(v any) string {
	var b strings.Builder
	if l, ok := v.([]any); ok {
		for _, x := range l {
			b.WriteString(asStr(x) + "\n")
		}
	}
	return b.String()
}

func structuredOf(t *testing.T, d *Document, ctx context.Context, docID string) map[string]any {
	t.Helper()
	got, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	p, _ := got["provenance"].(map[string]any)
	s, _ := p["structured"].(map[string]any)
	return s
}

// An import of text whose headings the extraction inferred records that in
// the document's provenance, with the runtime's authority: the record of the
// extracted file says so, while the file's bytes still hash to it.
func TestFileImport_InferredHeadingsRecordedInProvenance(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	ex := inferExtract(t, f, ctx, map[string]any{"infer_headings": true})
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"]})
	if res.IsError {
		t.Fatal(res.Text)
	}
	s := structuredOf(t, d, ctx, asStr(out["document_id"]))
	if s["headings"] != float64(2) || s["by"] != "extract:infer_headings" {
		t.Errorf("provenance structured = %v, want 2 headings by extract:infer_headings", s)
	}
}

func TestFileImport_NoStructuredWithoutInferredHeadings(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	ex := inferExtract(t, f, ctx, nil)
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"]})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if s := structuredOf(t, d, ctx, asStr(out["document_id"])); s != nil {
		t.Errorf("provenance structured = %v, want none", s)
	}
}

// A run that corrects the inferred outline with headings on import (RFC GB
// D4) is recorded as both: the extraction's inference, then this run.
func TestFileImport_HeadingsOverInferredRecordBoth(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	r := inboxRoot(t, ctx)
	ex := inferExtract(t, f, ctx, map[string]any{"infer_headings": true})
	md, err := os.ReadFile(r(asStr(ex["ref"])))
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for i, l := range strings.Split(string(md), "\n") {
		if strings.HasPrefix(l, "The plot was last surveyed") {
			line = i + 1
		}
	}
	if line == 0 {
		t.Fatalf("no body line to mark in:\n%s", md)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"],
		"headings": []any{map[string]any{"line": line, "level": 4, "as": "marker", "title": "History"}}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	s := structuredOf(t, d, ctx, asStr(out["document_id"]))
	if s["headings"] != float64(1) || s["by"] != "extract:infer_headings+"+tools.RunID(ctx) {
		t.Errorf("provenance structured = %v, want 1 heading by extract:infer_headings+%s", s, tools.RunID(ctx))
	}
}

// A file whose bytes no longer match the extraction's record is not
// claimed as inferred: the record does not stand for it.
func TestFileImport_EditedExtractClaimsNoInference(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	r := inboxRoot(t, ctx)
	ex := inferExtract(t, f, ctx, map[string]any{"infer_headings": true})
	p := r(asStr(ex["ref"]))
	md, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(md, []byte("\n\n## Added by hand\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": ex["ref"]})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if s := structuredOf(t, d, ctx, asStr(out["document_id"])); s != nil {
		t.Errorf("provenance structured = %v, want none for an edited file", s)
	}
}
