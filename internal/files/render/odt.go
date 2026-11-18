package render

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
)

// FormatODT is an OpenDocument text file (RFC GR P4a, D17).
const FormatODT = "odt"

const odtMediaType = "application/vnd.oasis.opendocument.text"

// The ODT writer emits OpenDocument by hand, over archive/zip, as the DOCX
// writer emits WordprocessingML: named paragraph styles the extractor reads
// back (Title, Heading 1…6), automatic text styles for emphasis, list styles,
// tables, pictures in Pictures/, bookmarks for anchors and internal and
// external links, and the document's properties in meta.xml.
//
// It writes text only. No field (text:*-field, text:database-*), no script
// (office:scripts), no event listener, no form, no object, and no link but an
// anchor in the document or an http, https or mailto URL: a body's text can
// never become something an office suite runs or fetches (RFC GR §4.7).

// Page geometry: centimetres, 2.54 cm margins.
const odtMarginCm = 2.54

func pageCm(size string) (w, h float64) {
	if size == "letter" {
		return 21.59, 27.94
	}
	return 21.0, 29.7
}

// odtWriter writes one content.xml and remembers the automatic styles it
// used.
type odtWriter struct {
	w      *bufio.Writer
	d      *Doc
	opts   Options
	media  map[string]docxMedia
	textCm float64 // the text width, the widest a picture is drawn
	frames int
	tables int
	// textStyles are the run formats used, by name (T1, T2, …).
	textStyles map[runProps]string
	textOrder  []runProps
}

// writeODT writes d as an OpenDocument text file. The zip streams into a
// capped writer, so MaxOutput holds on the compressed file as it is written.
func writeODT(ctx context.Context, w io.Writer, d *Doc, opts Options) error {
	cw := &capWriter{w: w, ctx: ctx}
	zw := zip.NewWriter(cw)
	pw, _ := pageCm(opts.PageSize)
	x := &odtWriter{d: d, opts: opts, media: map[string]docxMedia{}, textCm: pw - 2*odtMargin(d.Style),
		textStyles: map[runProps]string{}}
	for _, im := range d.Images {
		m := docxMedia{img: im, contentExt: imageExt[im.MediaType]}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(im.Data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			m.w, m.h = im.shown(cfg.Width, cfg.Height)
			m.ok = true
		}
		x.media[im.Name] = m
	}

	// mimetype first, stored, with no extra field (ODF 1.2 §3.3).
	if err := storedEntry(zw, "mimetype", []byte(odtMediaType)); err != nil {
		return firstErr(cw.err, err)
	}
	// The body is written to a buffer first: its automatic styles, which go
	// before it in content.xml, are known only once it is written. The cap
	// holds on the buffer as it does on the file.
	var body bytes.Buffer
	bc := &capWriter{w: &body, ctx: ctx}
	x.w = bufio.NewWriterSize(bc, 64<<10)
	x.body()
	if err := x.w.Flush(); err != nil {
		return firstErr(bc.err, err)
	}
	if bc.err != nil {
		return bc.err
	}
	if err := entry(zw, "content.xml", func(ew io.Writer) error {
		if _, err := io.WriteString(ew, x.contentHead()); err != nil {
			return err
		}
		if _, err := body.WriteTo(ew); err != nil {
			return err
		}
		_, err := io.WriteString(ew, `</office:text></office:body></office:document-content>`)
		return err
	}); err != nil {
		return firstErr(cw.err, err)
	}
	parts := []struct{ name, body string }{
		{"styles.xml", x.styles()},
		{"meta.xml", x.meta()},
		{"META-INF/manifest.xml", x.manifest()},
	}
	if d.Embed != nil {
		// The record is a file of the package (D21), listed in the manifest.
		js, err := d.Embed.JSON()
		if err != nil {
			return err
		}
		parts = append(parts, struct{ name, body string }{EmbedName, string(js)})
	}
	for _, p := range parts {
		b := p.body
		if err := entry(zw, p.name, func(ew io.Writer) error {
			_, err := io.WriteString(ew, b)
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
		if err := entry(zw, "Pictures/"+im.Name, func(ew io.Writer) error {
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

func (x *odtWriter) s(s string) { _, _ = x.w.WriteString(s) }

const odfNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:fo="urn:oasis:names:tc:opendocument:xmlns:xsl-fo-compatible:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:meta="urn:oasis:names:tc:opendocument:xmlns:meta:1.0" xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" xmlns:loext="urn:org:documentfoundation:names:experimental:office:xmlns:loext:1.0" office:version="1.3"`

// odtBookmark is an anchor as a bookmark name. ODF allows any string; the
// same letters, digits and underscores as Word keep the two formats alike.
func odtBookmark(anchor string) string { return bookmarkName(anchor) }

func (x *odtWriter) body() {
	d := x.d
	// The style's logo above the title.
	if im, ok := d.logo(); ok {
		if m := x.media[im.Name]; m.ok {
			x.s(`<text:p text:style-name="Standard">`)
			x.imageWidth(Node{Kind: KindImage, Asset: im.Name, Alt: "logo"}, d.Style.logoMM()/10)
			x.s(`</text:p>`)
		}
	}
	x.s(`<text:p text:style-name="Title">`)
	x.bookmarkStart(d.Anchor)
	x.text(oneLine(d.Title))
	if l := statusLabel(d.Status); l != "" {
		x.s(` <text:span text:style-name="Status_20_Label">` + xmlText(l) + `</text:span>`)
	}
	x.bookmarkEnd(d.Anchor)
	x.s(`</text:p>`)
	var meta []string
	if d.Provenance != "" {
		meta = append(meta, oneLine(d.Provenance))
	}
	if d.Date != "" {
		meta = append(meta, d.Date)
	}
	if len(meta) > 0 {
		x.s(`<text:p text:style-name="Subtitle">`)
		x.text(strings.Join(meta, " · "))
		x.s(`</text:p>`)
	}
	if x.opts.TitlePage {
		x.s(`<text:p text:style-name="P_PageBreak"/>`)
	}
	if x.opts.TOC && len(d.Sections) > 0 {
		x.toc(d.Sections, 0)
	}
	x.blocks(d.Blocks, odtCtx{})
	for _, s := range d.Sections {
		x.section(s)
	}
}

func (x *odtWriter) toc(ss []*Section, depth int) {
	if depth > maxDepth {
		return
	}
	for _, s := range ss {
		x.s(`<text:p text:style-name="P_Toc` + strconv.Itoa(min(depth, 8)) + `"><text:a xlink:type="simple" xlink:href="#` + odtBookmark(s.Anchor) + `" text:style-name="Internet_20_link">`)
		x.text(oneLine(s.Title))
		x.s(`</text:a></text:p>`)
		x.toc(s.Children, depth+1)
	}
}

func (x *odtWriter) bookmarkStart(anchor string) {
	if anchor != "" {
		x.s(`<text:bookmark-start text:name="` + odtBookmark(anchor) + `"/>`)
	}
}

func (x *odtWriter) bookmarkEnd(anchor string) {
	if anchor != "" {
		x.s(`<text:bookmark-end text:name="` + odtBookmark(anchor) + `"/>`)
	}
}

// odtHeading is a section level (2 for the root's children) as an outline
// level, 1…6, the level the extractor maps back.
func odtHeading(level int) int {
	l := clampLevel(level) - 1
	if l < 1 {
		l = 1
	}
	return l
}

func (x *odtWriter) section(s *Section) {
	l := strconv.Itoa(odtHeading(s.Level))
	x.s(`<text:h text:style-name="Heading_20_` + l + `" text:outline-level="` + l + `">`)
	x.bookmarkStart(s.Anchor)
	x.text(oneLine(s.Title))
	x.bookmarkEnd(s.Anchor)
	x.s(`</text:h>`)
	x.blocks(s.Blocks, odtCtx{})
	for _, c := range s.Children {
		x.section(c)
	}
}

// odtCtx is where a block sits.
type odtCtx struct {
	quote  bool
	inItem bool // inside a list item, where ODF allows no table
	depth  int
}

func (c odtCtx) para() string {
	if c.quote {
		return "Quotations"
	}
	return "Text_20_body"
}

func (x *odtWriter) blocks(ns []Node, c odtCtx) {
	for _, n := range ns {
		x.block(n, c)
	}
}

func (x *odtWriter) block(n Node, c odtCtx) {
	if c.depth > maxDepth {
		return
	}
	switch n.Kind {
	case KindParagraph:
		x.s(`<text:p text:style-name="` + c.para() + `">`)
		x.inlines(n.Children, runProps{}, 0)
		x.s(`</text:p>`)
	case KindHeading:
		// A heading inside a body is a heading in the outline too, as the
		// DOCX writer makes it.
		l := strconv.Itoa(odtHeading(n.Level))
		x.s(`<text:h text:style-name="Heading_20_` + l + `" text:outline-level="` + l + `">`)
		x.inlines(n.Children, runProps{}, 0)
		x.s(`</text:h>`)
	case KindRule:
		x.s(`<text:p text:style-name="Horizontal_20_Line"/>`)
	case KindCode, KindDiagram:
		x.s(`<text:p text:style-name="Preformatted_20_Text">`)
		x.text(n.Text)
		x.s(`</text:p>`)
		if n.Kind == KindDiagram {
			x.s(`<text:p text:style-name="Caption">`)
			x.text(oneLine(n.Lang) + " diagram")
			x.s(`</text:p>`)
		}
	case KindImage:
		x.s(`<text:p text:style-name="` + c.para() + `">`)
		x.image(n)
		x.s(`</text:p>`)
	case KindQuote:
		cc := c
		cc.quote = true
		cc.depth++
		x.blocks(n.Children, cc)
	case KindList:
		style := "L_Bullet"
		if n.Ordered {
			style = "L_Number"
		}
		x.s(`<text:list text:style-name="` + style + `">`)
		for i, it := range n.Children {
			x.s(`<text:list-item`)
			if i == 0 && n.Ordered && n.Start > 1 {
				x.s(` text:start-value="` + strconv.Itoa(n.Start) + `"`)
			}
			x.s(`>`)
			if len(it.Children) == 0 {
				x.s(`<text:p text:style-name="List_20_Contents"/>`)
			}
			cc := odtCtx{quote: c.quote, inItem: true, depth: c.depth + 1}
			for _, b := range it.Children {
				if b.Kind == KindParagraph {
					x.s(`<text:p text:style-name="List_20_Contents">`)
					x.inlines(b.Children, runProps{}, 0)
					x.s(`</text:p>`)
					continue
				}
				x.block(b, cc)
			}
			x.s(`</text:list-item>`)
		}
		x.s(`</text:list>`)
	case KindTable:
		if c.inItem {
			// ODF puts no table in a list item: each row is one paragraph,
			// its cells joined by a vertical bar.
			for _, r := range n.Children {
				x.s(`<text:p text:style-name="List_20_Contents">`)
				for i, cell := range r.Children {
					if i > 0 {
						x.text(" | ")
					}
					x.inlines(cell.Children, runProps{bold: r.Header}, 0)
				}
				x.s(`</text:p>`)
			}
			return
		}
		x.table(n)
	}
}

func (x *odtWriter) table(t Node) {
	cols := 0
	for _, r := range t.Children {
		cols = max(cols, len(r.Children))
	}
	if cols == 0 {
		return
	}
	x.tables++
	x.s(`<table:table table:name="Table` + strconv.Itoa(x.tables) + `" table:style-name="Tbl">`)
	x.s(`<table:table-column table:number-columns-repeated="` + strconv.Itoa(cols) + `"/>`)
	inHeader := false
	for ri, r := range t.Children {
		if r.Header && ri == 0 {
			x.s(`<table:table-header-rows>`)
			inHeader = true
		}
		x.s(`<table:table-row>`)
		for i := 0; i < cols; i++ {
			x.s(`<table:table-cell table:style-name="TblCell" office:value-type="string">`)
			style := "Table_20_Contents"
			if r.Header {
				style = "Table_20_Heading"
			}
			if i < len(t.Align) {
				switch t.Align[i] {
				case "center":
					style += "_Center"
				case "right":
					style += "_Right"
				}
			}
			x.s(`<text:p text:style-name="` + style + `">`)
			if i < len(r.Children) {
				x.inlines(r.Children[i].Children, runProps{}, 0)
			}
			x.s(`</text:p></table:table-cell>`)
		}
		x.s(`</table:table-row>`)
		if inHeader {
			x.s(`</table:table-header-rows>`)
			inHeader = false
		}
	}
	x.s(`</table:table>`)
}

func (x *odtWriter) inlines(ns []Node, p runProps, depth int) {
	if depth > maxDepth {
		return
	}
	for _, n := range ns {
		switch n.Kind {
		case KindText:
			x.span(n.Text, p)
		case KindBreak:
			x.s(`<text:line-break/>`)
		case KindEmph:
			q := p
			q.italic = true
			x.inlines(n.Children, q, depth+1)
		case KindStrong:
			q := p
			q.bold = true
			x.inlines(n.Children, q, depth+1)
		case KindStrike:
			q := p
			q.strk = true
			x.inlines(n.Children, q, depth+1)
		case KindCodeSpan:
			q := p
			q.style = "code"
			x.span(n.Text, q)
		case KindLink:
			switch {
			case n.Anchor != "":
				x.s(`<text:a xlink:type="simple" xlink:href="#` + odtBookmark(n.Anchor) + `" text:style-name="Internet_20_link">`)
			default:
				href, _, ok := ExternalOnly(n.Href)
				if !ok {
					x.inlines(n.Children, p, depth+1)
					continue
				}
				x.s(`<text:a xlink:type="simple" xlink:href="` + xmlText(href) + `" text:style-name="Internet_20_link">`)
			}
			x.inlines(n.Children, p, depth+1)
			x.s(`</text:a>`)
		case KindImage:
			x.image(n)
		}
	}
}

// span writes text in the run format p: as it is when p is plain, else in an
// automatic text style named for p.
func (x *odtWriter) span(text string, p runProps) {
	if text == "" {
		return
	}
	if p == (runProps{}) {
		x.text(text)
		return
	}
	name, ok := x.textStyles[p]
	if !ok {
		name = "T" + strconv.Itoa(len(x.textOrder)+1)
		x.textStyles[p] = name
		x.textOrder = append(x.textOrder, p)
	}
	x.s(`<text:span text:style-name="` + name + `">`)
	x.text(text)
	x.s(`</text:span>`)
}

// text writes text as ODF reads it: XML-escaped, a run of spaces as
// text:s (ODF collapses white space), tabs and newlines as their elements.
// Plain stretches are escaped whole and streamed, never built in memory.
func (x *odtWriter) text(s string) {
	start := 0
	plain := func(end int) {
		if end > start {
			x.s(xmlText(s[start:end]))
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ':
			n := 1
			for i+n < len(s) && s[i+n] == ' ' {
				n++
			}
			if n == 1 {
				continue // a single space is plain text
			}
			plain(i + 1)
			x.s(`<text:s text:c="` + strconv.Itoa(n-1) + `"/>`)
			i += n - 1
			start = i + 1
		case '\t', '\n', '\r':
			plain(i)
			switch c {
			case '\t':
				x.s(`<text:tab/>`)
			case '\n':
				x.s(`<text:line-break/>`)
			}
			start = i + 1
		}
	}
	plain(len(s))
}

// image writes a picture as a frame anchored as a character, scaled down to
// the text width. One whose size cannot be read is its alt text.
func (x *odtWriter) image(n Node) { x.imageWidth(n, 0) }

// imageWidth writes a picture as a frame, at width cm when width is set
// (the style's logo), else at its own size; never wider than the text.
func (x *odtWriter) imageWidth(n Node, width float64) {
	m, ok := x.media[n.Asset]
	if !ok || !m.ok {
		x.text("[" + oneLine(n.Alt) + "]")
		return
	}
	wcm, hcm := float64(m.w)*2.54/96, float64(m.h)*2.54/96
	if width > 0 && wcm > 0 {
		hcm = hcm * width / wcm
		wcm = width
	}
	if wcm > x.textCm {
		hcm = hcm * x.textCm / wcm
		wcm = x.textCm
	}
	x.frames++
	x.s(`<draw:frame draw:style-name="FrPic" draw:name="Picture` + strconv.Itoa(x.frames) + `" text:anchor-type="as-char" svg:width="` + cm(wcm) + `" svg:height="` + cm(hcm) + `" draw:z-index="0">`)
	x.s(`<draw:image xlink:href="Pictures/` + n.Asset + `" xlink:type="simple" xlink:show="embed" xlink:actuate="onLoad"/>`)
	if alt := oneLine(n.Alt); alt != "" {
		x.s(`<svg:title>` + xmlText(alt) + `</svg:title>`)
	}
	x.s(`</draw:frame>`)
}

func cm(v float64) string { return strconv.FormatFloat(max(v, 0.01), 'f', 3, 64) + "cm" }

// contentHead is content.xml up to the body: the fonts and the automatic
// styles the body used.
func (x *odtWriter) contentHead() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<office:document-content ` + odfNS + `>`)
	b.WriteString(odfFontDecls)
	b.WriteString(`<office:automatic-styles>`)
	for _, p := range x.textOrder {
		b.WriteString(`<style:style style:name="` + x.textStyles[p] + `" style:family="text"><style:text-properties`)
		if p.bold {
			b.WriteString(` fo:font-weight="bold" style:font-weight-asian="bold" style:font-weight-complex="bold"`)
		}
		if p.italic {
			b.WriteString(` fo:font-style="italic" style:font-style-asian="italic" style:font-style-complex="italic"`)
		}
		if p.strk {
			b.WriteString(` style:text-line-through-style="solid" style:text-line-through-type="single"`)
		}
		if p.style == "code" {
			b.WriteString(` style:font-name="Noto Sans Mono" fo:background-color="#f5f5f4"`)
		}
		b.WriteString(`/></style:style>`)
	}
	b.WriteString(`<style:style style:name="P_PageBreak" style:family="paragraph" style:parent-style-name="Standard"><style:paragraph-properties fo:break-after="page"/></style:style>`)
	for i := 0; i <= 8; i++ {
		fmt.Fprintf(&b, `<style:style style:name="P_Toc%d" style:family="paragraph" style:parent-style-name="Contents_20_Entry"><style:paragraph-properties fo:margin-left="%.2fcm"/></style:style>`, i, 0.64*float64(i))
	}
	for _, base := range []string{"Table_20_Contents", "Table_20_Heading"} {
		for _, al := range []struct{ suffix, align string }{{"_Center", "center"}, {"_Right", "end"}} {
			b.WriteString(`<style:style style:name="` + base + al.suffix + `" style:family="paragraph" style:parent-style-name="` + base + `"><style:paragraph-properties fo:text-align="` + al.align + `"/></style:style>`)
		}
	}
	b.WriteString(`<style:style style:name="Tbl" style:family="table"><style:table-properties style:width="` + cm(x.textCm) + `" table:align="left" fo:margin-top="0.2cm" fo:margin-bottom="0.2cm"/></style:style>`)
	b.WriteString(`<style:style style:name="TblCell" style:family="table-cell"><style:table-cell-properties fo:padding="0.1cm" fo:border="0.5pt solid #d6d3d1"/></style:style>`)
	b.WriteString(`<style:style style:name="FrPic" style:family="graphic"><style:graphic-properties style:vertical-pos="top" style:vertical-rel="baseline"/></style:style>`)
	b.WriteString(odtListStyles)
	b.WriteString(`</office:automatic-styles><office:body><office:text>`)
	return b.String()
}

const odfFontDecls = `<office:font-face-decls>` +
	`<style:font-face style:name="Noto Serif" svg:font-family="'Noto Serif'" style:font-family-generic="roman" style:font-pitch="variable"/>` +
	`<style:font-face style:name="Noto Sans" svg:font-family="'Noto Sans'" style:font-family-generic="swiss" style:font-pitch="variable"/>` +
	`<style:font-face style:name="Noto Sans Mono" svg:font-family="'Noto Sans Mono'" style:font-family-generic="modern" style:font-pitch="fixed"/>` +
	`</office:font-face-decls>`

// odtListStyles are the two list kinds, ten levels each, a bullet list and
// a decimal one.
var odtListStyles = func() string {
	var b strings.Builder
	for _, kind := range []string{"L_Bullet", "L_Number"} {
		b.WriteString(`<text:list-style style:name="` + kind + `">`)
		for l := 1; l <= 10; l++ {
			ind := fmt.Sprintf(`<style:list-level-properties text:list-level-position-and-space-mode="label-alignment"><style:list-level-label-alignment text:label-followed-by="listtab" text:list-tab-stop-position="%.2fcm" fo:text-indent="-0.64cm" fo:margin-left="%.2fcm"/></style:list-level-properties>`, 1.27*float64(l), 1.27*float64(l))
			if kind == "L_Number" {
				fmt.Fprintf(&b, `<text:list-level-style-number text:level="%d" style:num-suffix="." style:num-format="1">%s</text:list-level-style-number>`, l, ind)
			} else {
				fmt.Fprintf(&b, `<text:list-level-style-bullet text:level="%d" text:bullet-char="%s">%s</text:list-level-style-bullet>`, l, []string{"•", "◦", "▪"}[(l-1)%3], ind)
			}
		}
		b.WriteString(`</text:list-style>`)
	}
	return b.String()
}()

// styles is styles.xml: the built-in look (D13–D15), Noto Sans for text and
// headings and Noto Sans Mono for code, named as LibreOffice names its own
// styles, which is what the extractor reads.
func (x *odtWriter) styles() string {
	pw, ph := pageCm(x.opts.PageSize)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<office:document-styles ` + odfNS + `>`)
	b.WriteString(odfFontDecls)
	b.WriteString(`<office:styles>`)
	st := x.d.Style
	font, size, textColor := "Noto Sans", "10.5pt", ""
	if st != nil {
		if st.serif() {
			font = "Noto Serif"
		}
		size = typNum(st.BaseSize()) + "pt"
		textColor = ` fo:color="` + st.textColor() + `"`
	}
	b.WriteString(`<style:default-style style:family="paragraph"><style:paragraph-properties fo:margin-bottom="0.21cm" fo:line-height="115%"/><style:text-properties style:font-name="` + font + `" fo:font-size="` + size + `"` + textColor + `/></style:default-style>`)
	b.WriteString(`<style:style style:name="Standard" style:family="paragraph" style:class="text"/>`)
	b.WriteString(`<style:style style:name="Text_20_body" style:display-name="Text body" style:family="paragraph" style:parent-style-name="Standard" style:class="text"/>`)
	b.WriteString(`<style:style style:name="Title" style:family="paragraph" style:parent-style-name="Standard" style:next-style-name="Text_20_body" style:class="chapter"><style:paragraph-properties fo:margin-bottom="0.14cm"/><style:text-properties fo:font-size="22pt" fo:font-weight="bold"/></style:style>`)
	b.WriteString(`<style:style style:name="Subtitle" style:family="paragraph" style:parent-style-name="Standard" style:next-style-name="Text_20_body" style:class="chapter"><style:paragraph-properties fo:margin-bottom="0.64cm"/><style:text-properties fo:color="#5b5b5b" fo:font-size="9pt"/></style:style>`)
	headColor := ""
	if st != nil {
		headColor = ` fo:color="` + st.headingColor() + `"`
	}
	for i, pt := range []float64{16, 14, 12, 11, 10.5, 10.5} {
		n := strconv.Itoa(i + 1)
		sz := typNum(pt) + "pt"
		if st != nil {
			sz = strconv.FormatFloat(pt*st.headScale(), 'f', 1, 64) + "pt"
		}
		b.WriteString(`<style:style style:name="Heading_20_` + n + `" style:display-name="Heading ` + n + `" style:family="paragraph" style:parent-style-name="Standard" style:next-style-name="Text_20_body" style:default-outline-level="` + n + `" style:class="text"><style:paragraph-properties fo:margin-top="0.42cm" fo:margin-bottom="0.14cm" fo:keep-with-next="always"/><style:text-properties fo:font-size="` + sz + `" fo:font-weight="bold"` + headColor + `/></style:style>`)
	}
	b.WriteString(`<style:style style:name="Quotations" style:family="paragraph" style:parent-style-name="Standard" style:class="html"><style:paragraph-properties fo:margin-left="1cm" fo:padding-left="0.3cm" fo:border-left="1.5pt solid #d6d3d1"/><style:text-properties fo:color="#44403c"/></style:style>`)
	b.WriteString(`<style:style style:name="Preformatted_20_Text" style:display-name="Preformatted Text" style:family="paragraph" style:parent-style-name="Standard" style:class="html"><style:paragraph-properties fo:background-color="#f5f5f4" fo:padding="0.15cm"/><style:text-properties style:font-name="Noto Sans Mono" fo:font-size="9pt"/></style:style>`)
	b.WriteString(`<style:style style:name="Caption" style:family="paragraph" style:parent-style-name="Standard" style:class="extra"><style:text-properties fo:color="#5b5b5b" fo:font-size="8pt" fo:font-style="italic"/></style:style>`)
	b.WriteString(`<style:style style:name="List_20_Contents" style:display-name="List Contents" style:family="paragraph" style:parent-style-name="Standard" style:class="html"><style:paragraph-properties fo:margin-bottom="0.1cm"/></style:style>`)
	b.WriteString(`<style:style style:name="Table_20_Contents" style:display-name="Table Contents" style:family="paragraph" style:parent-style-name="Standard" style:class="extra"><style:paragraph-properties fo:margin-bottom="0cm"/></style:style>`)
	b.WriteString(`<style:style style:name="Table_20_Heading" style:display-name="Table Heading" style:family="paragraph" style:parent-style-name="Table_20_Contents" style:class="extra"><style:text-properties fo:font-weight="bold"/></style:style>`)
	b.WriteString(`<style:style style:name="Contents_20_Entry" style:display-name="Contents Entry" style:family="paragraph" style:parent-style-name="Standard" style:class="index"><style:paragraph-properties fo:margin-bottom="0.07cm"/></style:style>`)
	b.WriteString(`<style:style style:name="Horizontal_20_Line" style:display-name="Horizontal Line" style:family="paragraph" style:parent-style-name="Standard" style:class="html"><style:paragraph-properties fo:border-bottom="0.5pt solid #a8a29e" fo:padding="0cm"/></style:style>`)
	link := "#1d4ed8"
	if st != nil {
		link = st.linkColor()
	}
	b.WriteString(`<style:style style:name="Internet_20_link" style:display-name="Internet link" style:family="text"><style:text-properties fo:color="` + link + `" style:text-underline-style="solid" style:text-underline-width="auto" style:text-underline-color="font-color"/></style:style>`)
	b.WriteString(`<style:style style:name="Header_Footer" style:family="paragraph" style:parent-style-name="Standard"><style:paragraph-properties fo:margin-bottom="0cm"/><style:text-properties fo:color="#6e6e6e" fo:font-size="8pt"/></style:style>`)
	b.WriteString(`<style:style style:name="Status_20_Label" style:display-name="Status Label" style:family="text"><style:text-properties fo:color="#b45309" fo:font-size="8pt" fo:font-weight="bold" fo:text-transform="uppercase"/></style:style>`)
	b.WriteString(`</office:styles><office:automatic-styles>`)
	m := cm(odtMargin(st))
	b.WriteString(`<style:page-layout style:name="PM1"><style:page-layout-properties fo:page-width="` + cm(pw) + `" fo:page-height="` + cm(ph) + `" style:print-orientation="portrait" fo:margin-top="` + m + `" fo:margin-bottom="` + m + `" fo:margin-left="` + m + `" fo:margin-right="` + m + `"/></style:page-layout>`)
	b.WriteString(`</office:automatic-styles><office:master-styles><style:master-page style:name="Standard" style:page-layout-name="PM1"`)
	if h, f, n := st.header(), st.footer(), st != nil && st.pageNumbers(); h != "" || f != "" || n {
		// The style's header and footer lines, as text, and the page number
		// (text:page-number) at the footer's end.
		b.WriteString(`>`)
		if h != "" {
			b.WriteString(`<style:header><text:p text:style-name="Header_Footer">` + xmlText(h) + `</text:p></style:header>`)
		}
		if f != "" || n {
			b.WriteString(`<style:footer><text:p text:style-name="Header_Footer">` + xmlText(f))
			if n {
				if f != "" {
					b.WriteString(`<text:tab/>`)
				}
				b.WriteString(`<text:page-number text:select-page="current">1</text:page-number>`)
			}
			b.WriteString(`</text:p></style:footer>`)
		}
		b.WriteString(`</style:master-page>`)
	} else {
		b.WriteString(`/>`)
	}
	b.WriteString(`</office:master-styles></office:document-styles>`)
	return b.String()
}

// meta is meta.xml: the title, language, dates and the status, which ODF
// has no field of its own for (a user-defined property, as Word's
// contentStatus is in DOCX).
func (x *odtWriter) meta() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<office:document-meta ` + odfNS + `><office:meta>`)
	b.WriteString(`<meta:generator>loomcycle</meta:generator>`)
	b.WriteString(`<dc:title>` + xmlText(oneLine(x.d.Title)) + `</dc:title>`)
	if langRe.MatchString(x.d.Language) {
		b.WriteString(`<dc:language>` + x.d.Language + `</dc:language>`)
	}
	if dateRe.MatchString(x.d.Date) {
		b.WriteString(`<meta:creation-date>` + x.d.Date + `T00:00:00</meta:creation-date><dc:date>` + x.d.Date + `T00:00:00</dc:date>`)
	}
	if x.d.Status != "" {
		b.WriteString(`<meta:user-defined meta:name="Status" meta:value-type="string">` + xmlText(oneLine(x.d.Status)) + `</meta:user-defined>`)
	}
	b.WriteString(`</office:meta></office:document-meta>`)
	return b.String()
}

func (x *odtWriter) manifest() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3">`)
	b.WriteString(`<manifest:file-entry manifest:full-path="/" manifest:version="1.3" manifest:media-type="` + odtMediaType + `"/>`)
	for _, p := range []string{"content.xml", "styles.xml", "meta.xml"} {
		b.WriteString(`<manifest:file-entry manifest:full-path="` + p + `" manifest:media-type="text/xml"/>`)
	}
	if x.d.Embed != nil {
		b.WriteString(`<manifest:file-entry manifest:full-path="` + EmbedName + `" manifest:media-type="application/json"/>`)
	}
	for _, im := range x.d.Images {
		if m := x.media[im.Name]; m.ok {
			b.WriteString(`<manifest:file-entry manifest:full-path="Pictures/` + im.Name + `" manifest:media-type="` + im.MediaType + `"/>`)
		}
	}
	b.WriteString(`</manifest:manifest>`)
	return b.String()
}

// odtMargin is the page margin in cm: the style's, else 2.54.
func odtMargin(st *Style) float64 {
	if st == nil || st.Page.MarginMM == 0 {
		return odtMarginCm
	}
	return st.Page.MarginMM / 10
}
