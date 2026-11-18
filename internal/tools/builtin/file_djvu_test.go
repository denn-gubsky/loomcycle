//go:build unix

package builtin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/convert/djvutest"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
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
func djvuOCRConfig() config.OCRConfig {
	return config.OCRConfig{DjVu: &config.DjVuHelperConfig{
		Text:    &config.HelperCommand{Command: convert.DefaultDjVuTextCommand},
		Inspect: &config.HelperCommand{Command: convert.DefaultDjVuInspectCommand},
	}}
}

// TestOCR_DjVuIsNeverSentAsAnImageUnit: a DjVu is a book, not an image. The
// OCR path refuses it (until its pages can be rendered, RFC GV P4) before
// its "not a PDF, so an image" branch could send the whole file to a vision
// model; an extraction that asks for OCR reads the text layer and says why
// its thin pages were not OCR'd, and a dry run says OCR is not available.
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

	ref := x.keep(t, "scan.djvu", scan)
	out, md, res := x.extract(t, map[string]any{"path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(fmt.Sprint(out["notes"]), "OCR of DjVu pages is not available yet") || fmt.Sprint(intsOf(out["low_yield_pages"])) != "[1 2 3]" {
		t.Errorf("out = %v", out)
	}
	if !strings.Contains(md, "## Page 3") {
		t.Errorf("markdown:\n%s", md)
	}
	if _, _, res := x.extract(t, map[string]any{"path": ref, "dry_run": true}); !res.IsError || !strings.Contains(res.Text, "not available yet") {
		t.Errorf("dry run: %s", res.Text)
	}
	if sent := x.engine.sent(); len(sent) != 0 {
		t.Fatalf("the engine was sent %+v", sent)
	}
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
