package render

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// FormatPDF is a PDF, typeset by Typst from the source WriteTypst writes.
const FormatPDF = "pdf"

// The PDF writer (RFC GR P2) is two steps. WriteTypst, here, turns a Doc into
// Typst source; a helper program (typst compile, run by internal/convert in a
// scratch folder) typesets it. Write refuses pdf: this package runs no
// program.
//
// Every piece of text from the Doc is written as a Typst string literal
// (#"…"), never as markup. Inside a string only \ and " mean anything, and
// both are escaped, so nothing a body holds — #import, #read, $…$, <label>,
// @ref, `raw`, // comments, * and _ — can reach code mode or markup. The
// writer emits a fixed set of functions and never #import, #include, read(),
// json() or a package, so the typesetter needs no file beyond the job's
// pictures and no network (RFC GR §4.2, §4.7).
//
// Labels and internal links: Typst refuses a link to a label that does not
// exist, or exists twice, so only the first use of a valid anchor becomes a
// label and a link to any other anchor is its text.

// MaxPDFPages is the most pages a rendered PDF may have (RFC GR §4.6),
// checked once the typesetter has written it.
const MaxPDFPages = 2000

// maxPDFPixels is the largest picture (width × height) the PDF embeds. The
// typesetter decodes every picture whole; a larger one is its alt text.
const maxPDFPixels = 40_000_000

// Page geometry in points: the margin, and the text width each paper leaves.
const (
	typMarginPt   = 70.87 // 2.5 cm
	typPxToPt     = 0.75  // a pixel at 96 dpi
	typA4TextW    = 595.28 - 2*typMarginPt
	typLetterTxtW = 612.0 - 2*typMarginPt
)

// typLangRe is a language tag Typst's text(lang:) takes; another is left
// out, since Typst refuses an unknown one.
var typLangRe = regexp.MustCompile(`^[a-z]{2,3}$`)

// typRawLangRe is a code block's language as raw(lang:) gets it.
var typRawLangRe = regexp.MustCompile(`^[A-Za-z0-9_+.-]{1,32}$`)

// TypstImagePath is where WriteTypst's source reads a picture from, relative
// to the source file; the caller writes each picture there.
func TypstImagePath(name string) string { return "images/" + name }

// WriteTypst writes d as Typst source for the typesetter and returns the
// pictures the source reads, each at TypstImagePath(name), for the caller to
// write beside it. Only pictures that decode are used: one that does not (or
// is too large) is its alt text, so one bad picture cannot fail the PDF.
func WriteTypst(ctx context.Context, w io.Writer, d *Doc, opts Options) ([]Image, error) {
	opts = withStylePage(d, opts)
	cw := &capWriter{w: w, ctx: ctx}
	bw := bufio.NewWriterSize(cw, 64<<10)
	t := &typWriter{w: bw, d: d, opts: opts, images: map[string]typImage{}, anchors: map[string]bool{}, labelled: map[string]bool{}}
	t.textW = typA4TextW
	if opts.PageSize == "letter" {
		t.textW = typLetterTxtW
	}
	if d.Style != nil {
		// The text width a style's margins leave, in points.
		pageW := 595.28
		if opts.PageSize == "letter" {
			pageW = 612
		}
		t.textW = pageW - 2*d.Style.marginMM()*72/25.4
	}
	var used []Image
	for _, im := range d.Images {
		if !imageNameRe.MatchString(im.Name) || t.images[im.Name] != (typImage{}) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if wpx, hpx, ok := typDecodes(im.Data); ok {
			wpx, hpx = im.shown(wpx, hpx)
			t.images[im.Name] = typImage{w: wpx, h: hpx}
			used = append(used, im)
		}
	}
	if d.Embed != nil {
		js, err := d.Embed.JSON()
		if err != nil {
			return nil, err
		}
		t.embed = string(js)
	}
	t.collectAnchors()
	t.document()
	if err := bw.Flush(); err != nil {
		return nil, firstErr(cw.err, err)
	}
	if cw.err != nil {
		return nil, cw.err
	}
	return used, nil
}

// typDecodes reports a picture's size when it decodes whole and is not too
// large. The full decode is the point: a picture whose header reads but
// whose data does not would stop the typesetter.
func typDecodes(data []byte) (w, h int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPDFPixels {
		return 0, 0, false
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

type typImage struct{ w, h int }

type typWriter struct {
	w        *bufio.Writer
	d        *Doc
	opts     Options
	textW    float64
	images   map[string]typImage
	anchors  map[string]bool // anchors that will carry a label
	labelled map[string]bool // anchors already labelled
	// embed is the record a PDF carries as an attachment (D21), "" for none.
	embed string
}

func (t *typWriter) s(s string) { _, _ = t.w.WriteString(s) }

// str writes text as a Typst string literal in markup: #"…".
func (t *typWriter) str(s string) {
	if s == "" {
		return
	}
	t.s("#")
	t.s(typString(s))
}

// typString is s as a Typst string literal, quotes included. Only \ and "
// are syntax inside one; control characters are written as escapes, and
// invalid UTF-8 as U+FFFD.
func typString(s string) string {
	s = strings.ToValidUTF8(s, "�")
	// The two characters that are syntax inside a literal, escaped first
	// and on their own (backslash before quote, so the quote's escape is
	// not escaped again); the loop below handles only control characters.
	s = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029:
			// Not text a reader wants; written as an escape so the source
			// holds no raw control character.
			if r == 0 {
				continue
			}
			b.WriteString(`\u{` + strconv.FormatInt(int64(r), 16) + `}`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// collectAnchors marks the anchors that get a label: valid, and each once.
func (t *typWriter) collectAnchors() {
	add := func(a string) {
		if ValidAnchor(a) {
			t.anchors[a] = true
		}
	}
	add(t.d.Anchor)
	var walk func(ss []*Section, depth int)
	walk = func(ss []*Section, depth int) {
		if depth > maxDepth {
			return
		}
		for _, s := range ss {
			if s == nil {
				continue
			}
			add(s.Anchor)
			walk(s.Children, depth+1)
		}
	}
	walk(t.d.Sections, 0)
}

// label writes <anchor> after an element, the first time only.
func (t *typWriter) label(anchor string) {
	if !t.anchors[anchor] || t.labelled[anchor] {
		return
	}
	t.labelled[anchor] = true
	t.s(" <" + anchor + ">")
}

func (t *typWriter) document() {
	d := t.d
	t.s("// Written by loomcycle (RFC GR). Every text is a string literal.\n")
	t.s("#set document(title: " + typString(oneLine(d.Title)))
	if m := dateRe.FindString(d.Date); m != "" {
		y, _ := strconv.Atoi(m[0:4])
		mo, _ := strconv.Atoi(m[5:7])
		dd, _ := strconv.Atoi(m[8:10])
		if mo >= 1 && mo <= 12 && dd >= 1 && dd <= 31 {
			t.s(fmt.Sprintf(", date: datetime(year: %d, month: %d, day: %d)", y, mo, dd))
		} else {
			t.s(", date: none")
		}
	} else {
		// Typst's default is the time of typesetting: the same Doc would
		// make different bytes.
		t.s(", date: none")
	}
	t.s(")\n")
	paper := `"a4"`
	if t.opts.PageSize == "letter" {
		paper = `"us-letter"`
	}
	st := d.Style
	if st == nil {
		t.s("#set page(paper: " + paper + ", margin: 2.5cm, numbering: \"1\")\n")
		t.s(`#set text(font: ("Noto Sans", "DejaVu Sans", "Libertinus Serif"), size: 10.5pt`)
	} else {
		t.stylePage(st, paper)
		font := `("Noto Sans", "DejaVu Sans", "Libertinus Serif")`
		if st.serif() {
			font = `("Noto Serif", "DejaVu Serif", "Libertinus Serif")`
		}
		t.s(`#set text(font: ` + font + `, size: ` + typNum(st.BaseSize()) + `pt, fill: rgb("` + st.textColor() + `")`)
	}
	if typLangRe.MatchString(d.Language) {
		t.s(`, lang: "` + d.Language + `"`)
	}
	t.s(")\n")
	t.s("#set par(justify: false, leading: 0.65em)\n")
	t.s(`#show raw: set text(font: ("Noto Sans Mono", "DejaVu Sans Mono"), size: 9pt)` + "\n")
	if st == nil {
		t.s("#show link: set text(fill: rgb(\"#1d4ed8\"))\n")
	} else {
		t.s(`#show link: set text(fill: rgb("` + st.linkColor() + `"))` + "\n")
		t.s(`#show heading: set text(fill: rgb("` + st.headingColor() + `"))` + "\n")
		if k := st.headScale(); k != 1 {
			t.s(`#show heading: it => { set text(size: ` + typNum(k) + `em); it }` + "\n")
		}
	}
	t.s("#set table(stroke: 0.5pt + luma(170), inset: 5pt)\n")
	t.s("#show quote.where(block: true): it => block(stroke: (left: 2pt + luma(200)), inset: (left: 10pt, y: 3pt), it.body)\n\n")
	if t.embed != "" {
		// The record as an attached file (D21), its bytes from a string
		// literal: nothing is read from disk.
		t.s(`#pdf.attach("` + EmbedName + `", bytes(` + typString(t.embed) + `), mime-type: "application/json", description: "loomcycle document metadata")` + "\n\n")
	}

	// The style's logo above the title.
	if im, ok := d.logo(); ok {
		if _, decodes := t.images[im.Name]; decodes {
			t.s("#block(below: 0.8em)[#image(" + typString(TypstImagePath(im.Name)) + ", width: " + typNum(st.logoMM()) + "mm, alt: \"logo\")]\n")
		}
	}
	// The title, with the Draft or Review label beside it (D15).
	t.s("#block(below: 0.8em)[#text(size: 20pt, weight: \"bold\")[")
	t.str(oneLine(d.Title))
	t.s("]")
	if l := statusLabel(d.Status); l != "" {
		t.s("#h(0.6em)#box(inset: (x: 4pt, y: 2pt), stroke: 0.5pt + luma(120), radius: 2pt, baseline: -2pt)[#text(size: 8pt, fill: luma(80))[")
		t.str(l)
		t.s("]]")
	}
	t.s("]")
	t.label(d.Anchor)
	t.s("\n\n")
	var meta []string
	if d.Provenance != "" {
		meta = append(meta, oneLine(d.Provenance))
	}
	if d.Date != "" {
		meta = append(meta, oneLine(d.Date))
	}
	if len(meta) > 0 {
		t.s("#par[#text(size: 9pt, fill: luma(90))[")
		t.str(strings.Join(meta, " · "))
		t.s("]]\n\n")
	}
	if t.opts.TitlePage {
		t.s("#pagebreak()\n\n")
	}
	if t.opts.TOC && len(d.Sections) > 0 {
		t.s("#outline(title: none, depth: 6)\n\n")
	}
	t.blocks(d.Blocks, 0)
	for _, s := range d.Sections {
		t.section(s, 0)
	}
}

// typHeadingLevel maps a section level (2 for the root's children) to a
// Typst heading level, as the DOCX writer maps it to Heading 1…6.
func typHeadingLevel(level int) int {
	return max(clampLevel(level)-1, 1)
}

func (t *typWriter) section(s *Section, depth int) {
	if s == nil || depth > maxDepth {
		return
	}
	t.s("#heading(level: " + strconv.Itoa(typHeadingLevel(s.Level)) + ")[")
	t.str(oneLine(s.Title))
	t.s("]")
	t.label(s.Anchor)
	t.s("\n\n")
	t.blocks(s.Blocks, 0)
	for _, c := range s.Children {
		t.section(c, depth+1)
	}
}

func (t *typWriter) blocks(ns []Node, depth int) {
	for _, n := range ns {
		t.block(n, depth)
	}
}

func (t *typWriter) block(n Node, depth int) {
	if depth > maxDepth {
		return
	}
	switch n.Kind {
	case KindParagraph:
		t.s("#par[")
		t.inl(n.Children, depth+1)
		t.s("]\n\n")
	case KindHeading:
		t.s("#heading(level: " + strconv.Itoa(typHeadingLevel(n.Level)) + ", outlined: false)[")
		t.inl(n.Children, depth+1)
		t.s("]\n\n")
	case KindRule:
		t.s("#line(length: 100%, stroke: 0.5pt + luma(170))\n\n")
	case KindCode, KindDiagram:
		t.s("#block(fill: luma(245), inset: 8pt, radius: 2pt, width: 100%)[#raw(")
		t.s(typString(n.Text))
		t.s(", block: true")
		if typRawLangRe.MatchString(n.Lang) {
			t.s(", lang: " + typString(n.Lang))
		}
		t.s(")]\n\n")
		if n.Kind == KindDiagram {
			t.s("#par[#text(size: 9pt, fill: luma(90))[")
			t.str(oneLine(n.Lang) + " diagram")
			t.s("]]\n\n")
		}
	case KindImage:
		t.s("#block[")
		t.image(n, false)
		t.s("]\n\n")
	case KindQuote:
		t.s("#quote(block: true)[\n")
		t.blocks(n.Children, depth+1)
		t.s("]\n\n")
	case KindList:
		if n.Ordered {
			t.s("#enum(start: " + strconv.Itoa(max(n.Start, 1)))
		} else {
			t.s("#list(")
		}
		for i, it := range n.Children {
			if n.Ordered || i > 0 {
				t.s(", ")
			}
			t.s("[")
			if len(it.Children) > 0 {
				t.s("\n")
				t.blocks(it.Children, depth+1)
			}
			t.s("]")
		}
		t.s(")\n\n")
	case KindTable:
		t.table(n, depth)
	}
}

func (t *typWriter) table(n Node, depth int) {
	cols := 0
	for _, r := range n.Children {
		cols = max(cols, len(r.Children))
	}
	if cols == 0 {
		return
	}
	t.s("#table(columns: " + strconv.Itoa(cols) + ", align: (")
	for i := 0; i < cols; i++ {
		if i > 0 {
			t.s(", ")
		}
		a := "left"
		if i < len(n.Align) {
			switch n.Align[i] {
			case "center":
				a = "center"
			case "right":
				a = "right"
			}
		}
		t.s(a)
	}
	t.s(")")
	rows := n.Children
	// Leading header rows repeat on each page; a header row further down
	// is an ordinary row in bold.
	h := 0
	for h < len(rows) && rows[h].Header {
		h++
	}
	if h > 0 {
		t.s(",\n  table.header(")
		for i, r := range rows[:h] {
			if i > 0 {
				t.s(", ")
			}
			t.row(r, cols, depth)
		}
		t.s(")")
	}
	for _, r := range rows[h:] {
		t.s(",\n  ")
		t.row(r, cols, depth)
	}
	t.s(")\n\n")
}

// row writes one row's cells, padded to cols, comma-separated.
func (t *typWriter) row(r Node, cols, depth int) {
	for i := 0; i < cols; i++ {
		if i > 0 {
			t.s(", ")
		}
		t.s("[")
		if i < len(r.Children) {
			if r.Header {
				t.s("#strong[")
			}
			t.inl(r.Children[i].Children, depth+1)
			if r.Header {
				t.s("]")
			}
		}
		t.s("]")
	}
}

func (t *typWriter) inl(ns []Node, depth int) {
	if depth > maxDepth {
		return
	}
	for _, n := range ns {
		switch n.Kind {
		case KindText:
			t.str(n.Text)
		case KindBreak:
			t.s("#linebreak()")
		case KindEmph:
			t.s("#emph[")
			t.inl(n.Children, depth+1)
			t.s("]")
		case KindStrong:
			t.s("#strong[")
			t.inl(n.Children, depth+1)
			t.s("]")
		case KindStrike:
			t.s("#strike[")
			t.inl(n.Children, depth+1)
			t.s("]")
		case KindCodeSpan:
			if n.Text != "" {
				t.s("#raw(" + typString(n.Text) + ")")
			}
		case KindLink:
			switch {
			case n.Anchor != "" && t.anchors[n.Anchor]:
				t.s("#link(<" + n.Anchor + ">)[")
			case n.Anchor == "":
				href, _, ok := ExternalOnly(n.Href)
				if !ok {
					t.inl(n.Children, depth+1)
					continue
				}
				t.s("#link(" + typString(href) + ")[")
			default:
				// An anchor the output has no label for: its text.
				t.inl(n.Children, depth+1)
				continue
			}
			t.inl(n.Children, depth+1)
			t.s("]")
		case KindImage:
			t.image(n, true)
		}
	}
}

// image writes a picture scaled down to the text width; inline in a box.
// One that does not decode is its alt text.
func (t *typWriter) image(n Node, inline bool) {
	m, ok := t.images[n.Asset]
	if !ok {
		t.str("[" + oneLine(n.Alt) + "]")
		return
	}
	wpt := min(float64(m.w)*typPxToPt, t.textW)
	call := "image(" + typString(TypstImagePath(n.Asset)) + ", width: " + strconv.FormatFloat(wpt, 'f', 2, 64) + "pt"
	if alt := oneLine(n.Alt); alt != "" {
		call += ", alt: " + typString(alt)
	}
	call += ")"
	if inline {
		t.s("#box(" + call + ")")
		return
	}
	t.s("#" + call)
}

// typNum is a number as Typst source: a checked style value, at most two
// decimals.
func typNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// stylePage writes the page rule for a style: its margins, and its header
// and footer lines as string literals, the footer ending in the page number
// unless the style turns that off.
func (t *typWriter) stylePage(st *Style, paper string) {
	t.s("#set page(paper: " + paper + ", margin: " + typNum(st.marginMM()) + "mm")
	small := `text(size: 8pt, fill: luma(110))`
	if h := st.header(); h != "" {
		t.s(", header: align(right, " + small + "[#" + typString(h) + "])")
	}
	switch f, n := st.footer(), st.pageNumbers(); {
	case f != "" && n:
		t.s(", footer: " + small + "[#" + typString(f) + " #h(1fr) #context counter(page).display()]")
	case f != "":
		t.s(", footer: " + small + "[#" + typString(f) + "]")
	case n:
		t.s(`, numbering: "1"`)
	}
	t.s(")\n")
}
