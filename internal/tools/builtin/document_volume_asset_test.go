package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// testPNG is a real 4x4 PNG.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{G: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// volumeDocCtx is the document fixture's run, holding toolset and bound to
// a volume "docs" (with logo.png and notes.txt) and a volume "other" it
// holds no binding for. It returns the docs root and the root of a sibling
// directory outside every volume.
func volumeDocCtx(t *testing.T, ctx context.Context, toolset []string) (context.Context, string, string) {
	t.Helper()
	parent := t.TempDir()
	docs := filepath.Join(parent, "docs")
	outside := filepath.Join(parent, "outside")
	for _, d := range []string{docs, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pic := testPNG(t)
	for p, b := range map[string][]byte{
		filepath.Join(docs, "logo.png"):      pic,
		filepath.Join(docs, "notes.txt"):     []byte("just text, not an image"),
		filepath.Join(outside, "secret.png"): pic,
	} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx = tools.WithRunID(ctx, "run-doc-1")
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.Tools = toolset
		p.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{
			{Name: "docs", Root: docs, Default: true},
		}}
	})
	return ctx, docs, outside
}

// importedChunk returns the chunk titled title in the imported document,
// with its body, and the chunk's asset when it has one.
func importedChunk(t *testing.T, d *Document, ctx context.Context, docID, title string) (chunk map[string]any, body string, asset map[string]any) {
	t.Helper()
	q, _ := json.Marshal(map[string]any{"op": "query_chunks", "scope": "user", "document_id": docID})
	out, r := docExec(t, d, ctx, string(q))
	if r.IsError {
		t.Fatalf("query_chunks: %s", r.Text)
	}
	for _, c := range out["chunks"].([]any) {
		m := c.(map[string]any)
		if m["title"] != title {
			continue
		}
		g, _ := json.Marshal(map[string]any{"op": "get_chunk", "scope": "user", "id": m["id"]})
		got, gr := docExec(t, d, ctx, string(g))
		if gr.IsError {
			t.Fatalf("get_chunk: %s", gr.Text)
		}
		a, _ := json.Marshal(map[string]any{"op": "get_asset", "scope": "user", "id": m["id"]})
		as, ar := docExec(t, d, ctx, string(a))
		if ar.IsError {
			as = nil
		}
		return m, asStr(got["body"]), as
	}
	t.Fatalf("no chunk titled %q", title)
	return nil, "", nil
}

func importMarkdown(t *testing.T, d *Document, ctx context.Context, md string) map[string]any {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md})
	out, r := docExec(t, d, ctx, string(in))
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	return out
}

// A chunk whose whole body is ![alt](volume:...) becomes an image chunk with
// the file's bytes, read by the runtime: the bytes never pass as base64.
func TestImport_VolumeImageRefBecomesImageChunk(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx, _, _ := volumeDocCtx(t, base, []string{"Document", "Read"})
	out := importMarkdown(t, d, ctx, "# Протокол\n\nТекст.\n\n## Логотип\n\n![Логотип правління](volume:docs:logo.png)\n\n## Типовий том\n\n![](volume:logo.png)\n")
	for _, title := range []string{"Логотип", "Типовий том"} {
		c, body, asset := importedChunk(t, d, ctx, asStr(out["document_id"]), title)
		if c["type"] != "image" || asset == nil || asset["media_type"] != "image/png" || asInt(asset["size"]) != len(testPNG(t)) {
			t.Errorf("%s: chunk %v, asset %v; want an image chunk holding the file", title, c, asset)
		}
		if title == "Логотип" && body != "Логотип правління" {
			t.Errorf("caption = %q, want the alt text", body)
		}
	}
	if asInt(out["image_chunks"]) != 2 || asInt(out["skipped"]) != 0 {
		t.Errorf("import result = %v", out)
	}
}

func TestImport_OversizedImageStaysLink(t *testing.T) {
	d, base, _ := documentFixture(t)
	d.MaxAssetBytes = 16 // the test PNG is bigger
	ctx, _, _ := volumeDocCtx(t, base, []string{"Document", "Read"})
	out := importMarkdown(t, d, ctx, "# Doc\n\n## Big\n\n![велике](volume:docs:logo.png)\n\n## Text\n\n![не зображення](volume:docs:notes.txt)\n\n## Gone\n\n![](volume:docs:missing.png)\n")
	for title, link := range map[string]string{
		"Big":  "[велике](volume:docs:logo.png)",
		"Text": "[не зображення](volume:docs:notes.txt)",
		"Gone": "[](volume:docs:missing.png)",
	} {
		c, body, asset := importedChunk(t, d, ctx, asStr(out["document_id"]), title)
		if c["type"] == "image" || asset != nil || body != link {
			t.Errorf("%s: chunk %v body %q asset %v; want a plain link", title, c, body, asset)
		}
	}
	if asInt(out["skipped"]) != 3 || asInt(out["image_chunks"]) != 0 {
		t.Errorf("import result = %v", out)
	}
}

// import_md reads a volume with the run's own authority: Read must be held,
// the volume bound, and the path inside it. Anything else stays a link and
// no byte of the file is stored.
func TestImport_VolumeRefNeedsTheRunsOwnRead(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		toolset   []string
	}{
		{"no Read tool", "volume:docs:logo.png", []string{"Document"}},
		{"unbound volume", "volume:other:logo.png", []string{"Document", "Read"}},
		{"escapes the volume", "volume:docs:../outside/secret.png", []string{"Document", "Read"}},
		{"host path", "volume:docs:/etc/hosts", []string{"Document", "Read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			ctx, _, _ := volumeDocCtx(t, base, tc.toolset)
			out := importMarkdown(t, d, ctx, "# Doc\n\n## Pic\n\n![x]("+tc.ref+")\n")
			c, body, asset := importedChunk(t, d, ctx, asStr(out["document_id"]), "Pic")
			if c["type"] == "image" || asset != nil || body != "[x]("+tc.ref+")" {
				t.Errorf("chunk %v body %q asset %v: the file was read", c, body, asset)
			}
			if asInt(out["skipped"]) != 1 {
				t.Errorf("import result = %v", out)
			}
		})
	}
}

// A Read hook that denies the path refuses the read as it would refuse Read.
func TestImport_VolumeRefJudgedByReadHooks(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx, _, _ := volumeDocCtx(t, base, []string{"Document", "Read"})
	ctx = tools.WithHookPreCheck(ctx, func(_ context.Context, tool string, input json.RawMessage) (tools.Result, bool) {
		if tool == "Read" {
			return tools.Result{Text: "no reading logos", IsError: true}, false
		}
		return tools.Result{}, true
	})
	out := importMarkdown(t, d, ctx, "# Doc\n\n## Pic\n\n![x](volume:docs:logo.png)\n")
	if _, _, asset := importedChunk(t, d, ctx, asStr(out["document_id"]), "Pic"); asset != nil || asInt(out["skipped"]) != 1 {
		t.Errorf("a hook-denied read was stored: %v", out)
	}
}

// A volume image is the single-line form only: alt text spanning lines is
// not a reference import_md follows, so a neutraliser that works on lines
// and the parser agree on what is an image.
func TestImport_MultiLineAltTextIsNotAVolumeImage(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx, _, _ := volumeDocCtx(t, base, []string{"Document", "Read"})
	out := importMarkdown(t, d, ctx, "# Doc\n\n## Pic\n\n![a\nb](volume:docs:logo.png)\n")
	if c, _, asset := importedChunk(t, d, ctx, asStr(out["document_id"]), "Pic"); c["type"] == "image" || asset != nil {
		t.Errorf("multi-line alt text was read as an image: %v %v", c, asset)
	}
	if asInt(out["image_chunks"]) != 0 {
		t.Errorf("import result = %v", out)
	}
}
