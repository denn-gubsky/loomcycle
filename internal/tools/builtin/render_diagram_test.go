package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// diagramRun is a File run whose diagram renderer copies a fixture PNG to
// {out}.
func diagramRun(t *testing.T) (*File, context.Context, *Document) {
	t.Helper()
	f, ctx, d, _ := fileDocRun(t)
	ctx = withInboxScratch(t, ctx)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pic.png"), testPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	f.diagramCommand = "cp " + filepath.Join(dir, "pic.png") + " {out}"
	return f, ctx, d
}

func diagramDocument(t *testing.T, d *Document, ctx context.Context) string {
	t.Helper()
	md := "# Flow\n\nIntro.\n\n```mermaid\nflowchart LR\n  A --> B\n```\n"
	out, res := docExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":`+jsonString(md)+`}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	return asStr(out["document_id"])
}

func renderedPage(t *testing.T, f *File, ctx context.Context, in map[string]any) (string, map[string]any) {
	t.Helper()
	in["op"] = "render"
	out, res := fileOp(t, f, ctx, in)
	if res.IsError {
		t.Fatalf("render %v: %s", in, res.Text)
	}
	b, err := os.ReadFile(inboxFile(t, tools.EphemeralVolumes(ctx), asStr(out["ref"])))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), out
}

// A document's Mermaid diagram is drawn as a 2× PNG in an HTML page and a
// Word file alike. Markdown keeps its source. The report counts what was
// drawn.
func TestFileRender_DrawsMermaidDiagrams(t *testing.T) {
	f, ctx, d := diagramRun(t)
	docID := diagramDocument(t, d, ctx)

	page, out := renderedPage(t, f, ctx, map[string]any{"document": docID, "format": "html"})
	rep, _ := out["report"].(map[string]any)
	if !strings.Contains(page, `src="data:image/png;base64,`) || strings.Contains(page, `class="diagram"`) || rep["diagrams_drawn"] != float64(1) {
		t.Errorf("html: report %v\n%s", rep, page)
	}

	doc, out := renderedPage(t, f, ctx, map[string]any{"document": docID, "format": "docx"})
	if !strings.Contains(doc, "word/media/img-1.png") {
		t.Errorf("the Word file holds no PNG picture (report %v)", out["report"])
	}

	md, out := renderedPage(t, f, ctx, map[string]any{"document": docID, "format": "md"})
	if !strings.Contains(md, "```mermaid") {
		t.Errorf("markdown lost the diagram's source (report %v):\n%s", out["report"], md)
	}
}

// With no renderer on the server the render still succeeds, its diagram
// shown as code, and the report says why.
func TestFileRender_WithoutARendererDiagramsStayCode(t *testing.T) {
	f, ctx, d := diagramRun(t)
	f.diagramCommand = "loomcycle-no-such-renderer {in} {out}"
	docID := diagramDocument(t, d, ctx)
	page, out := renderedPage(t, f, ctx, map[string]any{"document": docID, "format": "html"})
	rep, _ := out["report"].(map[string]any)
	if !strings.Contains(page, `class="diagram"`) || rep["diagrams_left_as_code"] != float64(1) ||
		!strings.Contains(asStr(rep["notes"].([]any)[0]), "no diagram renderer") {
		t.Errorf("report %v\n%s", rep, page)
	}
}
