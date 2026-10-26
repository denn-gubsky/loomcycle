// Package transcribe is the transcription substrate: the engine interfaces OCR
// and speech-to-text engines implement, the effective per-run caps for an
// agent, the per-run budget that enforces them, and the sanitisers for what a
// model may pass in (a language hint, a vocabulary hint).
//
// Transcription is a runtime subroutine. The File tool reads bytes from the
// inbox, cuts them into Units an engine accepts, reserves the pages or seconds
// against the run's Budget BEFORE any engine call, calls the engine one Unit at
// a time, and writes text back. The tool owns cutting, ordering, retries, caps
// and the ledger; an engine owns exactly one call. No image, page or audio
// byte ever enters the agent's context.
//
// Engines, providers, models and helper commands are operator configuration
// (config.TranscriptionConfig). The types here are deliberately minimal; the
// phases that add engines extend them.
package transcribe

import (
	"context"
	"errors"
	"io"
	"time"
)

// OCREngine turns one page image, or one PDF page range, into text.
type OCREngine interface {
	Name() string
	// Accepts reports whether the engine can read this media type (image/*,
	// application/pdf).
	Accepts(mediaType string) bool
	// Plan resolves, without calling it, what the engine would use for a file
	// of mediaType in the run on ctx: the model, and whether a PDF goes whole
	// or one page image at a time. A file the engine cannot take is refused
	// here, before anything is reserved or spent; a dry run reports the plan.
	Plan(ctx context.Context, mediaType string) (Plan, error)
	// OCR makes one call for one unit under plan. Its Result carries what the
	// call spent, also alongside an error when the call got that far.
	OCR(ctx context.Context, plan Plan, unit Unit, opts OCROptions) (Result, error)
}

// Plan is what an OCR engine will use for one file.
type Plan struct {
	// Engine is the configured engine ("vision").
	Engine string
	// Provider and Model are the pair a model-backed engine calls.
	Provider string
	Model    string
	// WholePDF is true when the engine reads a PDF page range as one document;
	// false when it reads one page image at a time, so a PDF is rasterised
	// page by page first.
	WholePDF bool
	// MaxUnitBytes is the most one call may carry; the caller cuts a page
	// range smaller when it would not fit. 0 is no limit.
	MaxUnitBytes int64
}

// Result is one OCR call: the text of each page of its unit, in page order,
// and what the call spent. Usage fields are zero for an engine that is not
// a model.
type Result struct {
	Pages []Page
	// Provider and Model are the pair that served the call.
	Provider string
	Model    string
	// InputTokens and OutputTokens are what the model reported.
	InputTokens  int
	OutputTokens int
	// CredentialSource is whose key paid ("operator" when empty, "tenant",
	// "user"); CredentialScopeID names the override's owner.
	CredentialSource  string
	CredentialScopeID string
	// ReportedCost is the provider's own price for the call, when its answer
	// carried one; it wins over the pricing table.
	ReportedCost     float64
	ReportedCurrency string
}

// STTEngine turns one cut of audio (or a video the engine accepts whole) into
// timed segments.
type STTEngine interface {
	Name() string
	Accepts(mediaType string) bool
	// MaxBytes is the endpoint's per-request cap; a larger file is cut by the
	// media helper or refused, never truncated.
	MaxBytes() int64
	Transcribe(ctx context.Context, unit Unit, opts STTOptions) (Segments, error)
}

// Unit is one engine request's worth of input: a bounded window of a file,
// its media type, and which pages or which stretch of time it covers. It is an
// io.ReaderAt that cannot read outside its window, so an engine handed one cut
// cannot reach the next.
type Unit struct {
	data *io.SectionReader
	// MediaType is the type the engine receives (application/pdf, image/png,
	// audio/wav…).
	MediaType string
	// Pages is the page range a paged cut covers; zero for a non-paged unit.
	Pages PageRange
	// Span is the stretch of time a timed cut covers; zero for an untimed one.
	Span TimeRange
}

// NewUnit is the window [off, off+n) of r, of the given media type.
func NewUnit(r io.ReaderAt, off, n int64, mediaType string) (Unit, error) {
	switch {
	case r == nil:
		return Unit{}, errors.New("transcribe: unit has no reader")
	case off < 0:
		return Unit{}, errors.New("transcribe: unit offset is negative")
	case n < 0:
		return Unit{}, errors.New("transcribe: unit size is negative")
	case mediaType == "":
		return Unit{}, errors.New("transcribe: unit has no media type")
	}
	return Unit{data: io.NewSectionReader(r, off, n), MediaType: mediaType}, nil
}

// ReadAt reads within the unit's window; past its end it returns io.EOF.
func (u Unit) ReadAt(p []byte, off int64) (int, error) {
	if u.data == nil {
		return 0, io.EOF
	}
	return u.data.ReadAt(p, off)
}

// Size is the window's length in bytes.
func (u Unit) Size() int64 {
	if u.data == nil {
		return 0
	}
	return u.data.Size()
}

// PageRange is an inclusive, 1-based page range. The zero value is no pages.
type PageRange struct {
	First, Last int
}

// Count is the number of pages in the range; 0 for an empty or inverted one.
func (r PageRange) Count() int {
	if r.First < 1 || r.Last < r.First {
		return 0
	}
	return r.Last - r.First + 1
}

// TimeRange is a stretch of a recording, from Start to End.
type TimeRange struct {
	Start, End time.Duration
}

// Page is one page of OCR output. Text is Markdown; an empty answer from the
// engine is recorded as "[no text]" by the caller, not here.
type Page struct {
	Number int
	Text   string
}

// Segment is one stretch of transcribed speech. Speaker is empty when the
// engine does not diarize.
type Segment struct {
	Start, End time.Duration
	Speaker    string
	Text       string
}

// Segments is an engine's transcript of one Unit, in order.
type Segments []Segment

// OCROptions are the per-call inputs an OCR engine reads. Language has been
// through SanitizeLanguage; Prompt is the operator's template or the built-in
// instruction — never model text.
type OCROptions struct {
	Language  string
	Prompt    string
	MaxTokens int
}

// Diarize says whether to ask the engine for speaker labels.
type Diarize string

const (
	// DiarizeAuto asks for labels when the model supports them.
	DiarizeAuto Diarize = "auto"
	DiarizeOn   Diarize = "on"
	DiarizeOff  Diarize = "off"
)

// STTOptions are the per-call inputs a speech engine reads. Language has been
// through SanitizeSTTLanguage and Vocabulary through SanitizeVocabulary.
type STTOptions struct {
	Language   string
	Vocabulary string
	Diarize    Diarize
}
