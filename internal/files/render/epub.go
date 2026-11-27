package render

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

// FormatEPUB is an e-book (RFC GR P4a, D18).
const FormatEPUB = "epub"

const epubMediaType = "application/epub+zip"

// The EPUB writer builds an EPUB 3 container from the HTML writer's XHTML
// mode: one page for the title (with the root's own body) and one page per
// top-level section, nav.xhtml listing every section, the pictures as files
// and one fixed stylesheet. It writes no script and embeds no font. Every
// text is escaped as the HTML writer escapes it (G5).

const (
	epubRoot     = "OEBPS/"
	epubTextDir  = "text/"
	epubImageDir = "images/"
	epubStyle    = `body{margin:0 4%;color:#1b1b1b;font-family:"Noto Sans","Segoe UI",Calibri,Arial,sans-serif;line-height:1.5}
h1,h2,h3,h4,h5,h6{line-height:1.25;margin:1.4em 0 .5em}
h1{font-size:1.8em}h2{font-size:1.4em}h3{font-size:1.2em}h4,h5,h6{font-size:1.05em}
.status{margin-left:.5em;padding:0 .4em;border:1px solid #b45309;color:#b45309;font-size:.7em;font-weight:bold;text-transform:uppercase}
.meta{color:#5b5b5b;font-size:.85em}
code,pre{font-family:"Noto Sans Mono",Consolas,"Courier New",monospace;font-size:.9em}
pre{background:#f5f5f4;padding:.6em .8em;white-space:pre-wrap}
blockquote{margin:1em 0;padding:0 1em;border-left:3px solid #d6d3d1;color:#44403c}
table{border-collapse:collapse;margin:1em 0}
th,td{border:1px solid #d6d3d1;padding:.3em .5em;vertical-align:top}th{background:#f5f5f4}
img{max-width:100%;height:auto}
figure.diagram figcaption{color:#5b5b5b;font-size:.8em}
nav ol{list-style:none;padding-left:1.1em}
`
)

// epubPages maps every anchor of the Doc to the page that holds it, so a
// link to a part of the document points at the right file.
type epubPages struct {
	pageOf map[string]string // anchor → "page.xhtml" (pages share a folder)
}

// epubPage is one XHTML page of the book.
type epubPage struct {
	file  string // e.g. s001.xhtml
	title string
	sec   *Section // nil for the title page
}

// writeEPUB writes d as an EPUB 3 book. The zip streams into a capped writer,
// so MaxOutput holds on the compressed file as it is written.
func writeEPUB(ctx context.Context, w io.Writer, d *Doc, opts Options) error {
	cw := &capWriter{w: w, ctx: ctx}
	zw := zip.NewWriter(cw)

	pages := []epubPage{{file: "title.xhtml", title: oneLine(d.Title)}}
	book := &epubPages{pageOf: map[string]string{}}
	if d.Anchor != "" {
		book.pageOf[d.Anchor] = pages[0].file
	}
	for i, s := range d.Sections {
		p := epubPage{file: fmt.Sprintf("s%03d.xhtml", i+1), title: oneLine(s.Title), sec: s}
		pages = append(pages, p)
		book.mapAnchors(s, p.file, 0)
	}

	// The OCF container: mimetype first, stored, with no extra field.
	if err := storedEntry(zw, "mimetype", []byte(epubMediaType)); err != nil {
		return firstErr(cw.err, err)
	}
	lang := d.Language
	if !langRe.MatchString(lang) {
		lang = "und"
	}
	parts := []struct{ name, body string }{
		{"META-INF/container.xml", epubContainer},
		{epubRoot + "content.opf", epubOPF(d, pages, lang)},
		{epubRoot + "nav.xhtml", epubNav(d, pages, book, lang)},
		{epubRoot + "style.css", epubStyle + d.Style.cssOverrides("body")},
	}
	if d.Embed != nil {
		// In META-INF, the container's own folder: data for loomcycle's
		// import, not a resource of the publication a reader shows (D21).
		js, err := d.Embed.JSON()
		if err != nil {
			return firstErr(cw.err, err)
		}
		parts = append(parts, struct{ name, body string }{"META-INF/" + EmbedName, string(js)})
	}
	for _, p := range parts {
		body := p.body
		if err := entry(zw, p.name, func(ew io.Writer) error {
			_, err := io.WriteString(ew, body)
			return err
		}); err != nil {
			return firstErr(cw.err, err)
		}
	}
	for _, p := range pages {
		if err := entry(zw, epubRoot+epubTextDir+p.file, func(ew io.Writer) error {
			pw := &capWriter{w: ew, ctx: ctx}
			h := &htmlWriter{w: pw, d: d, book: book}
			h.page(p, opts, lang)
			return pw.err
		}); err != nil {
			return firstErr(cw.err, err)
		}
	}
	for _, im := range d.Images {
		img, ok := d.image(im.Name)
		if !ok {
			continue
		}
		if err := entry(zw, epubRoot+epubImageDir+img.Name, func(ew io.Writer) error {
			_, err := ew.Write(img.Data)
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

// mapAnchors records the page of a section and of every section under it.
func (b *epubPages) mapAnchors(s *Section, file string, depth int) {
	if depth > maxDepth {
		return
	}
	if s.Anchor != "" {
		b.pageOf[s.Anchor] = file
	}
	for _, c := range s.Children {
		b.mapAnchors(c, file, depth+1)
	}
}

// storedEntry writes an uncompressed entry whose local header carries its
// sizes and no extra field, as OCF requires of mimetype. The modification
// time is the bundle epoch in DOS form, set directly so the writer adds no
// extended-timestamp field.
func storedEntry(zw *zip.Writer, name string, data []byte) error {
	fh := &zip.FileHeader{
		Name:               name,
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(data),
		CompressedSize64:   uint64(len(data)),
		UncompressedSize64: uint64(len(data)),
		ModifiedDate:       (2000-1980)<<9 | 1<<5 | 1, // 2000-01-01, bundleEpoch
	}
	ew, err := zw.CreateRaw(fh)
	if err != nil {
		return err
	}
	_, err = ew.Write(data)
	return err
}

const epubContainer = `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>
`

// epubID is the book's identifier: stable for the same document, so the
// same Doc writes the same bytes (G6).
func epubID(d *Doc) string {
	key := d.Anchor
	if key == "" {
		key = d.Title
	}
	sum := sha256.Sum256([]byte(key))
	return "urn:loomcycle:document:" + hex.EncodeToString(sum[:16])
}

func epubOPF(d *Doc, pages []epubPage, lang string) string {
	modified := "2000-01-01T00:00:00Z"
	if dateRe.MatchString(d.Date) {
		modified = d.Date + "T00:00:00Z"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid" xml:lang="` + lang + `"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/">`)
	b.WriteString(`<dc:identifier id="uid">` + epubID(d) + `</dc:identifier>`)
	b.WriteString(`<dc:title>` + xmlText(oneLine(d.Title)) + `</dc:title>`)
	b.WriteString(`<dc:language>` + lang + `</dc:language>`)
	b.WriteString(`<dc:creator>loomcycle</dc:creator>`)
	if dateRe.MatchString(d.Date) {
		b.WriteString(`<dc:date>` + d.Date + `</dc:date>`)
	}
	b.WriteString(`<meta property="dcterms:modified">` + modified + `</meta>`)
	b.WriteString(`</metadata><manifest>`)
	b.WriteString(`<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>`)
	b.WriteString(`<item id="css" href="style.css" media-type="text/css"/>`)
	for i, p := range pages {
		b.WriteString(fmt.Sprintf(`<item id="p%d" href="%s%s" media-type="application/xhtml+xml"/>`, i, epubTextDir, p.file))
	}
	for i, im := range d.Images {
		if _, ok := d.image(im.Name); !ok {
			continue
		}
		b.WriteString(fmt.Sprintf(`<item id="img%d" href="%s%s" media-type="%s"/>`, i+1, epubImageDir, im.Name, im.MediaType))
	}
	b.WriteString(`</manifest><spine>`)
	for i := range pages {
		b.WriteString(fmt.Sprintf(`<itemref idref="p%d"/>`, i))
	}
	b.WriteString(`</spine></package>` + "\n")
	return b.String()
}

// epubNav is the book's table of contents: the title page, then every
// section at its depth.
func epubNav(d *Doc, pages []epubPage, book *epubPages, lang string) string {
	var b strings.Builder
	b.WriteString(xhtmlHead(lang, "Contents", ""))
	b.WriteString(`<nav epub:type="toc" id="toc"><h1>Contents</h1><ol>`)
	b.WriteString(`<li><a href="` + epubTextDir + pages[0].file + `">` + xmlText(pages[0].title) + `</a></li>`)
	var walk func(ss []*Section, depth int)
	walk = func(ss []*Section, depth int) {
		if depth > maxDepth {
			return
		}
		for _, s := range ss {
			b.WriteString(`<li><a href="` + epubTextDir + xmlText(book.pageOf[s.Anchor]+"#"+s.Anchor) + `">` + xmlText(navTitle(s.Title)) + `</a>`)
			if len(s.Children) > 0 {
				b.WriteString(`<ol>`)
				walk(s.Children, depth+1)
				b.WriteString(`</ol>`)
			}
			b.WriteString(`</li>`)
		}
	}
	walk(d.Sections, 0)
	b.WriteString(`</ol></nav></body></html>` + "\n")
	return b.String()
}

// navTitle is a section's title in the contents, never empty: EPUB requires
// text in each entry.
func navTitle(t string) string {
	if t = oneLine(t); t != "" {
		return t
	}
	return "Section"
}

// xhtmlHead opens an XHTML page, through the start of its body. css is the
// path to the stylesheet from the page, "" for none.
func xhtmlHead(lang, title, css string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<!DOCTYPE html>\n")
	b.WriteString(`<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="` + lang + `" xml:lang="` + lang + `">` + "\n")
	b.WriteString("<head>\n<meta charset=\"utf-8\"/>\n<title>" + xmlText(title) + "</title>\n")
	if css != "" {
		b.WriteString(`<link rel="stylesheet" type="text/css" href="` + css + `"/>` + "\n")
	}
	b.WriteString("</head>\n<body>\n")
	return b.String()
}

// page writes one XHTML page of the book: the title page carries the title,
// the status label, the provenance and date line, an inline contents list
// when asked, and the root's own body; a section page carries its section.
func (h *htmlWriter) page(p epubPage, opts Options, lang string) {
	d := h.d
	h.s(xhtmlHead(lang, p.title, "../style.css"))
	h.webLine("style-header", false, h.esc)
	end := func() {
		h.webLine("style-footer", true, h.esc)
		h.s("</body>\n</html>\n")
	}
	if p.sec != nil {
		h.section(p.sec)
		end()
		return
	}
	h.s("<header")
	if d.Anchor != "" {
		h.s(` id="` + h.esc(d.Anchor) + `"`)
	}
	h.s(">\n")
	h.logo()
	h.s("<h1>" + h.esc(oneLine(d.Title)))
	if l := statusLabel(d.Status); l != "" {
		h.s(` <span class="status">` + l + `</span>`)
	}
	h.s("</h1>\n")
	var meta []string
	if d.Provenance != "" {
		meta = append(meta, h.esc(oneLine(d.Provenance)))
	}
	if d.Date != "" {
		meta = append(meta, h.esc(d.Date))
	}
	if len(meta) > 0 {
		h.s(`<p class="meta">` + strings.Join(meta, " · ") + "</p>\n")
	}
	h.s("</header>\n")
	if opts.TOC && len(d.Sections) > 0 {
		h.s("<nav class=\"toc\">\n")
		h.toc(d.Sections)
		h.s("</nav>\n")
	}
	h.blocks(d.Blocks, 0)
	end()
}
