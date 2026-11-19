//go:build unix

package builtin

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/convert/djvutest"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/vision"
)

func djvuFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "djvu", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// djvuOCRConfig is an OCR block whose DjVu helpers are the defaults, which
// the stand-ins of djvutest answer for.
func djvuOCRConfig() config.OCRConfig { return withDjVu(config.OCRConfig{}) }

func withDjVu(op config.OCRConfig) config.OCRConfig {
	op.DjVu = &config.DjVuHelperConfig{
		Text:      &config.HelperCommand{Command: convert.DefaultDjVuTextCommand},
		Inspect:   &config.HelperCommand{Command: convert.DefaultDjVuInspectCommand},
		Rasterize: &config.HelperCommand{Command: convert.DefaultDjVuRasterizeCommand},
	}
	return op
}

// TestOCR_DjVuIsNeverSentAsAnImageUnit: a DjVu is a book, not an image. The
// OCR path refuses the file as a unit before its "not a PDF, so an image"
// branch could send it to a vision model, and an extraction that asks for
// OCR sends only one-page PNGs rendered from it, never the file.
func TestOCR_DjVuIsNeverSentAsAnImageUnit(t *testing.T) {
	djvutest.Install(t)
	x := ocrRun(t, djvuOCRConfig(), agentOCR("always"))
	scan := djvuFile(t, "scan.djvu")
	plan := transcribe.Plan{Engine: "vision", Provider: "anthropic", Model: "claude-x", MaxUnitBytes: 16 << 20}

	if _, _, err := x.f.ocrUnitFor(x.ctx, plan, bytes.NewReader(scan), int64(len(scan)), extract.DjVuMediaType, []int{1}, "uk"); !errors.Is(err, errDjVuOCR) {
		t.Errorf("ocrUnitFor: %v, want errDjVuOCR", err)
	}
	c, _, _ := x.f.ocrRequest(x.ctx, fileInput{})
	if p := x.f.runOCR(x.ctx, c, plan, bytes.NewReader(scan), int64(len(scan)), extract.DjVuMediaType, []int{1, 2, 3}); !errors.Is(p.err, errDjVuOCR) || len(p.done) != 0 {
		t.Errorf("runOCR: err %v, done %v", p.err, p.done)
	}
	if n := len(x.engine.sent()); n != 0 {
		t.Fatalf("the engine was sent %d units", n)
	}

	ref := x.keep(t, "scan.djvu", scan)
	out, md, res := x.extract(t, map[string]any{"path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	sent := x.engine.sent()
	if len(sent) != 3 {
		t.Fatalf("%d units sent, want one per page", len(sent))
	}
	for i, u := range sent {
		if u.mediaType != "image/png" || u.pages != 1 || bytes.Equal(u.data, scan) {
			t.Fatalf("unit %d: %s, %d pages, the file itself: %v", i+1, u.mediaType, u.pages, bytes.Equal(u.data, scan))
		}
		if _, err := png.DecodeConfig(bytes.NewReader(u.data)); err != nil {
			t.Fatalf("unit %d is not a PNG: %v", i+1, err)
		}
	}
	if !reflect.DeepEqual(x.engine.planTypes, []string{"image/png"}) {
		t.Errorf("the engine planned for %q, want image/png", x.engine.planTypes)
	}
	if !strings.Contains(md, "## Page 3\n\nocr call 3 page 1") || fmt.Sprint(intsOf(out["low_yield_pages"])) != "[1 2 3]" {
		t.Errorf("out %v\n%s", out, md)
	}
}

// TestOCR_DjVuAutoReadsOnlyThinPages: in auto mode only the page without a
// text layer is rendered and read; the others keep their text, and the pass
// is charged, counted and reported as a PDF's is.
func TestOCR_DjVuAutoReadsOnlyThinPages(t *testing.T) {
	fake := djvutest.Install(t)
	x := ocrRun(t, djvuOCRConfig(), agentOCR("auto"))
	ref := x.keep(t, "book.djvu", djvuFile(t, "book.djvu"))
	out, md, res := x.extract(t, map[string]any{"path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := renderedPages(fake.Calls(t)); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("ddjvu rendered pages %v, want [2]", got)
	}
	for _, want := range []string{"Text of page 1", "## Page 2\n\nocr call 1 page 1", "Text of page 3", "<!-- loom: ocr engine=vision"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	o, _ := out["ocr"].(map[string]any)
	if fmt.Sprint(intsOf(o["pages"])) != "[2]" || asInt(o["pages_sent"]) != 1 || o["status"] != "ok" {
		t.Errorf("ocr = %v", out["ocr"])
	}
	if ev := x.transcriptionEvents(); len(ev) != 1 || ev[0].Pages != 1 || ev[0].Status != "ok" {
		t.Errorf("events = %+v", ev)
	}
	if n := len(x.ledger.all()); n != 1 {
		t.Errorf("%d ledger rows, want 1", n)
	}
	// The pages reserved for the book and not rendered went back.
	c, _, _ := x.f.ocrRequest(x.ctx, fileInput{})
	run, tree, _ := x.f.ocrBudgetsFor(x.ctx, c.caps)
	if run.Pages() != 1 || tree.Pages() != 1 {
		t.Errorf("the run holds %d pages and its tree %d, want 1 each", run.Pages(), tree.Pages())
	}
}

// TestOCR_DjVuSpliceKeepsTheOutlineHeadings: a page an OCR transcript
// replaces keeps the outline headings that precede its own heading.
func TestOCR_DjVuSpliceKeepsTheOutlineHeadings(t *testing.T) {
	djvutest.Install(t)
	x := ocrRun(t, djvuOCRConfig(), agentOCR("always"))
	ref := x.keep(t, "book.djvu", djvuFile(t, "book.djvu"))
	_, md, res := x.extract(t, map[string]any{"path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(md, "## Chapter From The Stand-in\n\n## Page 1\n\nocr call 1 page 1") || strings.Contains(md, "Text of page 1") {
		t.Errorf("page 1 lost its outline heading or kept its text layer:\n%s", md)
	}

	// DjVuLibre's own outline: two headings before page 3.
	golden := string(djvuFile(t, "book.djvu.md"))
	starts := map[int]int{1: strings.Index(golden, "\n\n## Chapter One"), 2: strings.Index(golden, "\n\n## Page 2"), 3: strings.Index(golden, "\n\n## Chapter Two")}
	p := &ocrPass{text: map[int]string{2: "two", 3: "three"}, done: []int{2, 3}}
	got := spliceOCR(golden, starts, p)
	for _, want := range []string{"## Chapter One\n\n## Page 1\n\nFirst page", "## Page 2\n\ntwo\n\n## Chapter Two\n\n### Section 2.1\n\n## Page 3\n\nthree"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "Третья") {
		t.Errorf("page 3's text layer survived:\n%s", got)
	}
}

// pages limits what is read, and so what is rendered.
func TestOCR_DjVuPagesLimitsRendering(t *testing.T) {
	fake := djvutest.Install(t)
	x := ocrRun(t, djvuOCRConfig(), agentOCR("always"))
	ref := x.keep(t, "scan.djvu", djvuFile(t, "scan.djvu"))
	if _, _, res := x.extract(t, map[string]any{"path": ref, "pages": "2-3"}); res.IsError {
		t.Fatal(res.Text)
	}
	if got := renderedPages(fake.Calls(t)); !reflect.DeepEqual(got, []string{"2", "3"}) || len(x.engine.sent()) != 2 {
		t.Fatalf("rendered %v, sent %d", got, len(x.engine.sent()))
	}
}

// TestOCR_DjVuPageCapStopsRendering: the pages a run may OCR are reserved
// before the book is read, so the helper renders no more than that; the
// pass then reports the cap as a PDF's does, with the Markdown kept.
func TestOCR_DjVuPageCapStopsRendering(t *testing.T) {
	fake := djvutest.Install(t)
	x := ocrRun(t, withDjVu(config.OCRConfig{MaxPagesPerRun: 2}), agentOCR("always"))
	ref := x.keep(t, "scan.djvu", djvuFile(t, "scan.djvu"))
	_, _, res := x.extract(t, map[string]any{"path": ref})
	if !res.IsError || !strings.Contains(res.Text, "max_pages_per_run") || !strings.Contains(res.Text, `"ref"`) {
		t.Fatalf("res = %s", res.Text)
	}
	if got := renderedPages(fake.Calls(t)); !reflect.DeepEqual(got, []string{"1", "2"}) || len(x.engine.sent()) != 2 {
		t.Fatalf("rendered %v, sent %d; want two", got, len(x.engine.sent()))
	}
	if ev := x.transcriptionEvents(); len(ev) != 1 || ev[0].Status != "partial" || ev[0].Pages != 2 {
		t.Errorf("events = %+v", ev)
	}
}

// A dry run renders nothing, calls no engine and reserves nothing; it
// counts a unit per page.
func TestOCR_DjVuDryRunRendersNothing(t *testing.T) {
	fake := djvutest.Install(t)
	x := ocrRun(t, withDjVu(config.OCRConfig{MaxPagesPerRun: 3}), agentOCR("auto"))
	ref := x.keep(t, "scan.djvu", djvuFile(t, "scan.djvu"))
	out, _, res := x.extract(t, map[string]any{"path": ref, "dry_run": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["dry_run"] != true || asInt(out["pages"]) != 3 || asInt(out["units"]) != 3 || out["note"] != nil {
		t.Fatalf("dry run = %v", out)
	}
	if got := renderedPages(fake.Calls(t)); len(got) != 0 || len(x.engine.sent()) != 0 {
		t.Fatalf("a dry run rendered %v and sent %d units", got, len(x.engine.sent()))
	}
	if _, _, res := x.extract(t, map[string]any{"path": ref}); res.IsError {
		t.Fatalf("the dry run used up the budget: %s", res.Text)
	}
}

// RFC GV D9: the agent's transcription.ocr_model reads a DjVu's pages, as
// it reads a PDF's.
func TestOCR_DjVuUsesTheAgentsOCRModel(t *testing.T) {
	djvutest.Install(t)
	x := ocrRun(t, djvuOCRConfig(), &config.AgentTranscription{OCR: "auto", OCRModel: "local-vision"})
	ref := x.keep(t, "scan.djvu", djvuFile(t, "scan.djvu"))
	out, _, res := x.extract(t, map[string]any{"path": ref, "dry_run": true})
	if res.IsError || out["model"] != "local-vision" || out["provider"] != "ollama-local" {
		t.Fatalf("dry run: %v %s", out, res.Text)
	}
	out, _, res = x.extract(t, map[string]any{"path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if o, _ := out["ocr"].(map[string]any); o["model"] != "local-vision" {
		t.Errorf("ocr = %v", out["ocr"])
	}
	if !reflect.DeepEqual(x.engine.models, []string{"local-vision", "local-vision"}) {
		t.Errorf("engine asked for %q", x.engine.models)
	}
}

// Without a page rasterizer a DjVu asked for OCR is read for its text
// layer, and the result and the dry run say page OCR is not available
// here: an answer, not a failure.
func TestOCR_DjVuWithoutARasterizerSaysPageOCRIsUnavailable(t *testing.T) {
	djvutest.Install(t)
	op := djvuOCRConfig()
	op.DjVu.Rasterize = nil
	x := ocrRun(t, op, agentOCR("always"))
	ref := x.keep(t, "book.djvu", djvuFile(t, "book.djvu"))
	out, md, res := x.extract(t, map[string]any{"path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(fmt.Sprint(out["notes"]), "page OCR is not available here") || out["ocr"] != nil || !strings.Contains(md, "Text of page 3") {
		t.Errorf("out = %v", out)
	}
	out, _, res = x.extract(t, map[string]any{"path": ref, "dry_run": true})
	if res.IsError || asInt(out["pages"]) != 0 || !strings.Contains(fmt.Sprint(out["note"]), "page OCR is not available here") {
		t.Errorf("dry run: %v %s", out, res.Text)
	}
	if len(x.engine.sent()) != 0 {
		t.Fatal("the engine was called")
	}
}

// The page a DjVu is rendered at is the edge the vision engine sends an
// image at whole, and a rendered page fits one request.
func TestDjVuRenderEdge_IsTheVisionEdge(t *testing.T) {
	if extract.DjVuRenderEdge != vision.MaxImageEdge || extract.MaxDjVuPagePNG != vision.MaxUnitBytes {
		t.Errorf("render edge %d, page PNG %d; vision edge %d, unit %d", extract.DjVuRenderEdge, extract.MaxDjVuPagePNG, vision.MaxImageEdge, vision.MaxUnitBytes)
	}
}

// renderedPages are the pages ddjvu's stand-in was asked for.
func renderedPages(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "ddjvu ") {
			continue
		}
		for _, f := range strings.Fields(c) {
			if n, ok := strings.CutPrefix(f, "-page="); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// With the operator's DjVu helpers the File tool reads a DjVu in process,
// pages included; without them it says how a server reads DjVu.
func TestFileExtract_DjVuIsReadWithTheConfiguredHelpersOnly(t *testing.T) {
	djvutest.Install(t)
	x := ocrRun(t, djvuOCRConfig(), nil)
	ref := x.keep(t, "book.djvu", djvuFile(t, "book.djvu"))
	out, md, res := x.extract(t, map[string]any{"path": ref, "pages": "3"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(md, "Text of page 3") || strings.Contains(md, "Text of page 1") || out["format"] != "djvu" {
		t.Errorf("out %v\n%s", out, md)
	}
	if out["pages_read"] != "3" || strings.Contains(fmt.Sprint(out["notes"]), "pages reads PDF pages") {
		t.Errorf("pages_read %v, notes %v", out["pages_read"], out["notes"])
	}

	bare := ocrRun(t, config.OCRConfig{}, nil)
	ref = bare.keep(t, "book.djvu", djvuFile(t, "book.djvu"))
	_, _, res = bare.extract(t, map[string]any{"path": ref})
	if !res.IsError || !strings.Contains(res.Text, "cannot read DjVu") || res.Error == nil ||
		!strings.Contains(res.Error.Description, "djvulibre-bin") || !strings.Contains(res.Error.Description, "transcription.ocr.djvu.text") {
		t.Errorf("no helpers: %s (%+v)", res.Text, res.Error)
	}
}

// A worker's page images are held to what the core asked: pages it wrote,
// each once, no more than were reserved, none empty or past the bounds.
func TestExtractSink_PageImagesAreHeldToWhatWasAsked(t *testing.T) {
	s := &extractSink{starts: map[int]int{1: 0, 2: 10, 3: 20}, pageImageCap: 2}
	if err := s.PageImage(4, []byte("x")); err == nil {
		t.Error("a page not written was kept")
	}
	if err := s.PageImage(1, nil); err == nil {
		t.Error("an empty image was kept")
	}
	if err := s.PageImage(1, make([]byte, extract.MaxDjVuPagePNG+1)); err == nil {
		t.Error("an image past the per-page bound was kept")
	}
	if err := s.PageImage(1, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.PageImage(1, []byte("b")); err == nil {
		t.Error("a page was kept twice")
	}
	if err := s.PageImage(2, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if err := s.PageImage(3, []byte("d")); err == nil {
		t.Error("more pages than reserved were kept")
	}
	if len(s.pageImages) != 2 || string(s.pageImages[1]) != "a" || s.pageImageBytes != 2 {
		t.Errorf("kept %v (%d bytes)", s.pageImages, s.pageImageBytes)
	}
}
