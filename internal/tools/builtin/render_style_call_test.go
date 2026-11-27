package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// noDocsRun is a File run on a server without Documents (SQL Memory off):
// an inbox and a "docs" volume, and no Document tool behind File.
func noDocsRun(t *testing.T) (*File, context.Context, string) {
	t.Helper()
	_, ctx, docs := fileRun(t, inbox.Service{}, false)
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = []string{"File", "Read"} })
	if err := os.WriteFile(filepath.Join(docs, "cv.md"), []byte("# Ann Lee\n\n## Experience\n\nA paragraph.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &File{}, ctx, docs
}

// A style passed on the call styles a render on a server with no
// Documents at all (RFC HB D5), and the call's page text replaces its
// lines (D3).
func TestFileRender_AStyleOnTheCallNeedsNoDocuments(t *testing.T) {
	f, ctx, _ := noDocsRun(t)
	page, _, res := renderHTML(t, f, ctx, map[string]any{
		"path":      "volume:docs:cv.md",
		"style":     map[string]any{"loomcycle_style": 1, "font": "serif", "footer": "Shared footer"},
		"page_text": map[string]any{"footer": map[string]any{"left": "Ann Lee · {date}", "right": "Page {page} of {pages}"}, "date": "27 Nov 2026"},
	})
	if res.IsError {
		t.Fatal(res.Text)
	}
	for _, want := range []string{`"Noto Serif"`, `<p class="style-footer">Ann Lee · 27 Nov 2026</p>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "Shared footer") || strings.Contains(page, "{page}") {
		t.Errorf("the style's own footer or a placeholder survived:\n%s", page)
	}

	// page_text alone, with no style, still prints.
	page, _, res = renderHTML(t, f, ctx, map[string]any{"path": "volume:docs:cv.md", "page_text": map[string]any{"header": "{title}"}})
	if res.IsError || !strings.Contains(page, `<p class="style-header">Ann Lee</p>`) {
		t.Errorf("page_text without a style: %s\n%s", res.Text, page)
	}
}

// An inline style is held to the same strict keys and ranges as a style
// document, and so is page_text; a refusal names the field.
func TestFileRender_RefusesABadStyleOrPageTextOnTheCall(t *testing.T) {
	f, ctx, _ := noDocsRun(t)
	for name, c := range map[string]struct {
		in   map[string]any
		want string
	}{
		"unknown key":         {map[string]any{"style": map[string]any{"loomcycle_style": 1, "fnt": "serif"}}, "unknown key"},
		"out of range":        {map[string]any{"style": map[string]any{"loomcycle_style": 1, "size": 40}}, "size"},
		"logo inline":         {map[string]any{"style": map[string]any{"loomcycle_style": 1, "logo": "img-1.png"}}, "not keys of a style"},
		"bad placeholder":     {map[string]any{"page_text": map[string]any{"footer": "{name}"}}, "unknown placeholder {name}"},
		"long date":           {map[string]any{"page_text": map[string]any{"date": strings.Repeat("x", 41)}}, "date"},
		"style of wrong type": {map[string]any{"style": 7}, "style must be"},
	} {
		c.in["path"] = "volume:docs:cv.md"
		_, _, res := renderHTML(t, f, ctx, c.in)
		if !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: %s, want %q", name, res.Text, c.want)
		}
	}
}

// A different inline style is a different render, so the render log never
// reuses one for the other; the source names the style's hash.
func TestFileRender_AnInlineStyleIsNamedByItsHash(t *testing.T) {
	f, ctx, _ := noDocsRun(t)
	j := &renderJob{f: f, in: fileInput{}, format: "html"}
	j.in.Style.Inline = []byte(`{"loomcycle_style": 1, "font": "serif"}`)
	if res, ok := j.resolveStyle(ctx, true); !ok {
		t.Fatal(res.Text)
	}
	a := j.style.Source
	j.in.Style.Inline = []byte(`{"loomcycle_style": 1, "font": "sans"}`)
	if _, ok := j.resolveStyle(ctx, true); !ok {
		t.Fatal("second style refused")
	}
	if !strings.HasPrefix(a, "inline:") || a == j.style.Source {
		t.Errorf("sources %q and %q", a, j.style.Source)
	}
}
