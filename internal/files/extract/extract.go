// Package extract turns office and web documents into Markdown (RFC FF).
//
// A file that reached a run (an MCP attachment, a download) is bytes the model
// cannot read: zipped XML, a spreadsheet's shared-string table, a slide deck.
// An Extractor reads one format and writes Markdown to a Sink, handing over the
// images it finds as bytes; the caller decides where they go and returns the
// reference the Markdown should carry for each. The package touches no file
// system, no network and no run state, so every format plugs in the same way: a
// pure-Go reader here, and later ones that call out to a command or a service.
//
// Everything a document says is untrusted. The extractors parse with
// encoding/xml (no DTDs, no entities beyond the predefined five), read zip
// archives through one bounded reader, never use a name from inside a file as
// a path, and copy images without decoding them.
package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// Image is one image found in a document, its bytes exactly as stored.
type Image struct {
	// Name is a generated name, img-<n>.<ext>; never one from the file.
	Name      string
	MediaType string
	Data      []byte
	// Alt is the document's own description of the image, if it has one.
	Alt string
	// Page is the page (PDF) and Slide the slide (PPTX) the image is on,
	// counted from 1; zero when the format has neither.
	Page  int
	Slide int
}

// Sink receives an extraction's output in document order.
type Sink interface {
	// Text appends Markdown.
	Text(md string) error
	// Image keeps an image and returns the link target the Markdown should
	// use for it. An empty target leaves the image out (its alt text stays).
	Image(img Image) (string, error)
}

// PageSink is a Sink that is told where each page of a paged document
// begins: StartPage(n) comes just before page n's Markdown is written. A
// caller that replaces one page's text (with an OCR transcript) cuts the
// Markdown at these points instead of parsing headings back out of it.
type PageSink interface {
	Sink
	StartPage(n int)
}

// ErrSinkFull is what a Sink returns when it will take no more text. An
// extractor stops and returns its report so far with this error, so the
// caller can keep what was written and say it was cut.
var ErrSinkFull = errors.New("extract: the output limit was reached")

// ErrUnsupported is returned when no extractor reads a file's type.
var ErrUnsupported = errors.New("extract: no extractor reads this type")

// Options tune one extraction.
type Options struct {
	// Name is the file's own name: the title when the document has none.
	Name string
	// Images keeps images (Sink.Image is called); false writes only their
	// alt text.
	Images bool
	// MaxRows caps the rows of a spreadsheet or CSV table (header included).
	// Zero takes DefaultMaxRows.
	MaxRows int

	// limits overrides the archive bounds, and pdfMaxPages and pdfMaxDecode
	// the PDF page and decode caps, for tests.
	limits       *zipLimits
	pdfMaxPages  int
	pdfMaxDecode int64
}

// DefaultMaxRows is the row cap when Options.MaxRows is zero.
const DefaultMaxRows = 1000

func (o Options) maxRows() int {
	if o.MaxRows > 0 {
		return o.MaxRows
	}
	return DefaultMaxRows
}

// Report says what an extraction found. Counts that do not apply to a
// format are zero and omitted.
type Report struct {
	Format string `json:"format"`
	Chars  int    `json:"chars"`
	Pages  int    `json:"pages,omitempty"`
	Sheets int    `json:"sheets,omitempty"`
	Slides int    `json:"slides,omitempty"`
	Tables int    `json:"tables,omitempty"`
	Images int    `json:"images,omitempty"`
	// YieldCharsPerPage is Chars / Pages, when the page count is known. A
	// low yield on a document with pages marks a scan, whose text is in its
	// images rather than its text layer.
	YieldCharsPerPage float64  `json:"yield_chars_per_page,omitempty"`
	Notes             []string `json:"notes,omitempty"`
	// PageYields is the text each page read held, in page order (PDF). Not
	// part of what the model is shown: a caller uses it to choose the pages
	// an OCR pass reads.
	PageYields []PageYield `json:"-"`
}

// PageYield is how much text one page's text layer gave.
type PageYield struct {
	Page int
	// Chars counts the page's text characters, whitespace excluded.
	Chars int
	// Unread is a page whose content could not be read at all: a damaged
	// page, not a scan.
	Unread bool
}

func (r *Report) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// finish fills in what follows from the counts.
func (r *Report) finish(w *writer) {
	r.Chars = w.chars
	r.Images = w.images
	if r.Pages > 0 {
		r.YieldCharsPerPage = float64(r.Chars) / float64(r.Pages)
	}
	if w.skippedImages > 0 {
		r.note("%d images were not kept", w.skippedImages)
	}
	if w.unsupportedImages > 0 {
		r.note("%d images in formats other than PNG, JPEG, GIF, WebP or BMP were left out", w.unsupportedImages)
	}
}

// Extractor reads one family of formats.
type Extractor interface {
	// Match reports whether this extractor reads a file of mediaType or,
	// when the type does not say, one with this name. Either may be "".
	Match(mediaType, name string) bool
	Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error)
}

// Registry picks the extractor for a file.
type Registry struct {
	list []Extractor
}

// NewRegistry returns a registry of the given extractors, tried in order.
func NewRegistry(ex ...Extractor) *Registry {
	return &Registry{list: append([]Extractor(nil), ex...)}
}

// Default is the registry of the pure-Go extractors this package ships.
func Default() *Registry {
	return NewRegistry(DOCX{}, XLSX{}, PPTX{}, PDF{}, HTML{}, CSV{}, Text{}, ImageFile{})
}

// Register adds an extractor, tried before the ones already there, so a
// deployment's own extractor for a type wins over the built-in one.
func (g *Registry) Register(e Extractor) {
	g.list = append([]Extractor{e}, g.list...)
}

// For returns the extractor for a file: the first that matches its sniffed
// media type, else the first that matches its name's extension.
func (g *Registry) For(mediaType, name string) (Extractor, bool) {
	if mediaType != "" {
		for _, e := range g.list {
			if e.Match(mediaType, "") {
				return e, true
			}
		}
	}
	if name != "" {
		for _, e := range g.list {
			if e.Match("", name) {
				return e, true
			}
		}
	}
	return nil, false
}

// Extract runs the extractor For picks.
func (g *Registry) Extract(ctx context.Context, mediaType string, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	e, ok := g.For(mediaType, opts.Name)
	if !ok {
		return Report{}, ErrUnsupported
	}
	return e.Extract(ctx, r, size, sink, opts)
}

// matchType is the common Match: one of types, or one of exts.
func matchType(mediaType, name string, types, exts []string) bool {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	for _, t := range types {
		if mt != "" && mt == t {
			return true
		}
	}
	ext := strings.ToLower(path.Ext(name))
	for _, e := range exts {
		if ext != "" && ext == e {
			return true
		}
	}
	return false
}

// title is the document title when the document names none: the file name
// without its extension.
func title(name, fallback string) string {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	if base == "." || base == "/" || base == "" {
		return fallback
	}
	if t := strings.TrimSpace(strings.TrimSuffix(base, path.Ext(base))); t != "" {
		return t
	}
	return fallback
}

// writer is what an extractor writes through: it counts, checks the
// context, and numbers the images.
type writer struct {
	ctx               context.Context
	sink              Sink
	keepImages        bool
	page, slide       int // where the images handed over now are
	chars             int
	nImage            int // images handed to the sink, for their names
	images            int // images kept
	skippedImages     int
	unsupportedImages int
}

func newWriter(ctx context.Context, sink Sink, opts Options) *writer {
	return &writer{ctx: ctx, sink: sink, keepImages: opts.Images}
}

func (w *writer) text(md string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.chars += utf8.RuneCountInString(md)
	return w.sink.Text(md)
}

// image hands an image to the sink and returns the Markdown that stands
// for it: an embed when it was kept, its alt text in brackets otherwise.
func (w *writer) image(data []byte, alt string) (string, error) {
	if err := w.ctx.Err(); err != nil {
		return "", err
	}
	mt, ext := imageType(data)
	if mt == "" {
		w.unsupportedImages++
		return altOnly(cleanAlt(alt)), nil
	}
	return w.keep(data, mt, ext, alt)
}

// keep hands an image whose type the caller established to the sink.
func (w *writer) keep(data []byte, mt, ext, alt string) (string, error) {
	if err := w.ctx.Err(); err != nil {
		return "", err
	}
	alt = cleanAlt(alt)
	if !w.keepImages {
		return altOnly(alt), nil
	}
	w.nImage++
	ref, err := w.sink.Image(Image{Name: fmt.Sprintf("img-%d%s", w.nImage, ext), MediaType: mt, Data: data, Alt: alt, Page: w.page, Slide: w.slide})
	if err != nil {
		return "", err
	}
	if ref == "" {
		w.skippedImages++
		return altOnly(alt), nil
	}
	w.images++
	return "![" + escapeAlt(alt) + "](" + ref + ")", nil
}

func altOnly(alt string) string {
	if alt == "" {
		return "[image]"
	}
	return "[image: " + escapeAlt(alt) + "]"
}
