package convert

import (
	"bytes"
	"context"
	"errors"
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
