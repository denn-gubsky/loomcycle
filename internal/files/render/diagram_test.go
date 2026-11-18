package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// diagramDoc holds Mermaid diagrams at the top, in a section and inside a
// list item, the first twice, and a code block that is not one.
func diagramDoc(t *testing.T) *Doc {
	t.Helper()
	b := NewBuilder("Diagrams")
	b.Doc.Anchor = "chunk-root"
	b.Doc.Blocks = b.Body("```mermaid\nflowchart LR\n  A --> B\n```\n\n```go\nfmt.Println(1)\n```")
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "s1", Anchor: "chunk-s1", Title: "More", Level: 2,
		Blocks: b.Body("- item\n\n  ```mermaid\n  sequenceDiagram\n    A->>B: hi\n  ```\n\n```mermaid\nflowchart LR\n  A --> B\n```")}}
	d, _ := b.Finish()
	return d
}

// countKinds counts the Doc's nodes of each kind, nested ones included.
func countKinds(d *Doc) map[string]int {
	got := map[string]int{}
	var nodes func(ns []Node)
	nodes = func(ns []Node) {
		for _, n := range ns {
			got[n.Kind]++
			nodes(n.Children)
		}
	}
	nodes(d.Blocks)
	for _, s := range d.Sections {
		nodes(s.Blocks)
	}
	return got
}

func pngOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.Black)
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// fakeDraw answers every diagram with a 120×80 PNG, recording the sources.
func fakeDraw(calls *[]string) DrawFunc {
	return func(_ context.Context, src string) ([]byte, error) {
		*calls = append(*calls, src)
		return pngOf(120, 80), nil
	}
}

// Each Mermaid diagram becomes a 2× PNG, wherever it is nested; the same
// diagram is drawn once; its source is the picture's alt text. Markdown
// keeps its source.
func TestDrawDiagrams_ReplacesEachDiagramWithItsPicture(t *testing.T) {
	for _, format := range []string{FormatPDF, FormatHTML, FormatODT, FormatEPUB, FormatDOCX} {
		d := diagramDoc(t)
		var calls []string
		var rep Report
		if err := DrawDiagrams(context.Background(), d, format, fakeDraw(&calls), &rep); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 2 {
			t.Errorf("%s: draw calls %q, want two (the repeated diagram once)", format, calls)
		}
		if k := countKinds(d); k[KindDiagram] != 0 || k[KindImage] != 3 || k[KindCode] != 1 {
			t.Errorf("%s: kinds %v, want 3 pictures, no diagram, the Go block kept", format, k)
		}
		if rep.DiagramsDrawn != 3 || rep.DiagramsLeftAsCode != 0 || rep.Images != 2 || len(d.Images) != 2 ||
			d.Images[0].MediaType != "image/png" || d.Images[0].Density != 2 {
			t.Errorf("%s: report %+v, pictures %+v", format, rep, d.Images[0])
		}
		if alt := d.Blocks[0].Alt; alt != "flowchart LR A --> B" {
			t.Errorf("%s: alt = %q, want the source on one line", format, alt)
		}
	}
	d := diagramDoc(t)
	var calls []string
	var rep Report
	_ = DrawDiagrams(context.Background(), d, FormatMarkdown, fakeDraw(&calls), &rep)
	if len(calls) != 0 || countKinds(d)[KindDiagram] != 3 || rep.DiagramsDrawn != 0 {
		t.Errorf("markdown drew %q: %+v", calls, rep)
	}
}

// A diagram that does not draw, or whose picture is not one a file may
// carry, stays code, counted, and the render goes on; with no renderer on
// the server none is tried after the first answer.
func TestDrawDiagrams_LeavesWhatItCannotDrawAsCode(t *testing.T) {
	for name, tc := range map[string]struct {
		draw  DrawFunc
		calls int
		note  string
	}{
		"helper fails": {calls: 2, note: "3 diagrams could not be drawn",
			draw: func(context.Context, string) ([]byte, error) { return nil, errors.New("exit 1") }},
		"no renderer": {calls: 1, note: "no diagram renderer",
			draw: func(context.Context, string) ([]byte, error) { return nil, ErrNoDrawer }},
		"not a PNG": {calls: 2, note: "could not be drawn",
			draw: func(context.Context, string) ([]byte, error) { return []byte(`<svg/>`), nil }},
		"a JPEG": {calls: 2, note: "could not be drawn",
			draw: func(context.Context, string) ([]byte, error) { return []byte("\xff\xd8\xff\xe0 not a png"), nil }},
		"too wide": {calls: 2, note: "could not be drawn",
			draw: func(context.Context, string) ([]byte, error) { return pngOf(MaxDiagramEdge+1, 2), nil }},
		"truncated": {calls: 2, note: "could not be drawn",
			draw: func(context.Context, string) ([]byte, error) { return pngOf(40, 40)[:60], nil }},
	} {
		d := diagramDoc(t)
		var rep Report
		n := 0
		draw := func(ctx context.Context, src string) ([]byte, error) { n++; return tc.draw(ctx, src) }
		if err := DrawDiagrams(context.Background(), d, FormatHTML, draw, &rep); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n != tc.calls || rep.DiagramsDrawn != 0 || rep.DiagramsLeftAsCode != 3 || countKinds(d)[KindDiagram] != 3 || len(d.Images) != 0 {
			t.Errorf("%s: %d calls, report %+v, %d pictures", name, n, rep, len(d.Images))
		}
		if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], tc.note) {
			t.Errorf("%s: notes %q, want %q", name, rep.Notes, tc.note)
		}
	}
}

// manyDiagrams is a Doc of n different diagrams.
func manyDiagrams(n int) *Doc {
	var md strings.Builder
	for i := range n {
		fmt.Fprintf(&md, "```mermaid\nflowchart LR\n  A%d --> B\n```\n\n", i)
	}
	b := NewBuilder("Many")
	b.Doc.Blocks = b.Body(md.String())
	d, _ := b.Finish()
	return d
}

// At most MaxDiagrams diagrams are drawn in one render; a renderer that
// hangs is given up on after maxDiagramTimeouts diagrams, not asked about
// every one; a cancelled render stops drawing and says so.
func TestDrawDiagrams_IsBounded(t *testing.T) {
	var calls []string
	var rep Report
	if err := DrawDiagrams(context.Background(), manyDiagrams(MaxDiagrams+5), FormatHTML, fakeDraw(&calls), &rep); err != nil {
		t.Fatal(err)
	}
	if len(calls) != MaxDiagrams || rep.DiagramsDrawn != MaxDiagrams || rep.DiagramsLeftAsCode != 5 {
		t.Errorf("%d calls, report %+v", len(calls), rep)
	}

	// A renderer that runs out of time (as a hung helper does at
	// DiagramTimeout) is asked about maxDiagramTimeouts diagrams, no more.
	n := 0
	hang := func(context.Context, string) ([]byte, error) {
		n++
		return nil, fmt.Errorf("render.diagram: %w", context.DeadlineExceeded)
	}
	rep = Report{}
	if err := DrawDiagrams(context.Background(), manyDiagrams(10), FormatHTML, hang, &rep); err != nil {
		t.Fatal(err)
	}
	if n != maxDiagramTimeouts || rep.DiagramsLeftAsCode != 10 {
		t.Errorf("a hanging renderer was asked %d times (report %+v), want %d", n, rep, maxDiagramTimeouts)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if err := DrawDiagrams(cctx, diagramDoc(t), FormatHTML, fakeDraw(&calls), &Report{}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled render: %v", err)
	}
}

// drawnDoc is diagramDoc with its diagrams drawn.
func drawnDoc(t *testing.T) *Doc {
	t.Helper()
	d := diagramDoc(t)
	var calls []string
	if err := DrawDiagrams(context.Background(), d, FormatHTML, fakeDraw(&calls), &Report{}); err != nil {
		t.Fatal(err)
	}
	return d
}

// Every format shows a drawn diagram at half its pixel size: 120×80 pixels
// shown as 60×40.
func TestDiagram_EveryFormatShowsThePictureAtHalfSize(t *testing.T) {
	h := string(write(t, drawnDoc(t), FormatHTML, Options{}))
	if !strings.Contains(h, `<img width="60" height="40" src="data:image/png;base64,`) || !strings.Contains(h, `alt="flowchart LR A --&gt; B"`) {
		t.Errorf("html lacks the half-size picture:\n%s", h)
	}
	assertInertHTML(t, h)

	book := docxParts(t, write(t, drawnDoc(t), FormatEPUB, Options{}))
	if p := book["OEBPS/text/title.xhtml"]; !strings.Contains(p, `src="../images/img-1.png" width="60" height="40"`) {
		t.Errorf("the book shows the picture at its pixel size:\n%s", p)
	}

	// Word: 60 shown pixels at 9525 EMU each.
	w := docxParts(t, write(t, drawnDoc(t), FormatDOCX, Options{}))
	if !strings.Contains(w["word/document.xml"], `cx="571500" cy="381000"`) {
		t.Errorf("the Word picture is not 60×40 shown pixels")
	}

	// ODT: 60 px at 96 dpi is 1.5875 cm.
	o := docxParts(t, write(t, drawnDoc(t), FormatODT, Options{}))
	if !strings.Contains(o["content.xml"], `svg:width="1.588cm"`) && !strings.Contains(o["content.xml"], `svg:width="1.587cm"`) {
		t.Errorf("the ODT picture is not 60 shown pixels wide")
	}

	// Typst: 60 px at 0.75 pt each.
	if src := typSource(t, drawnDoc(t), Options{}); !strings.Contains(src, `width: 45.00pt`) {
		t.Errorf("typst shows the picture at its pixel size:\n%s", src)
	}

	// A picture of the document's own (density 0) is shown at its size.
	plain := drawnDoc(t)
	for i := range plain.Images {
		plain.Images[i].Density = 0
	}
	if h := string(write(t, plain, FormatHTML, Options{})); strings.Contains(h, `width="60"`) {
		t.Error("a picture of density 0 was halved")
	}
}

// A worker takes a picture's density only as the core sets it.
func TestDiagram_BundleRefusesAForgedDensity(t *testing.T) {
	d := drawnDoc(t)
	b, err := EncodeBundle(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b))); err != nil || got.Images[0].Density != 2 {
		t.Fatalf("a drawn diagram: %v", err)
	}
	d.Images[0].Density = 1000
	b, _ = EncodeBundle(d, Options{})
	if _, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrBadBundle) {
		t.Errorf("density 1000: %v, want ErrBadBundle", err)
	}
}

// A drawn diagram typesets. Skipped where typst is not installed; CI's
// render-office job requires it.
func TestPDF_TypesetsADrawnDiagram(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	if pdf := compileTypst(t, typst, drawnDoc(t), Options{}); len(pdf) == 0 {
		t.Error("no PDF")
	}
}

// drawnFixture is the full fixture (every block kind, the text the office
// checks look for) with a drawn diagram added, for those checks.
func drawnFixture(t *testing.T) *Doc {
	t.Helper()
	d := fixtureDoc(t)
	name := fmt.Sprintf("img-%d.png", len(d.Images)+1)
	d.Images = append(d.Images, Image{Name: name, MediaType: "image/png", Density: 2, Data: pngOf(120, 80)})
	d.Blocks = append(d.Blocks, Node{Kind: KindImage, Asset: name, Alt: "flowchart LR A --> B"})
	return d
}
