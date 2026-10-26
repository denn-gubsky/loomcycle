package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDF reads a PDF's text layer: a "## Page N" section per page with the text
// read row by row, top to bottom and left to right, and the page's JPEG and
// JPEG 2000 pictures copied out byte for byte. A page whose text is in its
// pictures (a scan) has little or no text layer; the report says which pages
// those are, for a reader that can see.
//
// pdfcpu parses the file under its resource limits (object and stream sizes,
// decoded bytes, object counts, recursion depth); the content streams and
// ToUnicode CMaps are read by this package from the bytes pdfcpu decoded.
type PDF struct{}

const (
	// MaxPDFPages caps the pages one extraction reads.
	MaxPDFPages = 2000
	// LowYieldChars is the text a page needs to count as having a text
	// layer; a page with less is listed as possibly scanned.
	LowYieldChars = 50
	// maxPDFPageChars caps the text kept from one page.
	maxPDFPageChars = 200_000
	// maxPDFPageContent caps the decoded content one page may run, its form
	// XObjects included.
	maxPDFPageContent = 32 << 20
	// maxPDFFormDepth caps form XObjects drawn inside form XObjects.
	maxPDFFormDepth = 4
	// maxPDFDecode caps what any one stream may decode to, and
	// maxPDFStream what one stream may hold encoded.
	maxPDFDecode = 64 << 20
	maxPDFStream = 64 << 20
	// maxPDFInput caps the file pdfcpu reads.
	maxPDFInput = 256 << 20
)

func (PDF) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"application/pdf", "application/x-pdf"}, []string{".pdf"})
}

// pdfConfig is pdfcpu's configuration for reading untrusted input: no
// configuration directory, no network, and tight resource limits.
func pdfConfig(maxDecode int64) *model.Configuration {
	conf := model.NewStatelessConfiguration()
	conf.Offline = true
	conf.ValidationMode = model.ValidationRelaxed
	conf.Limits.MaxInputBytes = maxPDFInput
	conf.Limits.MaxObjectBytes = 16 << 20
	conf.Limits.MaxStreamBytes = maxPDFStream
	conf.Limits.MaxDecodeBytes = maxDecode
	conf.Limits.MaxObjectCount = 1_000_000
	conf.Limits.MaxObjectStreamCount = 100_000
	conf.Limits.MaxXRefEntries = 1_000_000
	return conf
}

func (PDF) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (rep Report, err error) {
	rep = Report{Format: "pdf"}
	// A malformed PDF must not take the run down: pdfcpu recovers its own
	// panics, and this catches anything that escapes it or this package.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("extract: the PDF could not be read: %s", cutPanic(p))
		}
	}()
	maxDecode := int64(maxPDFDecode)
	if opts.pdfMaxDecode > 0 {
		maxDecode = opts.pdfMaxDecode
	}
	xr, err := openPDF(ctx, r, size, maxDecode)
	if err != nil {
		return rep, err
	}
	w := newWriter(ctx, sink, opts)
	e := &pdfExtract{xr: xr, w: w, rep: &rep, maxDecode: maxDecode, seen: map[int]bool{}, fonts: map[int]*pdfFont{}}
	err = e.run(opts)
	rep.finish(w)
	e.notes()
	return rep, err
}

// errNoPage is a page the page tree does not hold.
var errNoPage = errors.New("extract: no such page")

// cutPanic is a recovered panic as one short line: it may quote the file.
func cutPanic(p any) string {
	s := oneLine(fmt.Sprint(p))
	if t, cut := cutRunesPlain(s, 200); cut {
		return t + "…"
	}
	return s
}

func cutRunesPlain(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i := 0
	for range n {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return s[:i], true
}

// openPDF parses the file and refuses one that is encrypted.
func openPDF(ctx context.Context, r io.ReaderAt, size, maxDecode int64) (*model.XRefTable, error) {
	pc, err := openPDFContext(ctx, r, size, maxDecode)
	if err != nil {
		return nil, err
	}
	return pc.XRefTable, nil
}

// openPDFContext is openPDF returning pdfcpu's whole context, which a cut
// (SelectPages) writes from.
func openPDFContext(ctx context.Context, r io.ReaderAt, size, maxDecode int64) (*model.Context, error) {
	head := make([]byte, 1024)
	n, _ := r.ReadAt(head, 0)
	if !bytes.Contains(head[:n], []byte("%PDF-")) {
		return nil, errors.New("extract: not a PDF file")
	}
	rs := io.NewSectionReader(&ctxReaderAt{ctx: ctx, r: r}, 0, size)
	pc, err := pdfcpu.Read(ctx, rs, pdfConfig(maxDecode))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if errors.Is(err, pdfcpu.ErrWrongPassword) {
			return nil, refusedf("the PDF is encrypted")
		}
		return nil, fmt.Errorf("extract: not a readable PDF: %s", cutPanic(err))
	}
	xr := pc.XRefTable
	// Every encrypted PDF is refused, including one that opens with an empty
	// password: its owner restricted it, and decrypting is not this reader's
	// business.
	if xr.Encrypt != nil || xr.E != nil {
		return nil, refusedf("the PDF is encrypted")
	}
	if err := xr.EnsurePageCount(); err != nil {
		return nil, fmt.Errorf("extract: the PDF has no readable page tree: %s", cutPanic(err))
	}
	return pc, nil
}

// ctxReaderAt fails its reads once ctx is done, so a parse of a huge or
// hostile file stops at the deadline instead of running on.
type ctxReaderAt struct {
	ctx context.Context
	r   io.ReaderAt
}

func (c *ctxReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.ReadAt(p, off)
}

type pdfExtract struct {
	xr        *model.XRefTable
	w         *writer
	rep       *Report
	maxDecode int64

	seen  map[int]bool     // image objects already kept, by object number
	fonts map[int]*pdfFont // fonts read, by object number

	lowPages      []int
	cutPages      []int
	unreadPages   []int
	unmappedPages []int
	textChars     int
	pagesRead     int
	skippedImages int // pictures stored as pixels, not JPEG or JPEG 2000
	inlineImages  int
	repeated      int
}

func (o Options) pdfPageCap() int {
	if o.pdfMaxPages > 0 {
		return o.pdfMaxPages
	}
	return MaxPDFPages
}

func (e *pdfExtract) run(opts Options) error {
	total := e.xr.PageCount
	e.rep.Pages = total
	if err := e.w.text(heading(1, title(opts.Name, "Document"))); err != nil {
		return err
	}
	read := min(total, opts.pdfPageCap())
	for n := 1; n <= read; n++ {
		if err := e.w.ctx.Err(); err != nil {
			return err
		}
		md, err := e.page(n)
		if errors.Is(err, errNoPage) {
			// The page tree promised more pages than it holds (a damaged
			// or hostile /Count): the rest are not there to read.
			e.rep.Pages = e.pagesRead
			e.rep.note("the page tree is damaged: page %d and any after it could not be found", n)
			return nil
		}
		if err != nil {
			return err
		}
		if ps, ok := e.w.sink.(PageSink); ok {
			ps.StartPage(n)
		}
		if err := e.w.text("\n\n" + md); err != nil {
			return err
		}
		e.pagesRead++
	}
	if total > read {
		e.rep.note("only the first %d of %d pages were read", read, total)
	}
	return nil
}

func (e *pdfExtract) notes() {
	if e.pagesRead > 0 {
		e.rep.YieldCharsPerPage = float64(e.textChars) / float64(e.pagesRead)
	} else {
		e.rep.YieldCharsPerPage = 0
	}
	if len(e.lowPages) > 0 {
		if len(e.lowPages) == e.pagesRead {
			e.rep.note("no page has %d or more characters of text: the PDF is likely scanned, its text only in images", LowYieldChars)
		} else {
			e.rep.note("pages with under %d characters of text, likely scanned: %s", LowYieldChars, pageList(e.lowPages))
		}
	}
	if len(e.unmappedPages) > 0 {
		e.rep.note("text in fonts with no Unicode mapping was left out on %s", pagesPhrase(e.unmappedPages))
	}
	if len(e.cutPages) > 0 {
		e.rep.note("the text of %s was cut at %d characters", pagesPhrase(e.cutPages), maxPDFPageChars)
	}
	if len(e.unreadPages) > 0 {
		e.rep.note("the content of %s could not be read", pagesPhrase(e.unreadPages))
	}
	if e.skippedImages > 0 {
		e.rep.note("%d images stored as raw pixels (not JPEG or JPEG 2000) were left out", e.skippedImages)
	}
	if e.inlineImages > 0 {
		e.rep.note("%d small images inside page content were left out", e.inlineImages)
	}
	if e.repeated > 0 {
		e.rep.note("%d images repeated on later pages were kept once", e.repeated)
	}
}

// pagesPhrase is "page 3" or "pages 1, 3–5".
func pagesPhrase(pages []int) string {
	if len(pages) == 1 {
		return "page " + strconv.Itoa(pages[0])
	}
	return "pages " + pageList(pages)
}

// pageList renders page numbers as ranges, "1, 3–5, 9", at most 40 of them.
func pageList(pages []int) string {
	var parts []string
	for i := 0; i < len(pages); {
		j := i
		for j+1 < len(pages) && pages[j+1] == pages[j]+1 {
			j++
		}
		if len(parts) == 40 {
			parts = append(parts, "…")
			break
		}
		if i == j {
			parts = append(parts, strconv.Itoa(pages[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d–%d", pages[i], pages[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// page renders one page: its heading, its text, then its pictures.
func (e *pdfExtract) page(n int) (string, error) {
	e.w.page = n
	defer func() { e.w.page = 0 }()
	parts := []string{heading(2, fmt.Sprintf("Page %d", n))}

	d, _, inh, err := e.xr.PageDict(e.w.ctx, n, false)
	if err != nil || d == nil {
		if cerr := e.w.ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", errNoPage
	}
	var res types.Dict
	if inh != nil {
		res = inh.Resources
	}
	if res == nil {
		res, _ = e.xr.DereferenceDict(d["Resources"])
	}
	p := &pdfPage{e: e, budget: maxPDFPageContent}
	content, err := e.xr.PageContent(d, n)
	switch {
	case err == nil:
		p.budget -= len(content)
		if err := p.run(content, res, identity(), 0); err != nil {
			return "", err
		}
	case errors.Is(err, model.ErrNoContent):
	default:
		if cerr := e.w.ctx.Err(); cerr != nil {
			return "", cerr
		}
		e.unreadPages = appendOnce(e.unreadPages, n)
	}
	unread := len(e.unreadPages) > 0 && e.unreadPages[len(e.unreadPages)-1] == n
	if p.unmapped {
		e.unmappedPages = append(e.unmappedPages, n)
	}

	text, cut := p.text()
	chars := utf8.RuneCountInString(strings.Join(strings.Fields(text), ""))
	e.textChars += chars
	e.rep.PageYields = append(e.rep.PageYields, PageYield{Page: n, Chars: chars, Unread: unread})
	// A page whose content could not be read is not a scan: it is listed
	// as unread, not as one for a reader that can see.
	if chars < LowYieldChars && !unread {
		e.lowPages = append(e.lowPages, n)
	}
	if cut {
		e.cutPages = append(e.cutPages, n)
	}
	if t := pdfParagraphs(text); t != "" {
		parts = append(parts, t)
	}
	parts = append(parts, p.images...)
	return strings.Join(parts, "\n\n"), nil
}

// pdfParagraphs renders page text, blank-line separated blocks, as escaped
// Markdown paragraphs.
func pdfParagraphs(text string) string {
	var out []string
	for _, block := range strings.Split(text, "\n\n") {
		if p := paragraph(block); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// ---- the content interpreter ----

type mat [6]float64 // a b c d e f

func identity() mat { return mat{1, 0, 0, 1, 0, 0} }

// mul is m × n: m applied first, then n.
func (m mat) mul(n mat) mat {
	return mat{
		m[0]*n[0] + m[1]*n[2], m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2], m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4], m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

func matOf(args []pdfVal) (mat, bool) {
	if len(args) < 6 {
		return mat{}, false
	}
	var m mat
	for i := range 6 {
		a := args[len(args)-6+i]
		if a.kind != tokNum || math.IsNaN(a.num) || math.IsInf(a.num, 0) {
			return mat{}, false
		}
		m[i] = a.num
	}
	return m, true
}

type pdfRun struct {
	x, y, endX, size float64
	text             string
	seq              int
}

type textState struct {
	font                      *pdfFont
	size, cs, ws, scale, lead float64
}

type pdfPage struct {
	e        *pdfExtract
	budget   int
	runs     []pdfRun
	chars    int
	cut      bool
	unmapped bool
	images   []string
	forms    int
}

type gstate struct {
	ctm mat
	ts  textState
}

// run interprets one content stream with its resources under ctm.
func (p *pdfPage) run(content []byte, res types.Dict, ctm mat, depth int) error {
	gs := gstate{ctm: ctm, ts: textState{scale: 1}}
	var stack []gstate
	var tm, tlm mat
	fonts := p.resourceDict(res, "Font")
	xobjects := p.resourceDict(res, "XObject")
	var err error
	ops := 0
	pdfOperators(content, func(op string, args []pdfVal) bool {
		if ops++; ops%1024 == 0 {
			if err = p.e.w.ctx.Err(); err != nil {
				return false
			}
		}
		num := func(i int) float64 {
			if i < len(args) && args[i].kind == tokNum && !math.IsNaN(args[i].num) && !math.IsInf(args[i].num, 0) {
				return args[i].num
			}
			return 0
		}
		last := func() pdfVal {
			if len(args) == 0 {
				return pdfVal{}
			}
			return args[len(args)-1]
		}
		switch op {
		case "q":
			if len(stack) < 64 {
				stack = append(stack, gs)
			}
		case "Q":
			if len(stack) > 0 {
				gs, stack = stack[len(stack)-1], stack[:len(stack)-1]
			}
		case "cm":
			if m, ok := matOf(args); ok {
				gs.ctm = m.mul(gs.ctm)
			}
		case "BT":
			tm, tlm = identity(), identity()
		case "Tf":
			if len(args) >= 2 {
				gs.ts.font = p.font(fonts, args[len(args)-2].str)
				gs.ts.size = num(len(args) - 1)
			}
		case "Tc":
			gs.ts.cs = num(0)
		case "Tw":
			gs.ts.ws = num(0)
		case "Tz":
			gs.ts.scale = num(0) / 100
		case "TL":
			gs.ts.lead = num(0)
		case "Td", "TD":
			if len(args) >= 2 {
				tx, ty := num(len(args)-2), num(len(args)-1)
				if op == "TD" {
					gs.ts.lead = -ty
				}
				tlm = mat{1, 0, 0, 1, tx, ty}.mul(tlm)
				tm = tlm
			}
		case "Tm":
			if m, ok := matOf(args); ok {
				tm, tlm = m, m
			}
		case "T*":
			tlm = mat{1, 0, 0, 1, 0, -gs.ts.lead}.mul(tlm)
			tm = tlm
		case "Tj":
			tm = p.show(&gs, tm, []pdfVal{last()})
		case "'", "\"":
			if op == "\"" && len(args) >= 3 {
				gs.ts.ws, gs.ts.cs = num(len(args)-3), num(len(args)-2)
			}
			tlm = mat{1, 0, 0, 1, 0, -gs.ts.lead}.mul(tlm)
			tm = tlm
			tm = p.show(&gs, tm, []pdfVal{last()})
		case "TJ":
			if a := last(); a.kind == tokArr {
				tm = p.show(&gs, tm, a.arr)
			}
		case "Do":
			if a := last(); a.kind == tokName {
				err = p.xobject(xobjects, a.str, gs.ctm, depth)
			}
		case "BI":
			p.e.inlineImages++
		}
		return err == nil
	})
	return err
}

func (p *pdfPage) resourceDict(res types.Dict, key string) types.Dict {
	if res == nil {
		return nil
	}
	d, err := p.e.xr.DereferenceDict(res[key])
	if err != nil {
		return nil
	}
	return d
}

func (p *pdfPage) font(fonts types.Dict, name string) *pdfFont {
	o, ok := fonts[name]
	if !ok {
		return nil
	}
	if ref, ok := o.(types.IndirectRef); ok {
		nr := ref.ObjectNumber.Value()
		if f, ok := p.e.fonts[nr]; ok {
			return f
		}
		f := loadFont(p.e.xr, o)
		p.e.fonts[nr] = f
		return f
	}
	return loadFont(p.e.xr, o)
}

// show draws strings (and TJ adjustments) and returns the text matrix after
// them. One call makes one run: a TJ array is one piece of a line.
func (p *pdfPage) show(gs *gstate, tm mat, items []pdfVal) mat {
	ts := &gs.ts
	if ts.font == nil {
		ts.font = &pdfFont{simple: latinTable(), dw: 500}
	}
	trm := tm.mul(gs.ctm)
	x, y := trm[4], trm[5]
	size := math.Abs(ts.size) * math.Hypot(trm[2], trm[3])
	var b strings.Builder
	advance := func(tx float64) {
		tm = mat{1, 0, 0, 1, tx, 0}.mul(tm)
	}
	for _, it := range items {
		switch it.kind {
		case tokNum:
			tx := -it.num / 1000 * ts.size * ts.scale
			// A gap of about a space's width inside a TJ array is a word
			// break the producer drew by moving rather than by a space.
			if it.num < -180 && b.Len() > 0 && !strings.HasSuffix(b.String(), " ") {
				b.WriteByte(' ')
			}
			advance(tx)
		case tokStr:
			ts.font.decode(it.str, func(rs []rune, code int, w float64, single bool) {
				if rs == nil && ts.font.twoByte && ts.font.unmapped {
					p.unmapped = true
				}
				for _, r := range rs {
					if r == 0 || (unicode.IsControl(r) && r != '\t') {
						continue
					}
					b.WriteRune(r)
				}
				tx := w / 1000 * ts.size
				tx += ts.cs
				if single && code == 32 {
					tx += ts.ws
				}
				advance(tx * ts.scale)
			})
		}
	}
	if s := b.String(); s != "" {
		end := tm.mul(gs.ctm)
		p.add(pdfRun{x: x, y: y, endX: end[4], size: size, text: s})
	}
	return tm
}

func (p *pdfPage) add(r pdfRun) {
	if p.cut {
		return
	}
	n := utf8.RuneCountInString(r.text)
	if p.chars+n > maxPDFPageChars {
		p.cut = true
		return
	}
	p.chars += n
	r.seq = len(p.runs)
	p.runs = append(p.runs, r)
}

// xobject draws an XObject: a picture is kept, a form is run.
func (p *pdfPage) xobject(xobjects types.Dict, name string, ctm mat, depth int) error {
	o, ok := xobjects[name]
	if !ok {
		return nil
	}
	nr := -1
	if ref, ok := o.(types.IndirectRef); ok {
		nr = ref.ObjectNumber.Value()
	}
	sd, _, err := p.e.xr.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return nil
	}
	switch pdfName(p.e.xr, sd.Dict["Subtype"]) {
	case "Image":
		return p.image(sd, nr)
	case "Form":
		if depth >= maxPDFFormDepth || p.forms >= 1000 {
			return nil
		}
		p.forms++
		if err := sd.DecodeWithLimit(p.e.maxDecode); err != nil {
			return nil
		}
		if len(sd.Content) > p.budget {
			p.e.unreadPages = appendOnce(p.e.unreadPages, p.e.w.page)
			return nil
		}
		p.budget -= len(sd.Content)
		m := identity()
		if arr, err := p.e.xr.DereferenceArray(sd.Dict["Matrix"]); err == nil && len(arr) == 6 {
			for i, v := range arr {
				m[i], _ = pdfNumber(p.e.xr, v)
			}
		}
		res, _ := p.e.xr.DereferenceDict(sd.Dict["Resources"])
		if res == nil {
			res = types.Dict{}
		}
		return p.run(sd.Content, res, m.mul(ctm), depth+1)
	}
	return nil
}

func appendOnce(s []int, n int) []int {
	if len(s) > 0 && s[len(s)-1] == n {
		return s
	}
	return append(s, n)
}

// image keeps a picture whose bytes are a file of their own: JPEG
// (DCTDecode) and JPEG 2000 (JPXDecode), copied exactly as stored. Any other
// picture is pixels in the PDF's own layout; it is counted, not re-encoded.
func (p *pdfPage) image(sd *types.StreamDict, nr int) error {
	if nr >= 0 && p.e.seen[nr] {
		p.e.repeated++
		return nil
	}
	if nr >= 0 {
		p.e.seen[nr] = true
	}
	var filters []string
	switch f := sd.Dict["Filter"].(type) {
	case types.Name:
		filters = []string{f.Value()}
	case types.Array:
		for _, x := range f {
			filters = append(filters, pdfName(p.e.xr, x))
		}
	default:
		if n := pdfName(p.e.xr, f); n != "" {
			filters = []string{n}
		}
	}
	var mt, ext string
	data := sd.Raw
	if len(filters) == 1 {
		switch filters[0] {
		case "DCTDecode":
			if bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) {
				mt, ext = "image/jpeg", ".jpg"
			}
		case "JPXDecode":
			switch {
			case bytes.HasPrefix(data, []byte{0, 0, 0, 0x0C, 'j', 'P', ' ', ' '}):
				mt, ext = "image/jp2", ".jp2"
			case bytes.HasPrefix(data, []byte{0xFF, 0x4F, 0xFF, 0x51}):
				mt, ext = "image/j2c", ".j2c"
			}
		}
	}
	if mt == "" {
		p.e.skippedImages++
		return nil
	}
	md, err := p.e.w.keep(data, mt, ext, "")
	if err != nil {
		return err
	}
	p.images = append(p.images, md)
	return nil
}

// text assembles the page's runs into lines: runs on one baseline form a
// row, read left to right; rows go top to bottom, and a gap of more than
// about two lines starts a new paragraph.
func (p *pdfPage) text() (string, bool) {
	if len(p.runs) == 0 {
		return "", p.cut
	}
	runs := append([]pdfRun(nil), p.runs...)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].y > runs[j].y })
	type row struct {
		y, size float64
		runs    []pdfRun
	}
	var rows []*row
	for _, r := range runs {
		if n := len(rows); n > 0 {
			cur := rows[n-1]
			tol := math.Max(1, 0.4*math.Max(cur.size, r.size))
			if cur.y-r.y <= tol {
				cur.runs = append(cur.runs, r)
				continue
			}
		}
		rows = append(rows, &row{y: r.y, size: r.size, runs: []pdfRun{r}})
	}
	var b strings.Builder
	for i, rw := range rows {
		sort.SliceStable(rw.runs, func(a, c int) bool {
			if rw.runs[a].x != rw.runs[c].x {
				return rw.runs[a].x < rw.runs[c].x
			}
			return rw.runs[a].seq < rw.runs[c].seq
		})
		var line strings.Builder
		for k, r := range rw.runs {
			if k > 0 {
				prev := rw.runs[k-1]
				gap := r.x - prev.endX
				s := line.String()
				if gap > 0.2*math.Max(r.size, 1) && !strings.HasSuffix(s, " ") && !strings.HasPrefix(r.text, " ") {
					line.WriteByte(' ')
				}
			}
			line.WriteString(r.text)
		}
		l := strings.Join(strings.Fields(line.String()), " ")
		if l == "" {
			continue
		}
		if i > 0 && b.Len() > 0 {
			gap := rows[i-1].y - rw.y
			if gap > 2*math.Max(rw.size, 1) {
				b.WriteString("\n\n")
			} else {
				b.WriteByte('\n')
			}
		}
		b.WriteString(l)
	}
	return b.String(), p.cut
}
