package render

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"image"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// htmlCSP is the page's Content-Security-Policy: nothing is fetched and no
// script runs; the style is inline and the pictures are data URLs.
const htmlCSP = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

// htmlStyle is the built-in look (D13–D15): Noto Sans for text and headings,
// Noto Sans Mono for code, falling back to the reader's own sans.
const htmlStyle = `body{margin:0;background:#fff;color:#1b1b1b}
article{max-width:46rem;margin:0 auto;padding:2.5rem 1.25rem;font-family:"Noto Sans","Segoe UI",Calibri,Arial,sans-serif;font-size:1rem;line-height:1.55}
h1,h2,h3,h4,h5,h6{line-height:1.25;margin:1.6em 0 .5em}
h1{font-size:2rem;margin-top:0}h2{font-size:1.5rem}h3{font-size:1.25rem}h4,h5,h6{font-size:1.05rem}
.status{display:inline-block;vertical-align:middle;margin-left:.6rem;padding:.1rem .45rem;border:1px solid #b45309;border-radius:.25rem;color:#b45309;font-size:.75rem;font-weight:600;letter-spacing:.04em;text-transform:uppercase}
.meta{color:#5b5b5b;font-size:.875rem;margin:-.25rem 0 1.5rem}
.title-page{min-height:80vh;display:flex;flex-direction:column;justify-content:center}
nav.toc{margin:0 0 2rem}nav.toc ul{list-style:none;padding-left:1.1rem}nav.toc>ul{padding-left:0}
code,pre{font-family:"Noto Sans Mono",Consolas,"Courier New",monospace;font-size:.9em}
pre{background:#f5f5f4;padding:.75rem 1rem;overflow-x:auto;white-space:pre-wrap;border-radius:.25rem}
code{background:#f5f5f4;padding:.05rem .25rem;border-radius:.2rem}pre code{background:none;padding:0}
blockquote{margin:1rem 0;padding:.1rem 1rem;border-left:3px solid #d6d3d1;color:#44403c}
table{border-collapse:collapse;margin:1rem 0;display:block;overflow-x:auto}
th,td{border:1px solid #d6d3d1;padding:.35rem .6rem;vertical-align:top}th{background:#f5f5f4}
img{max-width:100%;height:auto}
figure.diagram figcaption{color:#5b5b5b;font-size:.8rem}
@media print{article{max-width:none;padding:0}.title-page{break-after:page;min-height:auto}h1,h2,h3{break-after:avoid}}
`

// writeHTML writes the Doc as one self-contained HTML file. Every text is
// escaped; the only markup is the writer's own.
func writeHTML(ctx context.Context, w io.Writer, d *Doc, opts Options) error {
	h := &htmlWriter{w: &capWriter{w: w, ctx: ctx}, d: d}
	lang := d.Language
	if !langRe.MatchString(lang) {
		lang = ""
	}
	h.s("<!DOCTYPE html>\n<html")
	if lang != "" {
		h.s(` lang="` + lang + `"`)
	}
	h.s(">\n<head>\n<meta charset=\"utf-8\">\n")
	h.s(`<meta http-equiv="Content-Security-Policy" content="` + htmlCSP + `">` + "\n")
	h.s(`<meta name="viewport" content="width=device-width, initial-scale=1">` + "\n")
	h.s(`<meta name="generator" content="loomcycle">` + "\n")
	if d.Status != "" {
		h.s(`<meta name="document-status" content="` + esc(oneLine(d.Status)) + `">` + "\n")
	}
	if d.Date != "" {
		h.s(`<meta name="date" content="` + esc(d.Date) + `">` + "\n")
	}
	h.s("<title>" + esc(oneLine(d.Title)) + "</title>\n<style>\n" + htmlStyle + d.Style.cssOverrides("article") + "</style>\n</head>\n<body>\n<article>\n")
	if l := d.Style.header(); l != "" {
		h.s(`<p class="style-header">` + esc(l) + "</p>\n")
	}

	h.s("<header")
	if opts.TitlePage {
		h.s(` class="title-page"`)
	}
	if d.Anchor != "" {
		h.s(` id="` + esc(d.Anchor) + `"`)
	}
	h.s(">\n")
	h.logo()
	h.s("<h1>" + esc(oneLine(d.Title)))
	if l := statusLabel(d.Status); l != "" {
		h.s(` <span class="status">` + l + `</span>`)
	}
	h.s("</h1>\n")
	var meta []string
	if d.Provenance != "" {
		meta = append(meta, esc(oneLine(d.Provenance)))
	}
	if d.Date != "" {
		meta = append(meta, esc(d.Date))
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
	for _, s := range d.Sections {
		h.section(s)
	}
	if l := d.Style.footer(); l != "" {
		h.s(`<p class="style-footer">` + esc(l) + "</p>\n")
	}
	h.s("</article>\n")
	if d.Embed != nil {
		js, err := d.Embed.JSON()
		if err != nil {
			return err
		}
		// Escaped text in a hidden element: the page runs no script, and
		// the record is data for loomcycle's import, not for the reader.
		h.s(`<pre id="loomcycle-metadata" hidden>` + esc(string(js)) + "</pre>\n")
	}
	h.s("</body>\n</html>\n")
	return h.w.err
}

// langRe is a language tag safe to put in an attribute as it is.
var langRe = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)

type htmlWriter struct {
	w *capWriter
	d *Doc
	// book, when set, writes XHTML for an EPUB page: void elements closed,
	// text escaped as XML 1.0 requires, pictures as files in the container
	// and links to a part of the document by the page that holds it.
	book *epubPages
}

func (h *htmlWriter) s(s string) { h.w.WriteString(s) }

func esc(s string) string { return html.EscapeString(s) }

// esc escapes text for the page being written. XHTML is XML, which has no
// place for most control characters, so they are replaced there.
func (h *htmlWriter) esc(s string) string {
	if h.book != nil {
		return xmlText(s)
	}
	return esc(s)
}

// void closes a void element: "<br>" in HTML, "<br/>" in XHTML.
func (h *htmlWriter) void() string {
	if h.book != nil {
		return "/>"
	}
	return ">"
}

func (h *htmlWriter) toc(ss []*Section) {
	h.s("<ul>\n")
	for _, s := range ss {
		h.s(`<li><a href="` + h.esc(h.anchorHref(s.Anchor)) + `">` + h.esc(oneLine(s.Title)) + "</a>")
		if len(s.Children) > 0 {
			h.s("\n")
			h.toc(s.Children)
		}
		h.s("</li>\n")
	}
	h.s("</ul>\n")
}

func (h *htmlWriter) section(s *Section) {
	lvl := strconv.Itoa(clampLevel(s.Level))
	h.s(`<section id="` + h.esc(s.Anchor) + `"`)
	if h.d.Embed != nil && embedIDRe.MatchString(s.ID) {
		h.s(` data-loom-chunk="` + s.ID + `"`)
	}
	h.s(">\n<h" + lvl + ">" + h.esc(oneLine(s.Title)) + "</h" + lvl + ">\n")
	h.blocks(s.Blocks, 0)
	for _, c := range s.Children {
		h.section(c)
	}
	h.s("</section>\n")
}

func (h *htmlWriter) blocks(ns []Node, depth int) {
	for _, n := range ns {
		h.block(n, depth)
	}
}

func (h *htmlWriter) block(n Node, depth int) {
	if depth > maxDepth {
		return
	}
	switch n.Kind {
	case KindParagraph:
		h.s("<p>")
		h.inlines(n.Children, 0)
		h.s("</p>\n")
	case KindHeading:
		lvl := strconv.Itoa(clampLevel(n.Level))
		h.s("<h" + lvl + ">")
		h.inlines(n.Children, 0)
		h.s("</h" + lvl + ">\n")
	case KindRule:
		h.s("<hr" + h.void() + "\n")
	case KindCode:
		h.s("<pre><code")
		if l := oneLine(n.Lang); l != "" {
			h.s(` class="language-` + h.esc(strings.ReplaceAll(l, " ", "-")) + `"`)
		}
		h.s(">" + h.esc(n.Text) + "</code></pre>\n")
	case KindDiagram:
		h.s(`<figure class="diagram"><pre><code>` + h.esc(n.Text) + "</code></pre><figcaption>" + h.esc(oneLine(n.Lang)) + " diagram</figcaption></figure>\n")
	case KindImage:
		h.s("<p>")
		h.image(n)
		h.s("</p>\n")
	case KindQuote:
		h.s("<blockquote>\n")
		h.blocks(n.Children, depth+1)
		h.s("</blockquote>\n")
	case KindList:
		if n.Ordered {
			h.s("<ol")
			if n.Start > 1 {
				h.s(` start="` + strconv.Itoa(n.Start) + `"`)
			}
			h.s(">\n")
		} else {
			h.s("<ul>\n")
		}
		for _, it := range n.Children {
			h.s("<li>")
			// A tight item's single paragraph reads as the item's text.
			if len(it.Children) == 1 && it.Children[0].Kind == KindParagraph {
				h.inlines(it.Children[0].Children, 0)
			} else {
				h.s("\n")
				h.blocks(it.Children, depth+1)
			}
			h.s("</li>\n")
		}
		if n.Ordered {
			h.s("</ol>\n")
		} else {
			h.s("</ul>\n")
		}
	case KindTable:
		h.s("<table>\n")
		for _, r := range n.Children {
			tag := "td"
			if r.Header {
				tag = "th"
			}
			h.s("<tr>")
			for i, c := range r.Children {
				h.s("<" + tag)
				if i < len(n.Align) && n.Align[i] != "" {
					switch n.Align[i] {
					case "left", "center", "right":
						h.s(` style="text-align:` + n.Align[i] + `"`)
					}
				}
				h.s(">")
				h.inlines(c.Children, 0)
				h.s("</" + tag + ">")
			}
			h.s("</tr>\n")
		}
		h.s("</table>\n")
	}
}

// inlines writes inline nodes straight to the capped writer, so a paragraph
// is never built whole in memory: one that names a picture thousands of
// times stops at MaxOutput rather than after it was assembled.
func (h *htmlWriter) inlines(ns []Node, depth int) {
	if depth > maxDepth {
		return
	}
	for _, n := range ns {
		if h.w.err != nil {
			return
		}
		switch n.Kind {
		case KindText:
			h.s(h.esc(n.Text))
		case KindBreak:
			h.s("<br" + h.void() + "\n")
		case KindEmph, KindStrong, KindStrike:
			tag := map[string]string{KindEmph: "em", KindStrong: "strong", KindStrike: "del"}[n.Kind]
			h.s("<" + tag + ">")
			h.inlines(n.Children, depth+1)
			h.s("</" + tag + ">")
		case KindCodeSpan:
			h.s("<code>" + h.esc(n.Text) + "</code>")
		case KindLink:
			href, ok := h.safeHref(n)
			if !ok {
				h.inlines(n.Children, depth+1)
				continue
			}
			h.s(`<a href="` + h.esc(href) + `"`)
			if n.Anchor == "" {
				h.s(` rel="noopener noreferrer"`)
			}
			h.s(">")
			h.inlines(n.Children, depth+1)
			h.s("</a>")
		case KindImage:
			h.image(n)
		}
	}
}

// safeHref is a link's target, checked again here: the Doc may come from a
// bundle, and only an anchor or an http, https or mailto URL is written. In
// an EPUB an anchor no page holds is no link.
func (h *htmlWriter) safeHref(n Node) (string, bool) {
	if n.Anchor != "" {
		if h.book != nil && h.book.pageOf[n.Anchor] == "" {
			return "", false
		}
		return h.anchorHref(n.Anchor), true
	}
	if href, _, ok := ExternalOnly(n.Href); ok {
		return href, true
	}
	return "", false
}

// anchorHref is where a link to an anchor of the document points: the
// anchor on this page in HTML, the page that holds it in an EPUB.
func (h *htmlWriter) anchorHref(anchor string) string {
	if h.book != nil {
		return h.book.pageOf[anchor] + "#" + anchor
	}
	return "#" + anchor
}

// image writes a picture as a data URL, base64-encoding it into the capped
// writer as it goes. In an EPUB it is a file in the container.
func (h *htmlWriter) image(n Node) {
	img, ok := h.d.image(n.Asset)
	if !ok {
		h.s(h.esc("[" + n.Alt + "]"))
		return
	}
	if h.book != nil {
		cls := ""
		if h.d.Style != nil && img.Name == h.d.Style.Logo && n.Alt == "logo" {
			cls = ` class="logo"`
		}
		h.s(`<img` + cls + ` src="../` + epubImageDir + img.Name + `"` + shownSize(img) + ` alt="` + h.esc(oneLine(n.Alt)) + `"/>`)
		return
	}
	h.s(`<img` + shownSize(img))
	if h.d.Style != nil && img.Name == h.d.Style.Logo && n.Alt == "logo" {
		h.s(` class="logo"`)
	}
	h.s(` src="data:` + img.MediaType + `;base64,`)
	if h.w.err == nil {
		enc := base64.NewEncoder(base64.StdEncoding, h.w)
		_, _ = enc.Write(img.Data)
		_ = enc.Close()
	}
	h.s(`" alt="` + esc(oneLine(n.Alt)) + `">`)
}

// image finds a picture by name.
func (d *Doc) image(name string) (Image, bool) {
	for _, im := range d.Images {
		if im.Name == name {
			if _, ok := imageExt[im.MediaType]; ok {
				return im, true
			}
			return Image{}, false
		}
	}
	return Image{}, false
}

// logo writes the style's picture above the title, when it has one.
func (h *htmlWriter) logo() {
	im, ok := h.d.logo()
	if !ok {
		return
	}
	h.image(Node{Kind: KindImage, Asset: im.Name, Alt: "logo"})
}

// shownSize is a 2× picture's width and height attributes, so a page shows
// it at half its pixel size; "" for any other picture, shown at its own.
func shownSize(im Image) string {
	if im.Density != 2 {
		return ""
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(im.Data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return ""
	}
	w, h := im.shown(cfg.Width, cfg.Height)
	return fmt.Sprintf(` width="%d" height="%d"`, w, h)
}
