package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// DjVu reads a DjVu document's text layer (RFC GV P3): a "## Page N" section
// per page, as the PDF reader writes, with the document's outline as
// headings and its metadata in the report. The container is walked here
// (djvu_iff.go); the text, the outline and the metadata come from
// DjVuLibre's programs through Helpers, which a converter that has them
// provides (D6). With no Helpers the extractor reads nothing, so it is never
// in Default(): a converter registers it.
//
// A page with no text layer is written with its heading and nothing else,
// and its yield (zero) is in Report.PageYields, which is what an OCR pass
// chooses pages by. Pages without a text chunk are never asked of the
// helper: a scanned book costs one container walk.
type DjVu struct {
	Helpers DjVuHelpers
}

// DjVuHelpers runs DjVuLibre's programs over one document.
type DjVuHelpers interface {
	// Open makes the document readable by the helpers, for the calls of
	// one extraction; the session is closed when it ends.
	Open(ctx context.Context, r io.ReaderAt, size int64) (DjVuSession, error)
	// Renders reports whether a session can render pages (Render): the
	// rasterize helper is configured here. Asked without opening one.
	Renders() bool
}

// DjVuSession is one document open for the helpers.
type DjVuSession interface {
	// Text runs the text helper (djvutxt --detail=page) over pages, a page
	// list such as "1-3,7", and returns its standard output, at most
	// maxBytes of it; cut says there was more.
	Text(ctx context.Context, pages string, maxBytes int) (out []byte, cut bool, err error)
	// Inspect runs the inspect helper (djvused -e) with script, which is
	// always one of the two fixed scripts below.
	Inspect(ctx context.Context, script DjVuScript, maxBytes int) (out []byte, cut bool, err error)
	// Render runs the rasterize helper (ddjvu) over page, fitted in a
	// maxSide × maxSide box, and returns the TIFF it wrote, at most
	// maxBytes of it (a larger one is an error, never cut). ErrHelperUnavailable
	// when no rasterize helper is configured.
	Render(ctx context.Context, page, maxSide, maxBytes int) ([]byte, error)
	Close() error
}

// DjVuScript is a djvused script the extractor runs: only DjVuOutline and
// DjVuMeta exist. Nothing from the file or the caller is ever a script.
type DjVuScript string

const (
	DjVuOutline DjVuScript = "print-outline"
	DjVuMeta    DjVuScript = "print-meta"
)

// Valid reports whether s is one of the two scripts.
func (s DjVuScript) Valid() bool { return s == DjVuOutline || s == DjVuMeta }

// ErrHelperUnavailable is a session's answer when a helper is not configured
// or its program is not installed: no page will read better, so the
// extraction stops (the text helper) or goes on without it (inspect).
var ErrHelperUnavailable = errors.New("extract: the DjVu helper is not available")

// DjVuMediaType is the media type of a DjVu document; image/x-djvu is the
// older name of the same type.
const DjVuMediaType = "image/vnd.djvu"

// IsDjVu reports whether a file of mediaType (or, when that says nothing,
// name) is a DjVu document. A DjVu file is a document, never an image a
// model can be shown.
func IsDjVu(mediaType, name string) bool {
	return matchType(mediaType, name, []string{DjVuMediaType, "image/x-djvu", "image/x.djvu"}, []string{".djvu", ".djv"})
}

func (DjVu) Match(mediaType, name string) bool { return IsDjVu(mediaType, name) }

const (
	// MaxDjVuPages caps the pages one extraction reads, as for a PDF.
	MaxDjVuPages = MaxPDFPages
	// djvuBatch is the pages asked of the text helper in one call: few
	// enough that a max_chars cut wastes little, many enough that a whole
	// book is not one process per page.
	djvuBatch = 16
	// maxDjVuPageBytes bounds the text helper's answer per page asked;
	// maxDjVuPageChars caps the text kept from one page.
	maxDjVuPageBytes = 1 << 20
	maxDjVuPageChars = maxPDFPageChars
	// maxDjVuInspectBytes bounds an outline or a metadata answer, and
	// maxDjVuOutline the outline entries kept.
	maxDjVuInspectBytes = 4 << 20
	maxDjVuOutline      = 5000
)

func (o Options) djvuBatch() int {
	if o.djvuBatchSize > 0 {
		return o.djvuBatchSize
	}
	return djvuBatch
}

func (e DjVu) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (rep Report, err error) {
	rep = Report{Format: "djvu"}
	if e.Helpers == nil {
		return rep, ErrUnsupported
	}
	info, err := walkDjVu(r, size)
	if err != nil {
		return rep, err
	}
	rep.Pages = info.pages
	x := &djvuExtract{e: e, r: r, size: size, info: info, rep: &rep, w: newWriter(ctx, sink, opts), batch: opts.djvuBatch()}
	defer x.close()
	err = x.run(opts)
	if err == nil || errors.Is(err, ErrSinkFull) {
		// After the text: the pages a cut kept are the pages OCR replaces.
		if rerr := x.render(opts.PageImages); rerr != nil {
			err = rerr
		}
	}
	rep.finish(x.w)
	x.notes()
	return rep, err
}

type djvuExtract struct {
	e     DjVu
	r     io.ReaderAt
	size  int64
	info  djvuInfo
	rep   *Report
	w     *writer
	batch int

	sess DjVuSession
	// text holds the pages of the batch fetched last.
	text map[int]djvuPage

	outline        map[int][]djvuHeading
	outlineSkipped int
	inspectFailed  bool
	pagesRead      int
	textChars      int
	lowPages       []int
	cutPages       []int
	unreadPages    []int
	// written are the pages whose Markdown the sink took, in order: the
	// only pages rendering may choose.
	written []int
}

type djvuPage struct {
	text   string
	unread bool
}

type djvuHeading struct {
	level int
	title string
}

func (x *djvuExtract) close() {
	if x.sess != nil {
		_ = x.sess.Close()
	}
}

// session opens the document for the helpers the first time one is needed.
func (x *djvuExtract) session() (DjVuSession, error) {
	if x.sess == nil {
		s, err := x.e.Helpers.Open(x.w.ctx, x.r, x.size)
		if err != nil {
			return nil, err
		}
		x.sess = s
	}
	return x.sess, nil
}

func (x *djvuExtract) run(opts Options) error {
	total := x.info.pages
	read := min(total, MaxDjVuPages)
	pages := opts.Pages
	if len(pages) == 0 {
		pages = make([]int, read)
		for i := range pages {
			pages[i] = i + 1
		}
	} else {
		// Only the pages asked for, inside the document and the cap. The
		// helper clamps a page past the end to the last page, so a number
		// the walk did not find must never reach it.
		var in []int
		for _, n := range pages {
			if n >= 1 && n <= read && (len(in) == 0 || n > in[len(in)-1]) {
				in = append(in, n)
			}
		}
		if len(in) < len(pages) {
			x.rep.note("pages asked for that the document does not have (it has %d) were not read", total)
		}
		pages = in
	}

	meta := x.metadata()
	if err := x.w.ctx.Err(); err != nil {
		return err
	}
	x.rep.Metadata = meta
	t := title(opts.Name, "Document")
	if meta != nil && meta.Title != "" {
		t = meta.Title
	}
	if err := x.w.text(heading(1, t)); err != nil {
		return err
	}
	x.readOutline()
	if err := x.w.ctx.Err(); err != nil {
		return err
	}

	var withText []int
	for _, n := range pages {
		if x.info.hasText(n) {
			withText = append(withText, n)
		}
	}
	for _, n := range pages {
		if err := x.w.ctx.Err(); err != nil {
			return err
		}
		p := djvuPage{}
		if x.info.hasText(n) {
			if _, ok := x.text[n]; !ok {
				if err := x.fetch(withText, n); err != nil {
					return err
				}
			}
			p = x.text[n]
		}
		if err := x.page(n, p); err != nil {
			return err
		}
	}
	switch {
	case len(opts.Pages) > 0:
		if len(pages) > 0 {
			x.rep.note("only %s of %d were read, as asked", pagesPhrase(pages), total)
		}
	case total > read:
		x.rep.note("only the first %d of %d pages were read", read, total)
	}
	return nil
}

// fetch asks the text helper for the batch of text pages that starts at n.
// A batch the helper fails on, or answers past its bound, is asked again a
// page at a time, so one damaged page costs only itself.
func (x *djvuExtract) fetch(withText []int, n int) error {
	i := 0
	for i < len(withText) && withText[i] != n {
		i++
	}
	batch := withText[i:min(i+x.batch, len(withText))]
	x.text = map[int]djvuPage{}
	if err := x.ask(batch); err == nil {
		return nil
	} else if !retryable(err) {
		return err
	}
	for _, p := range batch {
		err := x.ask([]int{p})
		switch {
		case err == nil:
		case !retryable(err):
			return err
		default:
			x.text[p] = djvuPage{unread: true}
		}
	}
	return nil
}

// errDjVuAnswer is a text answer that cannot be matched to its pages.
var errDjVuAnswer = errors.New("extract: the DjVu text helper's answer does not match the pages asked for")

// retryable: a page-level failure, as opposed to the context ending or the
// helper being unavailable, which no smaller batch changes.
func retryable(err error) bool {
	return !errors.Is(err, ErrHelperUnavailable) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (x *djvuExtract) ask(batch []int) error {
	s, err := x.session()
	if err != nil {
		return err
	}
	if err := x.w.ctx.Err(); err != nil {
		return err
	}
	out, cut, err := s.Text(x.w.ctx, pageSpec(batch), len(batch)*maxDjVuPageBytes)
	if err != nil {
		if cerr := x.w.ctx.Err(); cerr != nil {
			return cerr
		}
		return err
	}
	if cut {
		return errDjVuAnswer
	}
	exprs, err := parseSexprs(out)
	if err != nil {
		return err
	}
	if len(exprs) != len(batch) {
		return errDjVuAnswer
	}
	for i, e := range exprs {
		t, ok := djvuPageText(e)
		if !ok {
			return errDjVuAnswer
		}
		x.text[batch[i]] = djvuPage{text: t}
	}
	return nil
}

// pageSpec writes pages (ascending) as djvutxt's --page list: "1-3,7".
func pageSpec(pages []int) string {
	var b strings.Builder
	for i := 0; i < len(pages); {
		j := i
		for j+1 < len(pages) && pages[j+1] == pages[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(pages[i]))
		if j > i {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(pages[j]))
		}
		i = j + 1
	}
	return b.String()
}

// page writes page n: where it starts, the outline's headings that point
// to it, its "## Page N" heading and its text.
func (x *djvuExtract) page(n int, p djvuPage) error {
	if ps, ok := x.w.sink.(PageSink); ok {
		ps.StartPage(n)
	}
	var parts []string
	for _, h := range x.outline[n] {
		parts = append(parts, heading(h.level, h.title))
	}
	parts = append(parts, heading(2, fmt.Sprintf("Page %d", n)))
	text, chars, cut := djvuMarkdown(p.text)
	if text != "" {
		parts = append(parts, text)
	}
	x.textChars += chars
	x.rep.PageYields = append(x.rep.PageYields, PageYield{Page: n, Chars: chars, Unread: p.unread})
	switch {
	case p.unread:
		x.unreadPages = append(x.unreadPages, n)
	case chars < LowYieldChars:
		x.lowPages = append(x.lowPages, n)
	}
	if cut {
		x.cutPages = append(x.cutPages, n)
	}
	if err := x.w.text("\n\n" + strings.Join(parts, "\n\n")); err != nil {
		return err
	}
	x.written = append(x.written, n)
	x.pagesRead++
	return nil
}

// djvuMarkdown renders a page's text as paragraphs, and counts its
// characters without whitespace. DjVuLibre separates columns, regions and
// paragraphs with \v, \035 and \037; each is a paragraph break here.
func djvuMarkdown(text string) (md string, chars int, cut bool) {
	if text == "" {
		return "", 0, false
	}
	text = strings.ToValidUTF8(text, "")
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\v' || r == '\f' || r == 0x1d || r == 0x1f:
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, text)
	text = strings.ReplaceAll(text, " ", "\n\n")
	if t, c := cutRunesPlain(text, maxDjVuPageChars); c {
		text, cut = t, true
	}
	var paras []string
	for _, block := range strings.Split(text, "\n\n") {
		if p := paragraph(block); p != "" {
			paras = append(paras, p)
		}
	}
	for _, r := range text {
		if !unicode.IsSpace(r) {
			chars++
		}
	}
	return strings.Join(paras, "\n\n"), chars, cut
}

// inspect runs one fixed script, reporting false (and noting it once) when
// the helper failed. A helper that is not available is no failure: the
// document is read without an outline or metadata.
func (x *djvuExtract) inspect(script DjVuScript) ([]byte, bool) {
	s, err := x.session()
	if err == nil {
		var out []byte
		var cut bool
		out, cut, err = s.Inspect(x.w.ctx, script, maxDjVuInspectBytes)
		if err == nil && !cut {
			return out, true
		}
		if err == nil {
			err = errDjVuAnswer
		}
	}
	if !errors.Is(err, ErrHelperUnavailable) && x.w.ctx.Err() == nil && !x.inspectFailed {
		x.inspectFailed = true
		x.rep.note("the document's outline or metadata could not be read")
	}
	return nil, false
}

// metadata reads the document's metadata, when it has annotations to hold
// any.
func (x *djvuExtract) metadata() *BookMetadata {
	if !x.info.anno {
		return nil
	}
	out, ok := x.inspect(DjVuMeta)
	if !ok {
		return nil
	}
	return djvuMetadata(out)
}

// djvuMetadata reads print-meta's `key "value"` lines. Keys are the
// document's own words (DjVu fixes none); the common ones map to fields.
func djvuMetadata(out []byte) *BookMetadata {
	exprs, err := parseSexprs(out)
	if err != nil {
		return nil
	}
	m := &BookMetadata{SourceFormat: "djvu"}
	for i := 0; i+1 < len(exprs); i += 2 {
		k, v := exprs[i], exprs[i+1]
		if k.atom == "" || !v.isStr {
			continue
		}
		val := strings.ToValidUTF8(v.str, "")
		switch strings.ToLower(k.atom) {
		case "title", "booktitle":
			if m.Title == "" {
				m.Title = val
			}
		case "author":
			m.Authors = append(m.Authors, val)
		case "translator":
			m.Translators = append(m.Translators, val)
		case "year", "date":
			if m.Date == "" {
				m.Date = val
			}
		case "publisher":
			m.Publisher = val
		case "language", "lang":
			m.Language = val
		case "isbn":
			m.Identifiers = append(m.Identifiers, BookIdentifier{Scheme: "isbn", Value: val})
		case "subject", "keywords":
			m.Subjects = append(m.Subjects, val)
		case "description", "abstract", "note":
			if m.Description == "" {
				m.Description = val
			}
		case "series":
			m.Series = val
		}
	}
	return m.Clean()
}

// readOutline reads the bookmarks, when the document has any, into the
// headings written before the pages they point to.
func (x *djvuExtract) readOutline() {
	if !x.info.outline {
		return
	}
	out, ok := x.inspect(DjVuOutline)
	if !ok {
		return
	}
	exprs, err := parseSexprs(out)
	if err != nil {
		x.rep.note("the document's outline could not be read")
		return
	}
	x.outline = map[int][]djvuHeading{}
	kept := 0
	var walk func(list []sexpr, depth int)
	walk = func(list []sexpr, depth int) {
		for _, b := range list {
			// ("title" "#page" children...)
			if !b.isList || len(b.list) < 2 || !b.list[0].isStr || !b.list[1].isStr {
				continue
			}
			n, ok := djvuOutlinePage(b.list[1].str, x.info.pages)
			title := oneLine(strings.ToValidUTF8(b.list[0].str, ""))
			switch {
			case !ok || title == "":
				x.outlineSkipped++
			case kept < maxDjVuOutline:
				x.outline[n] = append(x.outline[n], djvuHeading{level: min(depth+1, 6), title: title})
				kept++
			}
			walk(b.list[2:], depth+1)
		}
	}
	for _, e := range exprs {
		if e.isList && len(e.list) > 0 && e.list[0].atom == "bookmarks" {
			walk(e.list[1:], 1)
		}
	}
	if kept == maxDjVuOutline {
		x.rep.note("the outline was cut at %d entries", maxDjVuOutline)
	}
}

// djvuOutlinePage is the page an outline link names: "#12" is page 12. A
// link to a page by its file name, or anywhere else, names no page here.
func djvuOutlinePage(link string, pages int) (int, bool) {
	s, ok := strings.CutPrefix(link, "#")
	if !ok || s == "" || len(s) > 6 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > pages {
		return 0, false
	}
	return n, true
}

func (x *djvuExtract) notes() {
	if x.pagesRead > 0 {
		x.rep.YieldCharsPerPage = float64(x.textChars) / float64(x.pagesRead)
	} else {
		x.rep.YieldCharsPerPage = 0
	}
	if len(x.lowPages) > 0 {
		if len(x.lowPages) == x.pagesRead {
			x.rep.note("no page has %d or more characters of text: the DjVu is likely a scan without a text layer", LowYieldChars)
		} else {
			x.rep.note("pages with under %d characters of text, likely scanned: %s", LowYieldChars, pageList(x.lowPages))
		}
	}
	if len(x.unreadPages) > 0 {
		x.rep.note("the text layer of %s could not be read", pagesPhrase(x.unreadPages))
	}
	if len(x.cutPages) > 0 {
		x.rep.note("the text of %s was cut at %d characters", pagesPhrase(x.cutPages), maxDjVuPageChars)
	}
	if x.outlineSkipped > 0 {
		x.rep.note("%d outline entries that point to no page of the document were left out", x.outlineSkipped)
	}
}
