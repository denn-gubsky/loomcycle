package render

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif" // DecodeConfig of a GIF picture's size
	_ "image/jpeg"
	_ "image/png"
	"io"
	"regexp"
	"strconv"
	"strings"

	_ "golang.org/x/image/webp"
)

// docxEmbedNS is the namespace of the custom XML part that carries the
// embedded record (D21); an extractor finds the part by it.
const docxEmbedNS = "urn:loomcycle:render:1"

// docxEmbedItemID is the custom XML part's item id: a GUID made from the
// document's id, so the same Doc writes the same bytes (G6).
func docxEmbedItemID(e *Embed) string {
	h := sha256.Sum256([]byte("loomcycle-embed:" + e.DocumentID))
	x := strings.ToUpper(hex.EncodeToString(h[:16]))
	return "{" + x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32] + "}"
}

// FormatDOCX is a Word document.
const FormatDOCX = "docx"

const docxMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// The DOCX writer (RFC GR P1) emits WordprocessingML by hand, over
// archive/zip, the subset loomcycle's own extractor reads: built-in styles
// (Title, Heading1…6, Normal, Quote, a code style), numbering for lists,
// tables, pictures in word/media, bookmarks for anchors and internal and
// external hyperlinks, and the document properties.
//
// It writes text runs only. No field (w:fldSimple, w:instrText), no macro,
// no w:altChunk, and no external relationship but an http, https or mailto
// hyperlink: a body's text can never become something Word executes or
// fetches (RFC GR §4.7). A table of contents is a list of internal links,
// not a TOC field.

// Page geometry in twentieths of a point (twips), and EMUs for pictures.
const (
	twipsPerInch  = 1440
	emuPerInch    = 914400
	emuPerPixel   = 9525 // at 96 dpi
	docxMarginTw  = twipsPerInch
	maxBookmarkID = 1 << 30
)

// dateRe is a render date the properties take as it is.
var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func pageTwips(size string) (w, h int) {
	if size == "letter" {
		return 12240, 15840
	}
	return 11906, 16838 // A4
}

// docxWriter writes one document.xml and remembers the relationships it
// needs.
type docxWriter struct {
	w        *bufio.Writer
	d        *Doc
	opts     Options
	links    []string // external hyperlink targets, rel ids rLink1…
	linkIDs  map[string]int
	bookmark int
	docPr    int
	lists    []docxList // one numbering instance per list
	media    map[string]docxMedia
	textW    int64 // text width in EMU, the widest a picture is drawn
	// headerFooter says the style's header and footer parts are written.
	headerFooter bool
}

type docxList struct {
	ordered bool
	start   int
	level   int
}

type docxMedia struct {
	img        Image
	w, h       int // pixels
	ok         bool
	relID      string
	contentExt string
}

// writeDOCX writes d as a Word document. The zip streams into a capped
// writer, so MaxOutput holds on the compressed file as it is written.
func writeDOCX(ctx context.Context, w io.Writer, d *Doc, opts Options) error {
	cw := &capWriter{w: w, ctx: ctx}
	zw := zip.NewWriter(cw)
	pw, _ := pageTwips(opts.PageSize)
	x := &docxWriter{d: d, opts: opts, linkIDs: map[string]int{}, media: map[string]docxMedia{},
		textW: int64(pw-2*docxMargin(d.Style)) * emuPerInch / twipsPerInch}
	for i, im := range d.Images {
		m := docxMedia{img: im, relID: "rImg" + strconv.Itoa(i+1), contentExt: imageExt[im.MediaType]}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(im.Data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			m.w, m.h, m.ok = cfg.Width, cfg.Height, true
		}
		x.media[im.Name] = m
	}

	// The style's header and footer parts, decided before document.xml,
	// whose section refers to them.
	x.headerFooter = d.Style != nil && (d.Style.header() != "" || d.Style.footer() != "" || d.Style.pageNumbers())

	// document.xml first: it decides the hyperlinks and lists the other
	// parts name.
	if err := entry(zw, "word/document.xml", func(ew io.Writer) error {
		x.w = bufio.NewWriterSize(ew, 64<<10)
		x.document()
		return x.w.Flush()
	}); err != nil {
		return firstErr(cw.err, err)
	}
	parts := []struct {
		name string
		body string
	}{
		{"[Content_Types].xml", x.contentTypes()},
		{"_rels/.rels", docxRootRels},
		{"docProps/core.xml", x.coreProps()},
		{"docProps/app.xml", docxAppProps},
		{"word/_rels/document.xml.rels", x.documentRels()},
		{"word/styles.xml", docxStylesFor(d.Style)},
		{"word/numbering.xml", x.numbering()},
		{"word/settings.xml", docxSettings},
		{"word/fontTable.xml", docxFontTable},
	}
	if x.headerFooter {
		// The style's header and footer as their own parts; the footer's
		// page number is the writer's one field (§12.1).
		parts = append(parts,
			struct {
				name string
				body string
			}{"word/header1.xml", docxHeaderFooter("hdr", d.Style.header(), false)},
			struct {
				name string
				body string
			}{"word/footer1.xml", docxHeaderFooter("ftr", d.Style.footer(), d.Style.pageNumbers())},
		)
	}
	if d.Embed != nil {
		// The record as a custom XML part (D21): data Word keeps with the
		// document and never shows or runs.
		js, err := d.Embed.JSON()
		if err != nil {
			return firstErr(cw.err, err)
		}
		parts = append(parts,
			struct {
				name string
				body string
			}{"customXml/item1.xml", xml.Header + `<loomcycle xmlns="` + docxEmbedNS + `">` + xmlText(string(js)) + `</loomcycle>`},
			struct {
				name string
				body string
			}{"customXml/itemProps1.xml", xml.Header + `<ds:datastoreItem ds:itemID="` + docxEmbedItemID(d.Embed) + `" xmlns:ds="http://schemas.openxmlformats.org/officeDocument/2006/customXml"><ds:schemaRefs><ds:schemaRef ds:uri="` + docxEmbedNS + `"/></ds:schemaRefs></ds:datastoreItem>`},
			struct {
				name string
				body string
			}{"customXml/_rels/item1.xml.rels", xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rProps1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/customXmlProps" Target="itemProps1.xml"/></Relationships>`},
		)
	}
	for _, p := range parts {
		if err := entry(zw, p.name, func(ew io.Writer) error {
			_, err := io.WriteString(ew, p.body)
			return err
		}); err != nil {
			return firstErr(cw.err, err)
		}
	}
	for _, im := range d.Images {
		m := x.media[im.Name]
		if !m.ok {
			continue
		}
		data := im.Data
		if err := entry(zw, "word/media/"+im.Name, func(ew io.Writer) error {
			_, err := ew.Write(data)
			return err
		}); err != nil {
			return firstErr(cw.err, err)
		}
	}
	if err := zw.Close(); err != nil {
		return firstErr(cw.err, err)
	}
	return cw.err
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func entry(zw *zip.Writer, name string, write func(io.Writer) error) error {
	ew, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: bundleEpoch})
	if err != nil {
		return err
	}
	return write(ew)
}

// xmlText escapes text for an element or attribute. A character XML 1.0
// does not allow (most control characters) is replaced, as an invalid one
// would make Word refuse the whole file.
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (x *docxWriter) s(s string) { _, _ = x.w.WriteString(s) }

// bookmarkName is an anchor as a Word bookmark: letters, digits and
// underscores, at most 40 characters.
func bookmarkName(anchor string) string {
	b := []byte(anchor)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}

func (x *docxWriter) document() {
	x.s(xml.Header)
	x.s(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><w:body>`)
	d := x.d

	// The style's logo above the title.
	if im, ok := d.logo(); ok {
		if m := x.media[im.Name]; m.ok {
			x.s(`<w:p>`)
			x.imageWidth(Node{Kind: KindImage, Asset: im.Name, Alt: "logo"}, int64(d.Style.logoMM()*36000))
			x.s(`</w:p>`)
		}
	}
	// The title, with the Draft or Review label beside it (D15).
	x.s(`<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr>`)
	x.bookmarkStart(d.Anchor)
	x.run(oneLine(d.Title), "")
	if l := statusLabel(d.Status); l != "" {
		x.run(" ", "")
		x.run(l, "StatusLabel")
	}
	x.bookmarkEnd(d.Anchor)
	x.s(`</w:p>`)
	var meta []string
	if d.Provenance != "" {
		meta = append(meta, oneLine(d.Provenance))
	}
	if d.Date != "" {
		meta = append(meta, d.Date)
	}
	if len(meta) > 0 {
		x.s(`<w:p><w:pPr><w:pStyle w:val="Subtitle"/></w:pPr>`)
		x.run(strings.Join(meta, " · "), "")
		x.s(`</w:p>`)
	}
	if x.opts.TitlePage {
		x.s(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
	}
	if x.opts.TOC && len(d.Sections) > 0 {
		x.toc(d.Sections, 0)
	}
	x.blocks(d.Blocks, blockCtx{})
	for _, s := range d.Sections {
		x.section(s)
	}
	pw, ph := pageTwips(x.opts.PageSize)
	m := strconv.Itoa(docxMargin(d.Style))
	x.s(`<w:sectPr>`)
	if x.headerFooter {
		x.s(`<w:headerReference w:type="default" r:id="rHdr1"/><w:footerReference w:type="default" r:id="rFtr1"/>`)
	}
	x.s(`<w:pgSz w:w="` + strconv.Itoa(pw) + `" w:h="` + strconv.Itoa(ph) + `"/><w:pgMar w:top="` + m + `" w:right="` + m + `" w:bottom="` + m + `" w:left="` + m + `" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr>`)
	x.s(`</w:body></w:document>`)
}

func (x *docxWriter) toc(ss []*Section, depth int) {
	if depth > maxDepth {
		return
	}
	for _, s := range ss {
		x.s(`<w:p><w:pPr><w:pStyle w:val="TOCEntry"/><w:ind w:left="` + strconv.Itoa(360*min(depth, 8)) + `"/></w:pPr>`)
		x.s(`<w:hyperlink w:anchor="` + bookmarkName(s.Anchor) + `">`)
		x.run(oneLine(s.Title), "Hyperlink")
		x.s(`</w:hyperlink></w:p>`)
		x.toc(s.Children, depth+1)
	}
}

func (x *docxWriter) bookmarkStart(anchor string) {
	if anchor == "" || x.bookmark >= maxBookmarkID {
		return
	}
	x.s(`<w:bookmarkStart w:id="` + strconv.Itoa(x.bookmark) + `" w:name="` + bookmarkName(anchor) + `"/>`)
}

func (x *docxWriter) bookmarkEnd(anchor string) {
	if anchor == "" || x.bookmark >= maxBookmarkID {
		return
	}
	x.s(`<w:bookmarkEnd w:id="` + strconv.Itoa(x.bookmark) + `"/>`)
	x.bookmark++
}

// headingStyle maps a section level (2 for the root's children) to Word's
// Heading 1…6, as the extractor maps them back.
func headingStyle(level int) string {
	l := clampLevel(level) - 1
	if l < 1 {
		l = 1
	}
	return "Heading" + strconv.Itoa(l)
}

func (x *docxWriter) section(s *Section) {
	x.s(`<w:p><w:pPr><w:pStyle w:val="` + headingStyle(s.Level) + `"/></w:pPr>`)
	x.bookmarkStart(s.Anchor)
	x.run(oneLine(s.Title), "")
	x.bookmarkEnd(s.Anchor)
	x.s(`</w:p>`)
	x.blocks(s.Blocks, blockCtx{})
	for _, c := range s.Children {
		x.section(c)
	}
}

// blockCtx is where a block sits: inside quotes, inside a list item.
type blockCtx struct {
	quote int
	// numID/ilvl number the block when it is a list item's first
	// paragraph; indent is the item's text indent for its other blocks.
	numID, ilvl int
	indent      int
	depth       int
}

func (c blockCtx) indentTw() int {
	return c.indent + 567*c.quote
}

func (x *docxWriter) pPr(style string, c blockCtx, extra string) {
	x.s(`<w:pPr>`)
	if style != "" {
		x.s(`<w:pStyle w:val="` + style + `"/>`)
	}
	if c.numID > 0 {
		x.s(`<w:numPr><w:ilvl w:val="` + strconv.Itoa(c.ilvl) + `"/><w:numId w:val="` + strconv.Itoa(c.numID) + `"/></w:numPr>`)
	}
	x.s(extra)
	if c.numID == 0 {
		if in := c.indentTw(); in > 0 {
			x.s(`<w:ind w:left="` + strconv.Itoa(in) + `"/>`)
		}
	}
	x.s(`</w:pPr>`)
}

func (x *docxWriter) paraStyle(c blockCtx) string {
	switch {
	case c.quote > 0:
		return "Quote"
	case c.numID > 0 || c.indent > 0:
		return "ListParagraph"
	}
	return ""
}

func (x *docxWriter) blocks(ns []Node, c blockCtx) {
	for i, n := range ns {
		cc := c
		if i > 0 {
			cc.numID = 0 // only an item's first paragraph carries its number
		}
		x.block(n, cc)
	}
}

func (x *docxWriter) block(n Node, c blockCtx) {
	if c.depth > maxDepth {
		return
	}
	switch n.Kind {
	case KindParagraph:
		x.s(`<w:p>`)
		x.pPr(x.paraStyle(c), c, "")
		x.inlines(n.Children, "", 0)
		x.s(`</w:p>`)
	case KindHeading:
		x.s(`<w:p>`)
		x.pPr(headingStyle(n.Level), blockCtx{}, "")
		x.inlines(n.Children, "", 0)
		x.s(`</w:p>`)
	case KindRule:
		x.s(`<w:p>`)
		x.pPr("", c, `<w:pBdr><w:bottom w:val="single" w:sz="6" w:space="1" w:color="A8A29E"/></w:pBdr>`)
		x.s(`</w:p>`)
	case KindCode, KindDiagram:
		x.s(`<w:p>`)
		x.pPr("CodeBlock", c, "")
		for i, line := range strings.Split(n.Text, "\n") {
			if i > 0 {
				x.s(`<w:r><w:br/></w:r>`)
			}
			x.run(line, "")
		}
		x.s(`</w:p>`)
		if n.Kind == KindDiagram {
			x.s(`<w:p>`)
			x.pPr("Caption", blockCtx{quote: c.quote, indent: c.indent}, "")
			x.run(oneLine(n.Lang)+" diagram", "")
			x.s(`</w:p>`)
		}
	case KindImage:
		x.s(`<w:p>`)
		x.pPr(x.paraStyle(c), c, "")
		x.image(n)
		x.s(`</w:p>`)
	case KindQuote:
		cc := c
		cc.quote++
		cc.depth++
		x.blocks(n.Children, cc)
	case KindList:
		ilvl := min(c.ilvlFor(), 8)
		x.lists = append(x.lists, docxList{ordered: n.Ordered, start: max(n.Start, 1), level: ilvl})
		numID := len(x.lists)
		for _, it := range n.Children {
			cc := blockCtx{quote: c.quote, numID: numID, ilvl: ilvl, indent: 720 * (ilvl + 1), depth: c.depth + 1}
			if len(it.Children) == 0 {
				x.s(`<w:p>`)
				x.pPr("ListParagraph", cc, "")
				x.s(`</w:p>`)
				continue
			}
			x.blocks(it.Children, cc)
		}
	case KindTable:
		x.table(n, c)
	}
}

// ilvlFor is the list level of a list opened in c: one below the item it
// is in.
func (c blockCtx) ilvlFor() int {
	if c.indent == 0 {
		return 0
	}
	return c.indent / 720 // the enclosing item's level + 1
}

func (x *docxWriter) table(t Node, c blockCtx) {
	cols := 0
	for _, r := range t.Children {
		cols = max(cols, len(r.Children))
	}
	if cols == 0 {
		return
	}
	x.s(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:w="0" w:type="auto"/>`)
	if in := c.indentTw(); in > 0 {
		x.s(`<w:tblInd w:w="` + strconv.Itoa(in) + `" w:type="dxa"/>`)
	}
	x.s(`<w:tblLook w:val="04A0" w:firstRow="1" w:lastRow="0" w:firstColumn="0" w:lastColumn="0" w:noHBand="0" w:noVBand="1"/></w:tblPr><w:tblGrid>`)
	colW := int(x.textW*twipsPerInch/emuPerInch) / cols
	for i := 0; i < cols; i++ {
		x.s(`<w:gridCol w:w="` + strconv.Itoa(colW) + `"/>`)
	}
	x.s(`</w:tblGrid>`)
	for _, r := range t.Children {
		x.s(`<w:tr>`)
		if r.Header {
			x.s(`<w:trPr><w:tblHeader/></w:trPr>`)
		}
		for i := 0; i < cols; i++ {
			x.s(`<w:tc><w:tcPr><w:tcW w:w="` + strconv.Itoa(colW) + `" w:type="dxa"/></w:tcPr><w:p>`)
			jc := ""
			if i < len(t.Align) {
				switch t.Align[i] {
				case "center":
					jc = `<w:jc w:val="center"/>`
				case "right":
					jc = `<w:jc w:val="right"/>`
				}
			}
			x.pPr("TableText", blockCtx{}, jc)
			if i < len(r.Children) {
				style := ""
				if r.Header {
					style = "Strong"
				}
				x.inlines(r.Children[i].Children, style, 0)
			}
			x.s(`</w:p></w:tc>`)
		}
		x.s(`</w:tr>`)
	}
	x.s(`</w:tbl>`)
	// Word needs a paragraph between two tables, and after one that ends a
	// cell or the body; an empty one is harmless everywhere.
	x.s(`<w:p/>`)
}

// runProps are the formatting of a run being written.
type runProps struct {
	style              string
	bold, italic, strk bool
}

func (x *docxWriter) inlines(ns []Node, style string, depth int) {
	x.inl(ns, runProps{style: style}, depth)
}

func (x *docxWriter) inl(ns []Node, p runProps, depth int) {
	if depth > maxDepth {
		return
	}
	for _, n := range ns {
		switch n.Kind {
		case KindText:
			x.runP(n.Text, p)
		case KindBreak:
			x.s(`<w:r><w:br/></w:r>`)
		case KindEmph:
			q := p
			q.italic = true
			x.inl(n.Children, q, depth+1)
		case KindStrong:
			q := p
			q.bold = true
			x.inl(n.Children, q, depth+1)
		case KindStrike:
			q := p
			q.strk = true
			x.inl(n.Children, q, depth+1)
		case KindCodeSpan:
			q := p
			q.style = "CodeChar"
			x.runP(n.Text, q)
		case KindLink:
			q := p
			q.style = "Hyperlink"
			switch {
			case n.Anchor != "":
				x.s(`<w:hyperlink w:anchor="` + bookmarkName(n.Anchor) + `">`)
			default:
				href, _, ok := ExternalOnly(n.Href)
				if !ok {
					x.inl(n.Children, p, depth+1)
					continue
				}
				x.s(`<w:hyperlink r:id="` + x.linkRel(href) + `">`)
			}
			x.inl(n.Children, q, depth+1)
			x.s(`</w:hyperlink>`)
		case KindImage:
			x.image(n)
		}
	}
}

func (x *docxWriter) linkRel(href string) string {
	id, ok := x.linkIDs[href]
	if !ok {
		x.links = append(x.links, href)
		id = len(x.links)
		x.linkIDs[href] = id
	}
	return "rLink" + strconv.Itoa(id)
}

func (x *docxWriter) run(text, style string) { x.runP(text, runProps{style: style}) }

// runP writes one run. Tabs and newlines inside text become Word's own.
func (x *docxWriter) runP(text string, p runProps) {
	if text == "" {
		return
	}
	x.s(`<w:r>`)
	if p.style != "" || p.bold || p.italic || p.strk {
		x.s(`<w:rPr>`)
		if p.style != "" {
			x.s(`<w:rStyle w:val="` + p.style + `"/>`)
		}
		if p.bold {
			x.s(`<w:b/>`)
		}
		if p.italic {
			x.s(`<w:i/>`)
		}
		if p.strk {
			x.s(`<w:strike/>`)
		}
		x.s(`</w:rPr>`)
	}
	for i, part := range strings.Split(text, "\n") {
		if i > 0 {
			x.s(`<w:br/>`)
		}
		for j, seg := range strings.Split(part, "\t") {
			if j > 0 {
				x.s(`<w:tab/>`)
			}
			if seg != "" {
				x.s(`<w:t xml:space="preserve">` + xmlText(seg) + `</w:t>`)
			}
		}
	}
	x.s(`</w:r>`)
}

// image writes a picture inline, scaled down to the text width. One whose
// size cannot be read is its alt text.
func (x *docxWriter) image(n Node) { x.imageWidth(n, 0) }

// imageWidth writes a picture inline, at width EMU when width is set (the
// style's logo), else at its own size; never wider than the text.
func (x *docxWriter) imageWidth(n Node, width int64) {
	m, ok := x.media[n.Asset]
	if !ok || !m.ok {
		x.run("["+oneLine(n.Alt)+"]", "")
		return
	}
	cx, cy := int64(m.w)*emuPerPixel, int64(m.h)*emuPerPixel
	if width > 0 && cx > 0 {
		cy = cy * width / cx
		cx = width
	}
	if cx > x.textW {
		cy = cy * x.textW / cx
		cx = x.textW
	}
	if cy < 1 {
		cy = 1
	}
	x.docPr++
	id := strconv.Itoa(x.docPr)
	alt := xmlText(oneLine(n.Alt))
	ext := `cx="` + strconv.FormatInt(cx, 10) + `" cy="` + strconv.FormatInt(cy, 10) + `"`
	x.s(`<w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0"><wp:extent ` + ext + `/><wp:docPr id="` + id + `" name="Picture ` + id + `" descr="` + alt + `"/>`)
	x.s(`<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:nvPicPr><pic:cNvPr id="` + id + `" name="` + n.Asset + `" descr="` + alt + `"/><pic:cNvPicPr/></pic:nvPicPr>`)
	x.s(`<pic:blipFill><a:blip r:embed="` + m.relID + `"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>`)
	x.s(`<pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext ` + ext + `/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r>`)
}

func (x *docxWriter) contentTypes() string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>`)
	seen := map[string]bool{}
	for _, im := range x.d.Images {
		m := x.media[im.Name]
		if !m.ok || seen[m.contentExt] {
			continue
		}
		seen[m.contentExt] = true
		b.WriteString(`<Default Extension="` + m.contentExt + `" ContentType="` + im.MediaType + `"/>`)
	}
	b.WriteString(`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>`)
	b.WriteString(`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>`)
	b.WriteString(`<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>`)
	b.WriteString(`<Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/>`)
	b.WriteString(`<Override PartName="/word/fontTable.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.fontTable+xml"/>`)
	b.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
	b.WriteString(`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>`)
	if x.headerFooter {
		b.WriteString(`<Override PartName="/word/header1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml"/>`)
		b.WriteString(`<Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>`)
	}
	if x.d.Embed != nil {
		b.WriteString(`<Override PartName="/customXml/itemProps1.xml" ContentType="application/vnd.openxmlformats-officedocument.customXmlProperties+xml"/>`)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

const docxRootRels = xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
	`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>` +
	`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>` +
	`</Relationships>`

func (x *docxWriter) documentRels() string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	b.WriteString(`<Relationship Id="rStyles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	b.WriteString(`<Relationship Id="rNumbering" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>`)
	b.WriteString(`<Relationship Id="rSettings" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/settings" Target="settings.xml"/>`)
	b.WriteString(`<Relationship Id="rFonts" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/fontTable" Target="fontTable.xml"/>`)
	if x.headerFooter {
		b.WriteString(`<Relationship Id="rHdr1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/header" Target="header1.xml"/>`)
		b.WriteString(`<Relationship Id="rFtr1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>`)
	}
	if x.d.Embed != nil {
		b.WriteString(`<Relationship Id="rCustom1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/customXml" Target="../customXml/item1.xml"/>`)
	}
	for _, im := range x.d.Images {
		m := x.media[im.Name]
		if !m.ok {
			continue
		}
		b.WriteString(`<Relationship Id="` + m.relID + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/` + im.Name + `"/>`)
	}
	for i, href := range x.links {
		b.WriteString(`<Relationship Id="rLink` + strconv.Itoa(i+1) + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="` + xmlText(href) + `" TargetMode="External"/>`)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

func (x *docxWriter) coreProps() string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`)
	b.WriteString(`<dc:title>` + xmlText(oneLine(x.d.Title)) + `</dc:title>`)
	b.WriteString(`<dc:creator>loomcycle</dc:creator>`)
	if langRe.MatchString(x.d.Language) {
		b.WriteString(`<dc:language>` + x.d.Language + `</dc:language>`)
	}
	if x.d.Status != "" {
		b.WriteString(`<cp:contentStatus>` + xmlText(oneLine(x.d.Status)) + `</cp:contentStatus>`)
	}
	if dateRe.MatchString(x.d.Date) {
		b.WriteString(`<dcterms:created xsi:type="dcterms:W3CDTF">` + x.d.Date + `T00:00:00Z</dcterms:created>`)
		b.WriteString(`<dcterms:modified xsi:type="dcterms:W3CDTF">` + x.d.Date + `T00:00:00Z</dcterms:modified>`)
	}
	b.WriteString(`</cp:coreProperties>`)
	return b.String()
}

const docxAppProps = xml.Header + `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>loomcycle</Application></Properties>`

const docxSettings = xml.Header + `<w:settings xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:defaultTabStop w:val="708"/><w:characterSpacingControl w:val="doNotCompress"/><w:compat><w:compatSetting w:name="compatibilityMode" w:uri="http://schemas.microsoft.com/office/word" w:val="15"/></w:compat></w:settings>`

// docxFontTable names the fonts and their families, so a reader without
// Noto substitutes its own sans or monospace (D13).
const docxFontTable = xml.Header + `<w:fonts xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:font w:name="Noto Sans"><w:altName w:val="Calibri"/><w:charset w:val="00"/><w:family w:val="swiss"/><w:pitch w:val="variable"/></w:font>` +
	`<w:font w:name="Noto Sans Mono"><w:altName w:val="Consolas"/><w:charset w:val="00"/><w:family w:val="modern"/><w:pitch w:val="fixed"/></w:font>` +
	`<w:font w:name="Noto Serif"><w:altName w:val="Georgia"/><w:charset w:val="00"/><w:family w:val="roman"/><w:pitch w:val="variable"/></w:font>` +
	`</w:fonts>`

// numbering is one abstract list per kind (bullet, decimal) and one
// instance per list, so each ordered list counts from its own start.
func (x *docxWriter) numbering() string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<w:numbering xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	for id, ordered := range []bool{false, true} {
		b.WriteString(`<w:abstractNum w:abstractNumId="` + strconv.Itoa(id) + `"><w:multiLevelType w:val="hybridMultilevel"/>`)
		for l := 0; l < 9; l++ {
			fmtV, text := "bullet", []string{"•", "◦", "▪"}[l%3]
			if ordered {
				fmtV, text = "decimal", "%"+strconv.Itoa(l+1)+"."
			}
			ind := strconv.Itoa(720 * (l + 1))
			b.WriteString(`<w:lvl w:ilvl="` + strconv.Itoa(l) + `"><w:start w:val="1"/><w:numFmt w:val="` + fmtV + `"/><w:lvlText w:val="` + text + `"/><w:lvlJc w:val="left"/><w:pPr><w:ind w:left="` + ind + `" w:hanging="360"/></w:pPr></w:lvl>`)
		}
		b.WriteString(`</w:abstractNum>`)
	}
	for i, l := range x.lists {
		abs := 0
		if l.ordered {
			abs = 1
		}
		b.WriteString(`<w:num w:numId="` + strconv.Itoa(i+1) + `"><w:abstractNumId w:val="` + strconv.Itoa(abs) + `"/>`)
		if l.ordered {
			b.WriteString(`<w:lvlOverride w:ilvl="` + strconv.Itoa(l.level) + `"><w:startOverride w:val="` + strconv.Itoa(l.start) + `"/></w:lvlOverride>`)
		}
		b.WriteString(`</w:num>`)
	}
	b.WriteString(`</w:numbering>`)
	return b.String()
}

// docxStyles is the built-in look (D13–D15): Noto Sans for text and
// headings, Noto Sans Mono for code. Style ids and names are Word's own
// built-ins, which is what the extractor (and Word's navigation pane) read.
var docxStyles = xml.Header + `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Noto Sans" w:hAnsi="Noto Sans" w:eastAsia="Noto Sans" w:cs="Noto Sans"/><w:sz w:val="21"/><w:szCs w:val="21"/><w:lang w:val="en-US"/></w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:after="80"/></w:pPr><w:rPr><w:b/><w:sz w:val="44"/><w:szCs w:val="44"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Subtitle"><w:name w:val="Subtitle"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:after="360"/></w:pPr><w:rPr><w:color w:val="5B5B5B"/><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr></w:style>` +
	docxHeadingStyles() +
	`<w:style w:type="paragraph" w:styleId="Quote"><w:name w:val="Quote"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:pBdr><w:left w:val="single" w:sz="18" w:space="8" w:color="D6D3D1"/></w:pBdr><w:ind w:left="567"/></w:pPr><w:rPr><w:color w:val="44403C"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:after="60"/><w:contextualSpacing/></w:pPr></w:style>` +
	`<w:style w:type="paragraph" w:customStyle="1" w:styleId="CodeBlock"><w:name w:val="Code Block"/><w:basedOn w:val="Normal"/><w:pPr><w:shd w:val="clear" w:color="auto" w:fill="F5F5F4"/><w:spacing w:after="120" w:line="240" w:lineRule="auto"/></w:pPr><w:rPr><w:rFonts w:ascii="Noto Sans Mono" w:hAnsi="Noto Sans Mono" w:cs="Noto Sans Mono"/><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Caption"><w:name w:val="caption"/><w:basedOn w:val="Normal"/><w:qFormat/><w:rPr><w:i/><w:color w:val="5B5B5B"/><w:sz w:val="16"/><w:szCs w:val="16"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:customStyle="1" w:styleId="TableText"><w:name w:val="Table Text"/><w:basedOn w:val="Normal"/><w:pPr><w:spacing w:after="0"/></w:pPr></w:style>` +
	`<w:style w:type="paragraph" w:customStyle="1" w:styleId="TOCEntry"><w:name w:val="Contents Entry"/><w:basedOn w:val="Normal"/><w:pPr><w:spacing w:after="40"/></w:pPr></w:style>` +
	`<w:style w:type="character" w:default="1" w:styleId="DefaultParagraphFont"><w:name w:val="Default Paragraph Font"/><w:uiPriority w:val="1"/><w:semiHidden/></w:style>` +
	`<w:style w:type="character" w:styleId="Hyperlink"><w:name w:val="Hyperlink"/><w:rPr><w:color w:val="1D4ED8"/><w:u w:val="single"/></w:rPr></w:style>` +
	`<w:style w:type="character" w:styleId="Strong"><w:name w:val="Strong"/><w:qFormat/><w:rPr><w:b/><w:bCs/></w:rPr></w:style>` +
	`<w:style w:type="character" w:customStyle="1" w:styleId="CodeChar"><w:name w:val="Code"/><w:rPr><w:rFonts w:ascii="Noto Sans Mono" w:hAnsi="Noto Sans Mono" w:cs="Noto Sans Mono"/><w:sz w:val="19"/><w:szCs w:val="19"/><w:shd w:val="clear" w:color="auto" w:fill="F5F5F4"/></w:rPr></w:style>` +
	`<w:style w:type="character" w:customStyle="1" w:styleId="StatusLabel"><w:name w:val="Status Label"/><w:rPr><w:b/><w:caps/><w:color w:val="B45309"/><w:sz w:val="16"/><w:szCs w:val="16"/><w:bdr w:val="single" w:sz="4" w:space="1" w:color="B45309"/></w:rPr></w:style>` +
	`<w:style w:type="table" w:default="1" w:styleId="TableNormal"><w:name w:val="Normal Table"/><w:uiPriority w:val="99"/><w:semiHidden/><w:tblPr><w:tblInd w:w="0" w:type="dxa"/><w:tblCellMar><w:top w:w="0" w:type="dxa"/><w:left w:w="108" w:type="dxa"/><w:bottom w:w="0" w:type="dxa"/><w:right w:w="108" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style>` +
	`<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:basedOn w:val="TableNormal"/><w:tblPr><w:tblBorders><w:top w:val="single" w:sz="4" w:space="0" w:color="D6D3D1"/><w:left w:val="single" w:sz="4" w:space="0" w:color="D6D3D1"/><w:bottom w:val="single" w:sz="4" w:space="0" w:color="D6D3D1"/><w:right w:val="single" w:sz="4" w:space="0" w:color="D6D3D1"/><w:insideH w:val="single" w:sz="4" w:space="0" w:color="D6D3D1"/><w:insideV w:val="single" w:sz="4" w:space="0" w:color="D6D3D1"/></w:tblBorders></w:tblPr></w:style>` +
	`</w:styles>`

func docxHeadingStyles() string {
	sizes := []int{32, 28, 24, 22, 21, 21}
	var b strings.Builder
	for i, sz := range sizes {
		n := strconv.Itoa(i + 1)
		s := strconv.Itoa(sz)
		fmt.Fprintf(&b, `<w:style w:type="paragraph" w:styleId="Heading%s"><w:name w:val="heading %s"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:keepLines/><w:spacing w:before="240" w:after="80"/><w:outlineLvl w:val="%d"/></w:pPr><w:rPr><w:b/><w:sz w:val="%s"/><w:szCs w:val="%s"/></w:rPr></w:style>`, n, n, i, s, s)
	}
	return b.String()
}

// docxMargin is the page margin in twips: the style's, else one inch.
func docxMargin(st *Style) int {
	if st == nil || st.Page.MarginMM == 0 {
		return docxMarginTw
	}
	return int(st.Page.MarginMM * twipsPerInch / 25.4)
}

// docxStylesFor is styles.xml for a style: the built-in look, its font,
// size and colours replaced by the style's. Every value is a checked one: a
// hex colour, a size in range, one of two font names.
func docxStylesFor(st *Style) string {
	if st == nil {
		return docxStyles
	}
	font := "Noto Sans"
	if st.serif() {
		font = "Noto Serif"
	}
	half := strconv.Itoa(int(st.BaseSize()*2 + 0.5))
	color := strings.ToUpper(strings.TrimPrefix(st.textColor(), "#"))
	s := docxStyles
	s = strings.Replace(s, `<w:rFonts w:ascii="Noto Sans" w:hAnsi="Noto Sans" w:eastAsia="Noto Sans" w:cs="Noto Sans"/><w:sz w:val="21"/><w:szCs w:val="21"/>`,
		`<w:rFonts w:ascii="`+font+`" w:hAnsi="`+font+`" w:eastAsia="`+font+`" w:cs="`+font+`"/><w:color w:val="`+color+`"/><w:sz w:val="`+half+`"/><w:szCs w:val="`+half+`"/>`, 1)
	s = strings.Replace(s, docxHeadingStyles(), docxHeadingStylesFor(st), 1)
	s = strings.Replace(s, `<w:style w:type="character" w:styleId="Hyperlink"><w:name w:val="Hyperlink"/><w:rPr><w:color w:val="1D4ED8"/>`,
		`<w:style w:type="character" w:styleId="Hyperlink"><w:name w:val="Hyperlink"/><w:rPr><w:color w:val="`+strings.ToUpper(strings.TrimPrefix(st.linkColor(), "#"))+`"/>`, 1)
	return s
}

// docxHeadingStylesFor is Heading 1…6 at the style's scale and colour.
func docxHeadingStylesFor(st *Style) string {
	sizes := []int{32, 28, 24, 22, 21, 21}
	color := strings.ToUpper(strings.TrimPrefix(st.headingColor(), "#"))
	var b strings.Builder
	for i, sz := range sizes {
		n := strconv.Itoa(i + 1)
		s := strconv.Itoa(int(float64(sz)*st.headScale() + 0.5))
		fmt.Fprintf(&b, `<w:style w:type="paragraph" w:styleId="Heading%s"><w:name w:val="heading %s"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:keepLines/><w:spacing w:before="240" w:after="80"/><w:outlineLvl w:val="%d"/></w:pPr><w:rPr><w:b/><w:color w:val="%s"/><w:sz w:val="%s"/><w:szCs w:val="%s"/></w:rPr></w:style>`, n, n, i, color, s, s)
	}
	return b.String()
}

// docxHeaderFooter is a header or footer part: the style's line as text,
// and in the footer the page number, a PAGE field. It is the one field the
// writer writes, and its instruction is the writer's own: no text of the
// document or the style reaches it (§12.1).
func docxHeaderFooter(kind, line string, pageNumber bool) string {
	root, align := "w:hdr", "right"
	if kind == "ftr" {
		root, align = "w:ftr", "left"
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<` + root + ` xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:pPr><w:jc w:val="` + align + `"/>`)
	if pageNumber {
		b.WriteString(`<w:tabs><w:tab w:val="right" w:pos="9000"/></w:tabs>`)
	}
	b.WriteString(`</w:pPr>`)
	run := func(t string) {
		b.WriteString(`<w:r><w:rPr><w:color w:val="6E6E6E"/><w:sz w:val="16"/></w:rPr><w:t xml:space="preserve">` + xmlText(t) + `</w:t></w:r>`)
	}
	if line != "" {
		run(line)
	}
	if pageNumber {
		b.WriteString(`<w:r><w:tab/></w:r><w:fldSimple w:instr=" PAGE "><w:r><w:rPr><w:color w:val="6E6E6E"/><w:sz w:val="16"/></w:rPr><w:t>1</w:t></w:r></w:fldSimple>`)
	}
	b.WriteString(`</w:p></` + root + `>`)
	return b.String()
}
