package wire

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
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
