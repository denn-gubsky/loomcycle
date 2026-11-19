// Package wire holds what the dispatcher and the worker must agree on beyond
// the generated messages: the protocol version, the metadata keys, the chunk
// size, the labels, and the translation of an extraction's report and error to
// and from their wire form.
package wire

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/render"
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

// Labels a worker carries: what it can do. Every worker extracts, cuts PDF
// pages, re-encodes images and writes documents; render, media, encode and
// write_pdf need the worker's helper program for each, so a worker carries
// them only when it resolves. write (a document rendered to a file, RFC GR)
// is not render, which is rasterizing a PDF page for OCR; write_pdf is a
// document written as a PDF, which needs the typesetter.
const (
	LabelExtract = "extract"
	LabelPDF     = "pdf"
	LabelImage   = "image"
	LabelRender  = "render"
	LabelMedia   = "media"
	LabelEncode  = "encode"
	LabelWrite   = "write"
	// LabelWritePDF is a document render with format pdf (RFC GR P2). A
	// worker built before it never carries it, so a PDF job never reaches a
	// worker that would refuse the format.
	LabelWritePDF = "write_pdf"
	// LabelWriteBook is a document render with format odt or epub (RFC GR
	// P4a). Every worker that writes them carries it; one built before
	// them never does, so such a job never reaches a worker that would
	// refuse the format.
	LabelWriteBook = "write_odt_epub"
	// LabelExtractPages is an extraction of only some of a document's pages
	// (JobStart.pages on an extract job, RFC GP D17). A worker built before
	// it ignores the pages and reads the whole file, so such a job goes only
	// to a worker that carries this label; every worker built since does.
	LabelExtractPages = "extract_pages"
	// LabelDraw is a Mermaid diagram drawn as a picture (RFC GR P4d), with
	// the diagram helper. A worker carries it when the helper resolves.
	LabelDraw = "draw_diagram"
	// LabelDjVu is an extraction of a DjVu document (RFC GV P3), which
	// needs DjVuLibre's djvutxt. A worker carries it when its text helper
	// resolves.
	LabelDjVu = "djvu"
	// LabelDjVuRender is a DjVu extraction that renders pages for OCR (RFC
	// GV P4, D7), which needs DjVuLibre's ddjvu. A worker carries it when
	// its rasterize helper resolves; one built before it never does, so a
	// job that asks for page images never reaches a worker that would
	// ignore the ask.
	LabelDjVuRender = "djvu_render"
)

// ExtractLabels are the labels an extract job needs, every one of them:
// extract, or djvu for a DjVu document (and djvu_render when it renders
// pages for OCR); and extract_pages when only some pages are read. A file
// is DjVu as a worker's registry would find it: by its media type, or by
// its name when no other extractor reads that type.
func ExtractLabels(mediaType, name string, pages, pageImages bool) []string {
	need := []string{LabelExtract}
	if IsDjVuJob(mediaType, name) {
		need = []string{LabelDjVu}
		if pageImages {
			need = append(need, LabelDjVuRender)
		}
	}
	if pages {
		need = append(need, LabelExtractPages)
	}
	return need
}

// IsDjVuJob reports whether an extract job of mediaType and name is a DjVu
// document, as a worker's registry would find it.
func IsDjVuJob(mediaType, name string) bool {
	_, typed := extract.Default().For(mediaType, "")
	return extract.IsDjVu(mediaType, "") || (!typed && extract.IsDjVu("", name))
}

// PageImagesToWire is the ask for pages rendered for OCR.
func PageImagesToWire(p *extract.PageImages) *workerpb.PageImages {
	if p == nil {
		return nil
	}
	return &workerpb.PageImages{Always: p.Always, MinChars: I32(p.MinChars), MaxPages: I32(p.MaxPages)}
}

// PageImagesFromWire is the ask a worker reads, its counts never negative.
func PageImagesFromWire(w *workerpb.PageImages) *extract.PageImages {
	if w == nil {
		return nil
	}
	return &extract.PageImages{Always: w.GetAlways(), MinChars: nonNeg(int(w.GetMinChars())), MaxPages: nonNeg(int(w.GetMaxPages()))}
}

// LabelFor is the label a job kind needs.
func LabelFor(k workerpb.JobKind) string {
	switch k {
	case workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES:
		return LabelPDF
	case workerpb.JobKind_JOB_KIND_PDF_RENDER:
		return LabelRender
	case workerpb.JobKind_JOB_KIND_MEDIA_CONVERT:
		return LabelMedia
	case workerpb.JobKind_JOB_KIND_AUDIO_ENCODE:
		return LabelEncode
	case workerpb.JobKind_JOB_KIND_IMAGE_REENCODE:
		return LabelImage
	case workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER:
		return LabelWrite
	case workerpb.JobKind_JOB_KIND_DIAGRAM_DRAW:
		return LabelDraw
	}
	return LabelExtract
}

// LabelForJob is the label a job of kind writing format needs: a PDF
// render needs the typesetter, every other job its kind's label.
func LabelForJob(k workerpb.JobKind, format string) string {
	if k == workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER {
		switch format {
		case "pdf":
			return LabelWritePDF
		case "odt", "epub":
			return LabelWriteBook
		}
	}
	return LabelFor(k)
}

// HelperLabel reports whether a label stands for an operator helper program,
// which a worker may lack while still running other jobs.
func HelperLabel(label string) bool {
	return label == LabelRender || label == LabelMedia || label == LabelEncode || label == LabelWritePDF || label == LabelDraw || label == LabelDjVu || label == LabelDjVuRender
}

// KindName is a job kind's name in metrics and logs.
func KindName(k workerpb.JobKind) string {
	switch k {
	case workerpb.JobKind_JOB_KIND_EXTRACT:
		return "extract"
	case workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES:
		return "pdf_select_pages"
	case workerpb.JobKind_JOB_KIND_PDF_RENDER:
		return "pdf_render"
	case workerpb.JobKind_JOB_KIND_MEDIA_CONVERT:
		return "media_convert"
	case workerpb.JobKind_JOB_KIND_AUDIO_ENCODE:
		return "audio_encode"
	case workerpb.JobKind_JOB_KIND_IMAGE_REENCODE:
		return "image_reencode"
	case workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER:
		return "document_render"
	case workerpb.JobKind_JOB_KIND_DIAGRAM_DRAW:
		return "diagram_draw"
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
		Format: r.Format, Chars: int64(r.Chars), Pages: I32(r.Pages), Sheets: I32(r.Sheets),
		Slides: I32(r.Slides), Tables: I32(r.Tables), Images: I32(r.Images),
		YieldCharsPerPage: r.YieldCharsPerPage, InferredHeadings: I32(r.InferredHeadings),
		DroppedDecorativeImages: I32(r.DroppedDecorativeImages), Notes: r.Notes,
	}
	for _, y := range r.PageYields {
		w.PageYields = append(w.PageYields, &workerpb.PageYield{Page: I32(y.Page), Chars: I32(y.Chars), Unread: y.Unread})
	}
	w.Metadata = metadataToWire(r.Metadata)
	w.PageImagesUnavailable = r.PageImagesUnavailable
	return w
}

func metadataToWire(m *extract.BookMetadata) *workerpb.BookMetadata {
	if m == nil {
		return nil
	}
	w := &workerpb.BookMetadata{
		Title: m.Title, Authors: m.Authors, Translators: m.Translators, Language: m.Language, Date: m.Date,
		Year: I32(m.Year), Publisher: m.Publisher, Series: m.Series, SeriesIndex: m.SeriesIndex,
		Subjects: m.Subjects, Description: m.Description, SourceFormat: m.SourceFormat,
	}
	for _, id := range m.Identifiers {
		w.Identifiers = append(w.Identifiers, &workerpb.BookIdentifier{Scheme: id.Scheme, Value: id.Value})
	}
	return w
}

// metadataFromWire is the metadata a worker sent, bounded again: Clean
// caps every value and every list, so a worker cannot hand the core more
// than an in-process extraction would keep.
func metadataFromWire(w *workerpb.BookMetadata) *extract.BookMetadata {
	if w == nil {
		return nil
	}
	m := &extract.BookMetadata{
		Title: w.GetTitle(), Language: w.GetLanguage(), Date: w.GetDate(), Year: int(w.GetYear()),
		Publisher: w.GetPublisher(), Series: w.GetSeries(), SeriesIndex: w.GetSeriesIndex(),
		Description: w.GetDescription(), SourceFormat: w.GetSourceFormat(),
		Authors:     firstN(w.GetAuthors(), extract.MaxMetaList),
		Translators: firstN(w.GetTranslators(), extract.MaxMetaList),
		Subjects:    firstN(w.GetSubjects(), extract.MaxMetaList),
	}
	for i, id := range w.GetIdentifiers() {
		if i == extract.MaxMetaList {
			break
		}
		m.Identifiers = append(m.Identifiers, extract.BookIdentifier{Scheme: id.GetScheme(), Value: id.GetValue()})
	}
	return m.Clean()
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
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
	r.Metadata = metadataFromWire(w.GetMetadata())
	r.PageImagesUnavailable = w.GetPageImagesUnavailable()
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
	// A render's own refusals keep their message: it names the format, or
	// the cap the document went over.
	case errors.Is(err, render.ErrUnsupported):
		return workerpb.ErrorClass_ERROR_CLASS_UNSUPPORTED, err.Error()
	case errors.Is(err, render.ErrBadBundle):
		return workerpb.ErrorClass_ERROR_CLASS_INPUT, err.Error()
	case errors.Is(err, render.ErrTooLarge):
		return workerpb.ErrorClass_ERROR_CLASS_REFUSED, err.Error()
	case errors.Is(err, convert.ErrNotConfigured):
		return workerpb.ErrorClass_ERROR_CLASS_NOT_CONFIGURED, ""
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
// sentinel or type the in-process converter would have returned, so the
// caller's wording does not depend on where the job ran. kind shapes a
// failure's message: an extraction's reads "extract: ...", a helper's names
// its helper already.
func ErrorFromWire(kind workerpb.JobKind, class workerpb.ErrorClass, msg string) error {
	msg = clip(msg, maxNoteBytes)
	if kind == workerpb.JobKind_JOB_KIND_DOCUMENT_RENDER {
		// A render's refusals answer as the render package's own sentinels,
		// as the in-process renderer's would.
		switch class {
		case workerpb.ErrorClass_ERROR_CLASS_UNSUPPORTED:
			return remoteErrorOf(render.ErrUnsupported, msg)
		case workerpb.ErrorClass_ERROR_CLASS_INPUT:
			return remoteErrorOf(render.ErrBadBundle, msg)
		case workerpb.ErrorClass_ERROR_CLASS_REFUSED:
			return remoteErrorOf(render.ErrTooLarge, msg)
		}
	}
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
	case workerpb.ErrorClass_ERROR_CLASS_NOT_CONFIGURED:
		return convert.ErrNotConfigured
	}
	if msg == "" {
		msg = "the worker gave no reason"
	}
	if kind != workerpb.JobKind_JOB_KIND_EXTRACT && kind != workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES {
		return errors.New(msg)
	}
	// The extractors' own errors read "extract: ..."; keep that shape.
	if !strings.HasPrefix(msg, "extract: ") {
		msg = "extract: " + msg
	}
	return errors.New(msg)
}

// remoteError is a worker's message that errors.Is its sentinel. The
// message already reads as the sentinel's own error does (the worker sent
// err.Error()), so it is not prefixed again.
type remoteError struct {
	msg string
	err error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.err }

func remoteErrorOf(sentinel error, msg string) error {
	if msg == "" {
		return sentinel
	}
	return &remoteError{msg: msg, err: sentinel}
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

// I32 is n as an int32, saturated at the type's bounds: a count or page
// number past them is clamped on the wire, never wrapped into a different
// (or negative) one.
func I32(n int) int32 {
	// Explicit comparisons, not min/max: code scanning reads these as the
	// bound check the conversion needs.
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
