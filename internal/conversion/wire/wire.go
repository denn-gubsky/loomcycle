// Package wire holds what the dispatcher and the worker must agree on beyond
// the generated messages: the protocol version, the metadata keys, the chunk
// size, the labels, and the translation of an extraction's report and error to
// and from their wire form.
package wire

import (
	"context"
	"errors"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// ProtocolVersion is the worker protocol this build speaks. A worker whose
// Hello names another is refused (RFC GO D7); a different build of the same
// protocol is admitted with a warning.
const ProtocolVersion = 1

// ChunkSize is the most bytes one Chunk frame carries, so the 4 MiB gRPC
// message default is never the limit.
const ChunkSize = 1 << 20

// MDAuthorization is the metadata key every worker call carries the shared
// token in, as a bearer. A Work stream names its worker in its Ready frame.
const MDAuthorization = "authorization"

// Labels a worker carries: what it can do. P1 workers run extraction and the
// PDF page cut; the helper and image labels arrive with P2.
const (
	LabelExtract = "extract"
	LabelPDF     = "pdf"
)

// LabelFor is the label a job kind needs.
func LabelFor(k workerpb.JobKind) string {
	switch k {
	case workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES:
		return LabelPDF
	}
	return LabelExtract
}

// KindName is a job kind's name in metrics and logs.
func KindName(k workerpb.JobKind) string {
	switch k {
	case workerpb.JobKind_JOB_KIND_EXTRACT:
		return "extract"
	case workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES:
		return "pdf_select_pages"
	}
	return "unknown"
}

// Caps on what the core accepts back from a worker, whose answer is untrusted
// like any other input.
const (
	maxNotes     = 64
	maxNoteBytes = 1024
	maxFormat    = 32
	maxYields    = 100000
)

// ReportToWire is the report a worker sends.
func ReportToWire(r extract.Report) *workerpb.Report {
	w := &workerpb.Report{
		Format: r.Format, Chars: int64(r.Chars), Pages: int32(r.Pages), Sheets: int32(r.Sheets),
		Slides: int32(r.Slides), Tables: int32(r.Tables), Images: int32(r.Images),
		YieldCharsPerPage: r.YieldCharsPerPage, InferredHeadings: int32(r.InferredHeadings),
		DroppedDecorativeImages: int32(r.DroppedDecorativeImages), Notes: r.Notes,
	}
	for _, y := range r.PageYields {
		w.PageYields = append(w.PageYields, &workerpb.PageYield{Page: int32(y.Page), Chars: int32(y.Chars), Unread: y.Unread})
	}
	return w
}

// ReportFromWire is the report the core keeps, cut to its caps.
func ReportFromWire(w *workerpb.Report) extract.Report {
	if w == nil {
		return extract.Report{}
	}
	r := extract.Report{
		Format: clip(w.GetFormat(), maxFormat), Chars: nonNeg(int(w.GetChars())), Pages: nonNeg(int(w.GetPages())),
		Sheets: nonNeg(int(w.GetSheets())), Slides: nonNeg(int(w.GetSlides())), Tables: nonNeg(int(w.GetTables())),
		Images: nonNeg(int(w.GetImages())), YieldCharsPerPage: w.GetYieldCharsPerPage(),
		InferredHeadings: nonNeg(int(w.GetInferredHeadings())), DroppedDecorativeImages: nonNeg(int(w.GetDroppedDecorativeImages())),
	}
	for i, n := range w.GetNotes() {
		if i == maxNotes {
			break
		}
		r.Notes = append(r.Notes, clip(n, maxNoteBytes))
	}
	for i, y := range w.GetPageYields() {
		if i == maxYields {
			break
		}
		r.PageYields = append(r.PageYields, extract.PageYield{Page: int(y.GetPage()), Chars: nonNeg(int(y.GetChars())), Unread: y.GetUnread()})
	}
	return r
}

// ErrorToWire classifies a job's error for Done.
func ErrorToWire(err error) (workerpb.ErrorClass, string) {
	var refused *extract.RefusedError
	switch {
	case err == nil:
		return workerpb.ErrorClass_ERROR_CLASS_NONE, ""
	case errors.Is(err, extract.ErrSinkFull):
		return workerpb.ErrorClass_ERROR_CLASS_SINK_FULL, ""
	case errors.Is(err, extract.ErrUnsupported):
		return workerpb.ErrorClass_ERROR_CLASS_UNSUPPORTED, ""
	case errors.Is(err, context.DeadlineExceeded):
		return workerpb.ErrorClass_ERROR_CLASS_DEADLINE, ""
	case errors.Is(err, context.Canceled):
		return workerpb.ErrorClass_ERROR_CLASS_CANCELLED, ""
	case errors.As(err, &refused):
		return workerpb.ErrorClass_ERROR_CLASS_REFUSED, refused.Reason
	}
	return workerpb.ErrorClass_ERROR_CLASS_FAILED, err.Error()
}

// ErrorFromWire is the error the core's caller sees for a Done: the same
// sentinel or type the in-process extractor would have returned, so the
// caller's wording does not depend on where the job ran.
func ErrorFromWire(class workerpb.ErrorClass, msg string) error {
	msg = clip(msg, maxNoteBytes)
	switch class {
	case workerpb.ErrorClass_ERROR_CLASS_NONE:
		return nil
	case workerpb.ErrorClass_ERROR_CLASS_SINK_FULL:
		return extract.ErrSinkFull
	case workerpb.ErrorClass_ERROR_CLASS_UNSUPPORTED:
		return extract.ErrUnsupported
	case workerpb.ErrorClass_ERROR_CLASS_DEADLINE:
		return context.DeadlineExceeded
	case workerpb.ErrorClass_ERROR_CLASS_CANCELLED:
		return context.Canceled
	case workerpb.ErrorClass_ERROR_CLASS_REFUSED:
		return &extract.RefusedError{Reason: msg}
	case workerpb.ErrorClass_ERROR_CLASS_INPUT:
		return errors.New("convert: the worker refused the input: " + msg)
	}
	if msg == "" {
		msg = "the worker gave no reason"
	}
	// The extractors' own errors read "extract: ..."; keep that shape.
	if !strings.HasPrefix(msg, "extract: ") {
		msg = "extract: " + msg
	}
	return errors.New(msg)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func nonNeg(n int) int { return max(n, 0) }
