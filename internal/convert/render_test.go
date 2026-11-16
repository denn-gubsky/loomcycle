package convert

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/render"
)

// tinyBundle is a one-paragraph document packed as a render bundle.
func tinyBundle(t *testing.T) []byte {
	t.Helper()
	b := render.NewBuilder("Field notes")
	b.Doc.Blocks = b.Body("Hello **world**.")
	d, _ := b.Finish()
	bundle, err := render.EncodeBundle(d, render.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// A bundle renders in process to the file the render package writes, in each
// format it writes.
func TestInProcess_RenderWritesWhatTheRenderPackageWrites(t *testing.T) {
	bundle := tinyBundle(t)
	for _, format := range []string{render.FormatMarkdown, render.FormatHTML, render.FormatDOCX} {
		got, err := (&InProcess{Extractions: extract.NewLimiter(1)}).Render(context.Background(), bundle, format)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		var want bytes.Buffer
		if err := render.RenderBundle(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), format, &want); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Errorf("%s: the in-process render differs from the render package's (%d vs %d bytes)", format, len(got), want.Len())
		}
		// A DOCX is compressed; the text formats show the document as text.
		if format != render.FormatDOCX && (!strings.Contains(string(got), "Field notes") || !strings.Contains(string(got), "world")) {
			t.Errorf("%s: the text is not in the file:\n%s", format, got)
		}
	}
}

// A format the render package does not write, and a bundle it did not write,
// are refused as its own sentinels.
func TestInProcess_RenderRefusalsAreTheRenderSentinels(t *testing.T) {
	p := &InProcess{}
	if _, err := p.Render(context.Background(), tinyBundle(t), "docx-not-yet"); !errors.Is(err, render.ErrUnsupported) {
		t.Errorf("unknown format: %v, want render.ErrUnsupported", err)
	}
	if _, err := p.Render(context.Background(), []byte("not a zip"), render.FormatHTML); !errors.Is(err, render.ErrBadBundle) {
		t.Errorf("malformed bundle: %v, want render.ErrBadBundle", err)
	}
}

// A render waits for an extraction slot like the jobs it shares the budget
// with, and is answered busy when none frees.
func TestInProcess_RenderWaitsForAnExtractionSlot(t *testing.T) {
	lim := extract.NewLimiter(1)
	release, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p := &InProcess{Extractions: lim, SlotWait: 20 * time.Millisecond}
	if _, err := p.Render(context.Background(), tinyBundle(t), render.FormatMarkdown); !errors.Is(err, ErrBusy) {
		t.Errorf("render with every slot taken: %v, want ErrBusy", err)
	}
}

// A PDF render with no helper is not configured; with a helper whose program
// is missing, it is the no-helper answer the File tool words for PDF.
func TestInProcess_PDFNeedsItsHelper(t *testing.T) {
	root := t.TempDir()
	if _, err := (&InProcess{Scratch: scratchIn(root)}).Render(context.Background(), tinyBundle(t), render.FormatPDF); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no helper: %v, want ErrNotConfigured", err)
	}
	p := &InProcess{Scratch: scratchIn(root), WritePDF: &Helper{Key: PDFHelperKey, Command: "loomcycle-no-such-typesetter {in} {out}"}}
	if _, err := p.Render(context.Background(), tinyBundle(t), render.FormatPDF); !errors.Is(err, ErrNoHelper) {
		t.Errorf("missing program: %v, want ErrNoHelper", err)
	}
	assertEmpty(t, root)
}

// What the typesetter hands back is checked: a failure carries its own
// error line, and a file that is not a PDF is refused, never returned.
func TestInProcess_PDFOutputIsChecked(t *testing.T) {
	root := t.TempDir()
	run := func(cmd string) error {
		p := &InProcess{Scratch: scratchIn(root), WritePDF: &Helper{Key: PDFHelperKey, Command: cmd, Timeout: 10 * time.Second}}
		_, err := p.Render(context.Background(), tinyBundle(t), render.FormatPDF)
		return err
	}
	if err := run(`sh -c 'echo "warning: a font" >&2; echo "error: unknown variable" >&2; exit 1' {in}`); err == nil || !strings.Contains(err.Error(), "error: unknown variable") {
		t.Errorf("a failing typesetter: %v", err)
	}
	if err := run("cp {in} {out}"); err == nil || !strings.Contains(err.Error(), "not a readable PDF") {
		t.Errorf("a typesetter that wrote its source back: %v", err)
	}
	if err := run("true {in} {out}"); err == nil || !strings.Contains(err.Error(), "wrote no PDF") {
		t.Errorf("a typesetter that wrote nothing: %v", err)
	}
	assertEmpty(t, root)
}

// With Typst installed, the default command typesets a bundle into a PDF
// whose text the extractor reads back. Skipped where typst is missing; the
// CI job that installs it sets LOOMCYCLE_REQUIRE_TYPST.
func TestInProcess_PDFTypesetsWithTypst(t *testing.T) {
	if _, err := exec.LookPath("typst"); err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	root := t.TempDir()
	p := &InProcess{Scratch: scratchIn(root), WritePDF: &Helper{Key: PDFHelperKey, Command: DefaultPDFCommand, Timeout: time.Minute}}
	pdf, err := p.Render(context.Background(), tinyBundle(t), render.FormatPDF)
	if err != nil {
		t.Fatal(err)
	}
	var sink mdSink
	if _, err := (extract.PDF{}).Extract(context.Background(), bytes.NewReader(pdf), int64(len(pdf)), &sink, extract.Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.b.String(), "Field notes") || !strings.Contains(sink.b.String(), "world") {
		t.Errorf("the PDF's text: %s", sink.b.String())
	}
	assertEmpty(t, root)
}
