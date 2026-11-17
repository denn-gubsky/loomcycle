// Package render turns a chunked Document back into a file (RFC GR): Markdown,
// HTML, DOCX, ODT, EPUB, and PDF through Typst source a helper program
// typesets.
//
// The work is in two halves. The core reads the Document from the store and
// builds a Doc — a format-neutral tree with a stable anchor per chunk, the
// bodies parsed with goldmark, the images as named assets (Builder). A writer
// then turns the Doc into bytes (Write). The two halves meet in a bundle, a zip
// of the Doc and its images (EncodeBundle), which is what a conversion worker
// receives: a worker cannot reach the store, and a hostile body or picture then
// only takes a worker down (RFC GO).
//
// Nothing in a body ever becomes markup in the output. Raw HTML in a body is
// text, links keep only the http, https and mailto schemes, and every writer
// escapes what its format would read as syntax.
package render

import "errors"

// Formats this package writes.
const (
	FormatMarkdown = "md"
	FormatHTML     = "html"
)

// MediaType is the type of a rendered file of format, "" for a format this
// package does not write.
func MediaType(format string) string {
	switch format {
	case FormatMarkdown:
		return "text/markdown"
	case FormatHTML:
		return "text/html"
	case FormatDOCX:
		return docxMediaType
	case FormatPDF:
		return "application/pdf"
	case FormatODT:
		return odtMediaType
	case FormatEPUB:
		return epubMediaType
	}
	return ""
}

// Supported reports whether format is one a render produces: pdf among
// them, which this package writes as Typst source for a helper to typeset.
func Supported(format string) bool { return MediaType(format) != "" }

// Limits (RFC GR §4.6). The core enforces the input caps while it builds a
// Doc; a worker enforces them again on the bundle it is sent, whose author it
// does not trust.
const (
	MaxSections  = 20000
	MaxBodyBytes = 32 << 20
	MaxImages    = 2000
	MaxImage     = 8 << 20
	// MaxImageBytes caps the pictures of one render together, so a bundle
	// (pictures plus bodies) stays under MaxBundle.
	MaxImageBytes = 192 << 20
	MaxBundle     = 256 << 20
	MaxOutput     = 64 << 20
	// maxDepth bounds the nesting of blocks the writers recurse through. A
	// body nested deeper (a list in a quote in a list, thousands deep) is cut
	// there; goldmark itself parses it iteratively, the writers would not.
	maxDepth = 64
)

var (
	// ErrUnsupported is returned for a format this package does not write.
	ErrUnsupported = errors.New("render: unsupported format")
	// ErrTooLarge is returned when a bundle or an output passes its cap.
	ErrTooLarge = errors.New("render: too large")
	// ErrBadBundle is returned for a bundle that is not one this package wrote.
	ErrBadBundle = errors.New("render: malformed bundle")
)

// Doc is a Document as the writers see it.
type Doc struct {
	// Title is the document's (or the rendered subtree's) title.
	Title string `json:"title"`
	// Anchor is the title's anchor: the root chunk's.
	Anchor string `json:"anchor,omitempty"`
	// Status is the root chunk's status. draft and review are shown beside
	// the title (D15); every status goes into the file's properties.
	Status string `json:"status,omitempty"`
	// Provenance is one line saying where an imported document came from.
	Provenance string `json:"provenance,omitempty"`
	// Date is the render date, YYYY-MM-DD in UTC. It is an input, so the
	// same Doc always writes the same bytes (G6).
	Date string `json:"date,omitempty"`
	// Language is the document's language tag (uk, en, ...), "" if unknown.
	Language string `json:"language,omitempty"`
	// Blocks are the root chunk's own body, before its first section.
	Blocks []Node `json:"blocks,omitempty"`
	// Sections are the root's children, each with its own.
	Sections []*Section `json:"sections,omitempty"`
	// Images are the pictures the nodes name, in the order they were added.
	// Their bytes travel beside the Doc in a bundle, never in its JSON.
	Images []Image `json:"images,omitempty"`
}

// Section is one chunk below the root.
type Section struct {
	// ID is the chunk's id; Anchor its anchor in the output.
	ID     string `json:"id,omitempty"`
	Anchor string `json:"anchor"`
	Title  string `json:"title"`
	// Level is the heading level, 2 to 6: the root's children are 2.
	Level    int        `json:"level"`
	Blocks   []Node     `json:"blocks,omitempty"`
	Children []*Section `json:"children,omitempty"`
}

// Image is a picture a node names.
type Image struct {
	// Name is the picture's file name, img-N.<ext>, unique in the Doc.
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"-"`
}

// Node kinds. Block kinds hold blocks or inline nodes; inline kinds hold
// inline nodes, or Text.
const (
	KindParagraph = "p"
	KindHeading   = "h"    // a heading inside a body: Level, inline children
	KindList      = "list" // Ordered, Start; children are KindItem
	KindItem      = "li"   // block children
	KindTable     = "table"
	KindRow       = "tr" // Header; children are KindCell
	KindCell      = "td" // inline children
	KindCode      = "code"
	KindQuote     = "quote"
	KindImage     = "img" // Asset, Alt; a block or inline
	KindDiagram   = "diagram"
	KindRule      = "hr"

	KindText     = "text"
	KindEmph     = "em"
	KindStrong   = "strong"
	KindStrike   = "del"
	KindCodeSpan = "cs"
	KindLink     = "a" // Href (external) or Anchor (inside the Doc)
	KindBreak    = "br"
)

// Node is one block or inline element. One struct for every kind keeps the
// JSON a worker decodes flat and closed: a kind it does not know is refused.
type Node struct {
	Kind     string   `json:"k"`
	Text     string   `json:"t,omitempty"`
	Level    int      `json:"level,omitempty"`
	Ordered  bool     `json:"ordered,omitempty"`
	Start    int      `json:"start,omitempty"`
	Header   bool     `json:"header,omitempty"`
	Align    []string `json:"align,omitempty"` // KindTable: left, center, right or ""
	Lang     string   `json:"lang,omitempty"`
	Href     string   `json:"href,omitempty"`
	Anchor   string   `json:"anchor,omitempty"`
	Asset    string   `json:"asset,omitempty"`
	Alt      string   `json:"alt,omitempty"`
	Children []Node   `json:"c,omitempty"`
}

// Options are what a caller chooses about the output.
type Options struct {
	// TOC adds a table of contents after the title.
	TOC bool `json:"toc,omitempty"`
	// TitlePage puts the title on a page of its own (D14).
	TitlePage bool `json:"title_page,omitempty"`
	// PageSize is a4 (the default) or letter, for the paged formats.
	PageSize string `json:"page_size,omitempty"`
	// AssetDir is the folder a Markdown rendering links its pictures under,
	// relative to the Markdown file.
	AssetDir string `json:"asset_dir,omitempty"`
	// ImagePaths, when set, is where each picture was written, by its name,
	// relative to the Markdown file; it wins over AssetDir. The caller
	// writes the pictures first and may have had to rename them.
	ImagePaths map[string]string `json:"image_paths,omitempty"`
}

// Report says what a render contained and what it left out.
type Report struct {
	Format         string   `json:"format"`
	Sections       int      `json:"sections"`
	Images         int      `json:"images"`
	Tables         int      `json:"tables"`
	Pages          int      `json:"pages,omitempty"`
	ImagesSkipped  int      `json:"images_skipped,omitempty"`
	BodiesCut      int      `json:"bodies_cut,omitempty"`
	LinksFlattened int      `json:"links_flattened,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// statusLabel is the label shown beside the title for status (D15), "" for
// a status that is not marked.
func statusLabel(status string) string {
	switch status {
	case "draft":
		return "Draft"
	case "review":
		return "Review"
	}
	return ""
}
