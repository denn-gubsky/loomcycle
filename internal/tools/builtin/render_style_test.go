package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// styleRun is a File run that may also read and write the tenant scope,
// where house styles live (RFC GR P4c).
func styleRun(t *testing.T) (*File, context.Context, *Document) {
	t.Helper()
	f, ctx, d, _ := fileDocRun(t)
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.Memory.AllowedScopes = append(p.Memory.AllowedScopes, "user", "tenant")
		p.SqlMem.AllowedScopes = append(p.SqlMem.AllowedScopes, "user", "tenant")
	})
	return f, ctx, d
}

// putStyle writes a style document at path in the tenant scope: body its
// JSON, with a logo picture when logo is set.
func putStyle(t *testing.T, d *Document, ctx context.Context, path, body string, logo bool) {
	t.Helper()
	md := "# House style\n\n" + body + "\n"
	if logo {
		md += "\n## Logo\n\n![logo](data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG(t)) + ")\n"
	}
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "path": path, "markdown": md})
	if _, r := docExec(t, d, ctx, string(b)); r.IsError {
		t.Fatalf("put style %s: %s", path, r.Text)
	}
}

func renderHTML(t *testing.T, f *File, ctx context.Context, in map[string]any) (string, map[string]any, tools.Result) {
	t.Helper()
	in["op"], in["format"] = "render", "html"
	out, res := fileOp(t, f, ctx, in)
	if res.IsError {
		return "", out, res
	}
	b, err := os.ReadFile(inboxFile(t, tools.EphemeralVolumes(ctx), asStr(out["ref"])))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), out, res
}

const houseStyle = "```json\n{\"loomcycle_style\": 1, \"font\": \"serif\", \"colors\": {\"link\": \"#aa0000\"}, \"footer\": \"Acme internal\"}\n```"

// A render is written in the style the call names, read from the tenant's
// Path tree; without one, in the tenant's default when there is one; with
// none, in the built-in look.
func TestFileRender_WritesTheTenantsHouseStyle(t *testing.T) {
	f, ctx, d := styleRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	putStyle(t, d, ctx, "/render/styles/house", houseStyle, true)

	page, _, res := renderHTML(t, f, ctx, map[string]any{"document": docID, "style": "/render/styles/house"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	for _, want := range []string{`"Noto Serif"`, "a{color:#aa0000}", `<p class="style-footer">Acme internal</p>`, `<img class="logo"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the styled page lacks %q", want)
		}
	}

	if page, _, _ := renderHTML(t, f, ctx, map[string]any{"document": docID}); strings.Contains(page, "style-footer") {
		t.Error("with no default, a render was styled")
	}
	putStyle(t, d, ctx, renderDefaultStyle, "{\"loomcycle_style\": 1, \"header\": \"Default house\"}", false)
	if page, _, _ := renderHTML(t, f, ctx, map[string]any{"document": docID}); !strings.Contains(page, `<p class="style-header">Default house</p>`) {
		t.Error("the tenant default was not used")
	}
	if page, _, _ := renderHTML(t, f, ctx, map[string]any{"document": docID, "style": "none"}); strings.Contains(page, "style-header") {
		t.Error("style none was styled")
	}

	// Markdown is never styled; the report says so.
	out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "md", "style": "/render/styles/house"})
	if res.IsError || !strings.Contains(res.Text, "the style does not apply to Markdown") {
		t.Errorf("markdown with a style: %v %s", out, res.Text)
	}
}

// A named style that is not there, or not readable, is refused with one
// answer; one that is not a style is refused naming the field, the
// default included: a style never falls back.
func TestFileRender_RefusesAStyleItCannotUse(t *testing.T) {
	f, ctx, d := styleRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	if _, _, res := renderHTML(t, f, ctx, map[string]any{"document": docID, "style": "/render/styles/missing"}); !res.IsError || !strings.Contains(res.Text, "no style at /render/styles/missing") {
		t.Errorf("a missing style: %s", res.Text)
	}
	putStyle(t, d, ctx, "/render/styles/bad", "{\"loomcycle_style\": 1, \"colors\": {\"text\": \"red\"}}", false)
	if _, _, res := renderHTML(t, f, ctx, map[string]any{"document": docID, "style": "/render/styles/bad"}); !res.IsError || !strings.Contains(res.Text, "colors.text") {
		t.Errorf("a bad style: %s", res.Text)
	}
	putStyle(t, d, ctx, renderDefaultStyle, "{\"loomcycle_style\": 1, \"font\": \"comic\"}", false)
	if _, _, res := renderHTML(t, f, ctx, map[string]any{"document": docID}); !res.IsError || !strings.Contains(res.Text, "font") {
		t.Errorf("a bad default style fell back: %s", res.Text)
	}
}

// The run's Document hooks are asked about the style read, as about any
// export_md of the run's.
func TestFileRender_DocumentHookSeesTheStyleRead(t *testing.T) {
	f, ctx, d := styleRun(t)
	docID, _ := renderFixtureDoc(t, d, ctx)
	putStyle(t, d, ctx, "/render/styles/house", houseStyle, false)
	hooked := tools.WithHookPreCheck(ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		if name == "Document" && strings.Contains(string(input), `"path":"/render/styles/house"`) {
			return tools.Result{Text: "styles are frozen", IsError: true}, false
		}
		return tools.Result{}, true
	})
	if _, _, res := renderHTML(t, f, hooked, map[string]any{"document": docID, "style": "/render/styles/house"}); !res.IsError || !strings.Contains(res.Text, "styles are frozen") {
		t.Errorf("a hook refusing the style read let the render through: %s", res.Text)
	}
}
