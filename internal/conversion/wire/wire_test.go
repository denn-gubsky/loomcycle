package wire

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
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
