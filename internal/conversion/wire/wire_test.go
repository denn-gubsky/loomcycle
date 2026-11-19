package wire

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/render"
)

// A value past int32 is clamped on the wire, never wrapped into another one.
func TestI32_SaturatesInsteadOfWrapping(t *testing.T) {
	for _, c := range []struct {
		in   int
		want int32
	}{{0, 0}, {42, 42}, {-7, -7}, {math.MaxInt32, math.MaxInt32}, {math.MaxInt32 + 1, math.MaxInt32}, {math.MinInt32 - 1, math.MinInt32}} {
		if got := I32(c.in); got != c.want {
			t.Errorf("I32(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// A failed extraction reads "extract: ..." as the in-process extractor's
// errors do; a failed helper job keeps the helper's own wording, which names
// its setting. A worker with no helper answers the sentinel the callers word.
func TestErrorFromWire_ShapesAFailureByItsKind(t *testing.T) {
	if got := ErrorFromWire(workerpb.JobKind_JOB_KIND_EXTRACT, workerpb.ErrorClass_ERROR_CLASS_FAILED, "bad zip").Error(); got != "extract: bad zip" {
		t.Errorf("extract failure = %q", got)
	}
	if got := ErrorFromWire(workerpb.JobKind_JOB_KIND_AUDIO_ENCODE, workerpb.ErrorClass_ERROR_CLASS_FAILED, "speech.encode: exit 1").Error(); got != "speech.encode: exit 1" {
		t.Errorf("encode failure = %q", got)
	}
	class, _ := ErrorToWire(fmt.Errorf("wrapped: %w", convert.ErrNotConfigured))
	if err := ErrorFromWire(workerpb.JobKind_JOB_KIND_PDF_RENDER, class, ""); !errors.Is(err, convert.ErrNotConfigured) {
		t.Errorf("not configured round trip = %v", err)
	}
}

// A document render needs the write label, which every worker carries: it
// is not render (rasterizing a PDF page), which needs a helper program.
func TestLabelFor_DocumentRenderNeedsWriteNotAHelper(t *testing.T) {
	k := workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER
	if got := LabelFor(k); got != LabelWrite {
		t.Errorf("LabelFor(document_render) = %q, want %q", got, LabelWrite)
	}
	if HelperLabel(LabelWrite) {
		t.Error("write is a helper label; a worker without helpers would never take a render")
	}
	if got := KindName(k); got != "document_render" {
		t.Errorf("KindName = %q, want document_render", got)
	}
	if LabelFor(workerpb.JobKind_JOB_KIND_PDF_RENDER) != LabelRender {
		t.Error("pdf_render no longer needs the render label")
	}
}

// A render's refusals cross the wire as their classes and come back as the
// render package's sentinels, keeping the worker's message.
func TestErrorFromWire_RenderRefusalsRoundTripToTheRenderSentinels(t *testing.T) {
	k := workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER
	for _, c := range []struct {
		sentinel error
		class    workerpb.ErrorClass
	}{
		{render.ErrUnsupported, workerpb.ErrorClass_ERROR_CLASS_UNSUPPORTED},
		{render.ErrBadBundle, workerpb.ErrorClass_ERROR_CLASS_INPUT},
		{render.ErrTooLarge, workerpb.ErrorClass_ERROR_CLASS_REFUSED},
	} {
		sent := fmt.Errorf("%w: detail", c.sentinel)
		class, msg := ErrorToWire(sent)
		if class != c.class {
			t.Errorf("%v: class %v, want %v", c.sentinel, class, c.class)
		}
		back := ErrorFromWire(k, class, msg)
		if !errors.Is(back, c.sentinel) || back.Error() != sent.Error() {
			t.Errorf("%v: came back as %q (%T), want %q matching the sentinel", c.sentinel, back, back, sent)
		}
	}
	// An extraction's unsupported type still answers as the extractor's.
	if err := ErrorFromWire(workerpb.JobKind_JOB_KIND_EXTRACT, workerpb.ErrorClass_ERROR_CLASS_UNSUPPORTED, ""); errors.Is(err, render.ErrUnsupported) {
		t.Errorf("extract unsupported = %v, a render sentinel", err)
	}
}

// A PDF render needs the typesetter's label, a helper label; every other
// document render keeps write, which every worker carries.
func TestLabelForJob_APDFRenderNeedsTheTypesetter(t *testing.T) {
	k := workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER
	if got := LabelForJob(k, "pdf"); got != LabelWritePDF || !HelperLabel(got) {
		t.Errorf("LabelForJob(document_render, pdf) = %q", got)
	}
	for _, f := range []string{"md", "html", "docx", ""} {
		if got := LabelForJob(k, f); got != LabelWrite {
			t.Errorf("LabelForJob(document_render, %q) = %q, want write", f, got)
		}
	}
	for _, f := range []string{"odt", "epub"} {
		if got := LabelForJob(k, f); got != LabelWriteBook || HelperLabel(got) {
			t.Errorf("LabelForJob(document_render, %q) = %q, want %s (not a helper)", f, got, LabelWriteBook)
		}
	}
	if got := LabelForJob(workerpb.JobKind_JOB_KIND_EXTRACT, "pdf"); got != LabelExtract {
		t.Errorf("an extract job's format changed its label: %q", got)
	}
}

// A diagram draw needs the diagram helper's label, a helper label a worker
// carries only with its program (RFC GR P4d).
func TestLabelFor_DiagramDrawNeedsTheRenderer(t *testing.T) {
	if got := LabelForJob(workerpb.JobKind_JOB_KIND_DIAGRAM_DRAW, ""); got != LabelDraw || !HelperLabel(got) {
		t.Errorf("label %q (helper %v), want %q", got, HelperLabel(got), LabelDraw)
	}
	if KindName(workerpb.JobKind_JOB_KIND_DIAGRAM_DRAW) != "diagram_draw" {
		t.Error("KindName")
	}
}

// A book's metadata crosses the worker protocol whole, and what comes back
// is bounded again: a worker cannot hand the core more than an in-process
// extraction would keep (RFC GV D10).
func TestReportFromWire_MetadataRoundTripsAndIsBounded(t *testing.T) {
	in := extract.Report{Format: "epub", Metadata: &extract.BookMetadata{
		Title: "Война и мир", Authors: []string{"Лев Толстой"}, Translators: []string{"A. Maude"},
		Language: "ru", Date: "1869-01-01", Year: 1869, Publisher: "P", Series: "S", SeriesIndex: "2",
		Identifiers: []extract.BookIdentifier{{Scheme: "isbn", Value: "9780000000002"}},
		Subjects:    []string{"Fiction"}, Description: "D", SourceFormat: "epub",
	}}
	got := ReportFromWire(ReportToWire(in))
	if !reflect.DeepEqual(got.Metadata, in.Metadata) {
		t.Errorf("metadata changed on the way:\n got %+v\nwant %+v", got.Metadata, in.Metadata)
	}
	if ReportFromWire(ReportToWire(extract.Report{Format: "pdf"})).Metadata != nil {
		t.Error("a report with no metadata came back with some")
	}

	many := make([]string, 500)
	for i := range many {
		many[i] = fmt.Sprintf("author %d", i)
	}
	w := &workerpb.Report{Metadata: &workerpb.BookMetadata{
		Title: strings.Repeat("x", 100000) + "\x1b[2J", Authors: many, Description: strings.Repeat("d", 100000),
	}}
	m := ReportFromWire(w).Metadata
	if m == nil || utf8.RuneCountInString(m.Title) > extract.MaxMetaString || len(m.Authors) > extract.MaxMetaList ||
		utf8.RuneCountInString(m.Description) > extract.MaxMetaDescription || strings.ContainsRune(m.Title, 0x1b) {
		t.Errorf("a worker's metadata was not bounded: title %d, authors %d, description %d",
			len(m.Title), len(m.Authors), len(m.Description))
	}
}

// TestPageImages_RoundTripAndCannotGoNegative: the ask for page images and
// the report that rendering is unavailable cross the wire; counts a worker
// reads are never negative.
func TestPageImages_RoundTripAndCannotGoNegative(t *testing.T) {
	in := &extract.PageImages{Always: true, MinChars: 40, MaxPages: 7}
	if got := PageImagesFromWire(PageImagesToWire(in)); *got != *in {
		t.Errorf("round trip = %+v, want %+v", *got, *in)
	}
	if PageImagesToWire(nil) != nil || PageImagesFromWire(nil) != nil {
		t.Error("no ask became one")
	}
	if got := PageImagesFromWire(&workerpb.PageImages{MinChars: -5, MaxPages: -1}); got.MinChars != 0 || got.MaxPages != 0 {
		t.Errorf("negative counts read as %+v", *got)
	}
	if r := ReportFromWire(ReportToWire(extract.Report{Format: "djvu", PageImagesUnavailable: true})); !r.PageImagesUnavailable {
		t.Error("page_images_unavailable did not cross the wire")
	}
}
