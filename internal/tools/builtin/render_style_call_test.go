package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// The call's photo, a picture in the run's inbox, goes beside the title as
// the style places it (RFC HB D4); a file that is not a PNG or JPEG, or one
// outside the inbox, is refused.
func TestFileRender_PlacesTheCallsPhoto(t *testing.T) {
	f, ctx, docs := noDocsRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "portrait.png", Origin: "mcp:jobs/getUploadedFile"}, testPNG(t))
	page, _, res := renderHTML(t, f, ctx, map[string]any{"path": "volume:docs:cv.md", "photo": kept.Record.Ref,
		"style": map[string]any{"loomcycle_style": 1, "photo": map[string]any{"place": "left", "width_mm": 25}}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(page, `<img class="photo photo-left" src="data:image/png`) || !strings.Contains(page, "img.photo{width:25mm") {
		t.Errorf("page:\n%s", page)
	}

	// No style at all: the photo goes right, at the default width.
	if page, _, res := renderHTML(t, f, ctx, map[string]any{"path": "volume:docs:cv.md", "photo": kept.Record.Ref}); res.IsError || !strings.Contains(page, `class="photo photo-right"`) {
		t.Errorf("photo without a style: %s", res.Text)
	}

	text := inbox.Capture(ctx, inbox.Item{Name: "notes.txt", Origin: "mcp:x/y"}, []byte("not a picture"))
	if err := os.WriteFile(filepath.Join(docs, "p.png"), testPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ ref, want string }{
		"not a picture": {text.Record.Ref, "not a PNG or JPEG"},
		"outside inbox": {"volume:docs:p.png", "in the run's inbox"},
		"missing":       {"volume:inbox:documents/none.png", "photo:"},
		"not a ref":     {"portrait.png", "in the run's inbox"},
	} {
		if _, _, res := renderHTML(t, f, ctx, map[string]any{"path": "volume:docs:cv.md", "photo": c.ref}); !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: %s, want %q", name, res.Text, c.want)
		}
	}
}

// A document render takes the document's first picture before its first
// heading as the photo when the style places one, and draws it once.
func TestFileRender_TakesTheDocumentsFirstPictureAsThePhoto(t *testing.T) {
	f, ctx, d := styleRun(t)
	md := "# Ann Lee\n\n![portrait](data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG(t)) + ")\n\nEngineer.\n\n## Experience\n\nAcme.\n"
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md})
	_, r := docExec(t, d, ctx, string(b))
	if r.IsError {
		t.Fatal(r.Text)
	}
	var doc map[string]any
	_ = json.Unmarshal([]byte(r.Text), &doc)
	page, _, res := renderHTML(t, f, ctx, map[string]any{"document": doc["document_id"],
		"style": map[string]any{"loomcycle_style": 1, "photo": map[string]any{"place": "right"}}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if strings.Count(page, "<img") != 1 || !strings.Contains(page, `class="photo photo-right"`) {
		t.Errorf("page:\n%s", page)
	}
}
