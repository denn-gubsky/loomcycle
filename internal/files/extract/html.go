package extract

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxHTMLBytes caps the HTML a document may hold: the parser builds the
// whole tree in memory, and nothing bounds that tree while it is built, so
// the page is also counted before it is parsed (countHTML).
const maxHTMLBytes = 8 << 20

// HTML reads a web page: its headings, paragraphs, lists, tables and
// preformatted blocks. Scripts, styles and forms are dropped. An <img> is
// written as a link to where it points; nothing is fetched.
type HTML struct{}

func (HTML) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"text/html", "application/xhtml+xml"}, []string{".html", ".htm", ".xhtml"})
}

func (HTML) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	return extractHTML(ctx, r, size, sink, opts, maxCSSWork)
}

// extractHTML is HTML.Extract with the bound on class rule work as a
// parameter, so a test can reach it with a small page.
func extractHTML(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options, cssBudget int) (Report, error) {
	rep := Report{Format: "html"}
	if size > maxHTMLBytes {
		return rep, refusedf("the page is over %d bytes", maxHTMLBytes)
	}
	// Both passes read through the context, so a deadline that comes while
	// either is reading stops it at its next read: html.Parse has no context
	// of its own.
	if err := countHTML(ctx, &ctxReader{ctx: ctx, r: io.NewSectionReader(r, 0, size)}); err != nil {
		return rep, err
	}
	doc, err := html.Parse(&ctxReader{ctx: ctx, r: io.NewSectionReader(r, 0, size)})
	if err != nil {
		return rep, err
	}
	w := newWriter(ctx, sink, opts)
	h := &htmlState{w: w, rep: &rep, infer: opts.InferHeadings}
	docTitle := ""
	if t := findElement(doc, "title"); t != nil {
		docTitle = strings.TrimSpace(textOf(t))
	}
	if docTitle == "" {
		docTitle = title(opts.Name, "Page")
	}
	h.blocks = append(h.blocks, heading(1, docTitle))
	body := findElement(doc, "body")
	if body == nil {
		body = doc
	}
	if h.infer {
		h.sheet = readStyleSheets(doc)
		if !h.sheet.cssWork(doc, cssBudget) {
			// Applying the page's class rules would be work out of
			// proportion to it: the page is extracted as if the option
			// were off, and says so.
			h.infer, h.sheet = false, nil
			rep.note("headings were not inferred: the page's class rules apply to more elements than %d", cssBudget)
		}
	}
	if h.infer {
		h.cur = htmlStyled(body, runFmt{size: htmlBodyPt}, h.sheet)
		h.decor = decorative{}
		budget := maxXMLNodes
		countImages(body, h.decor, &budget, 0)
	}
	if err := h.walk(body, 0); err != nil {
		return rep, err
	}
	h.flush()
	if h.infer {
		h.promote()
	}
	if err := w.text(strings.Join(h.blocks, "\n\n")); err != nil {
		rep.finish(w)
		return rep, err
	}
	rep.finish(w)
	return rep, nil
}

// htmlFormatting are the elements the parser keeps on its list of active
// formatting elements, and htmlMarkers those that open a new scope of it.
var (
	htmlFormatting = map[atom.Atom]bool{
		atom.A: true, atom.B: true, atom.Big: true, atom.Code: true, atom.Em: true, atom.Font: true, atom.I: true,
		atom.Nobr: true, atom.S: true, atom.Small: true, atom.Strike: true, atom.Strong: true, atom.Tt: true, atom.U: true,
	}
	htmlMarkers = map[atom.Atom]bool{
		atom.Applet: true, atom.Object: true, atom.Marquee: true, atom.Template: true, atom.Td: true, atom.Th: true, atom.Caption: true,
	}
)

// fmtEntry is one entry of countHTML's copy of the active formatting list:
// the element, its attributes as written (the parser keeps at most three
// entries alike), and what one clone of it costs (itself and its
// attributes). An entry with markers > 0 stands for that many scope markers
// in a row, so a flood of table cells does not grow the list.
type fmtEntry struct {
	tag     atom.Atom
	key     string
	weight  int
	markers int
}

// maxHTMLActive bounds countHTML's copy of the active formatting list. The
// parser refuses a page that nests more than 512 elements, and re-creating
// more formatting elements than that would nest them that deep.
const maxHTMLActive = 512

// countHTML reads the page with the tokenizer, which keeps one tag at a time,
// and refuses it before html.Parse builds anything when the tree would be out
// of proportion to the page: more than maxXMLNodes elements and text runs, as
// the walk counts them (whose table walk keeps a row's first maxSheetColumns
// cells, so the cells past them were never counted while the parser built a
// node for each), an element of more than
// maxXMLAttrsPerElement attributes or more than maxXMLAttrs in all, or more
// than maxXMLNodes elements and attributes the parser would clone.
//
// The clones: a formatting element (<b>, <font>, ...) left open when an
// enclosing element closes is re-created by the parser before the next
// content, so a few hundred open <b>s and ten thousand paragraphs build
// millions of nodes from a few hundred KB of page. countHTML keeps the
// parser's list of active formatting elements the way the parser does
// (pushed on a start tag, at most three alike, removed by the matching end
// tag, scoped by markers) and charges the whole list's weight at every other
// tag, each of which may close what holds them. That over-counts a formatting
// element nothing closes (it is never cloned), never under-counts, and a page
// that closes what it opens is charged nothing.
//
// The tokenizer alone reads <script>, <style>, <title> and the like as raw
// text, but the parser has it read them as markup inside <svg> and <math>.
// So once the page has opened either, countHTML reads them as markup too: it
// may count tags the parser never sees, never miss one it builds.
//
// What the count itself costs: the tokenizer holds the attributes of the
// current tag, about 100 bytes each, so a page that is one tag of a million
// distinct attributes costs about 160 MiB before it is refused.
func countHTML(ctx context.Context, r io.Reader) error {
	z := html.NewTokenizer(r)
	var active []fmtEntry
	nodes, attrs, weight, cloned := 0, 0, 0, 0
	foreign := false
	for i := 0; ; i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if err := z.Err(); err != io.EOF {
				return err
			}
			return nil
		case html.TextToken, html.CommentToken, html.DoctypeToken:
			// Each is a node the walk counts against the same cap.
			if nodes++; nodes > maxXMLNodes {
				return refusedf("the page holds more than %d elements", maxXMLNodes)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, more := z.TagName()
			tag := atom.Lookup(name)
			if tag == atom.Svg || tag == atom.Math {
				foreign = true
			}
			if foreign {
				z.NextIsNotRawText()
			}
			formatting := htmlFormatting[tag] && tt == html.StartTagToken
			var key strings.Builder
			n := 0
			for more {
				var k, v []byte
				k, v, more = z.TagAttr()
				n++
				if formatting && key.Len() < 4096 {
					key.Write(k)
					key.WriteByte('=')
					key.Write(v)
					key.WriteByte(0)
				}
			}
			nodes++
			attrs += n
			switch {
			case n > maxXMLAttrsPerElement:
				return refusedf("an element of the page holds more than %d attributes", maxXMLAttrsPerElement)
			case attrs > maxXMLAttrs:
				return refusedf("the page holds more than %d attributes", maxXMLAttrs)
			case nodes > maxXMLNodes:
				return refusedf("the page holds more than %d elements", maxXMLNodes)
			}
			switch {
			case formatting:
				e := fmtEntry{tag: tag, key: key.String(), weight: 1 + n}
				// The parser's Noah's Ark clause: a fourth entry alike since the
				// last marker drops the earliest of them.
				alike, first := 0, -1
				for j := len(active) - 1; j >= 0 && active[j].markers == 0; j-- {
					if active[j].tag == e.tag && active[j].key == e.key {
						alike++
						first = j
					}
				}
				if alike >= 3 {
					weight -= active[first].weight
					active = append(active[:first], active[first+1:]...)
				}
				if len(active) >= maxHTMLActive {
					return refusedf("the page leaves more than %d formatting elements open", maxHTMLActive)
				}
				active = append(active, e)
				weight += e.weight
			case htmlMarkers[tag] && tt == html.StartTagToken:
				if l := len(active); l > 0 && active[l-1].markers > 0 {
					active[l-1].markers++
				} else if l >= maxHTMLActive {
					return refusedf("the page leaves more than %d formatting elements open", maxHTMLActive)
				} else {
					active = append(active, fmtEntry{markers: 1})
				}
			default:
				cloned += weight
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := atom.Lookup(name)
			switch {
			case htmlFormatting[tag]:
				for j := len(active) - 1; j >= 0 && active[j].markers == 0; j-- {
					if active[j].tag == tag {
						weight -= active[j].weight
						active = append(active[:j], active[j+1:]...)
						break
					}
				}
			case htmlMarkers[tag]:
				// Clear the list back to the last marker.
				for len(active) > 0 {
					l := len(active) - 1
					if active[l].markers > 0 {
						if active[l].markers--; active[l].markers == 0 {
							active = active[:l]
						}
						break
					}
					weight -= active[l].weight
					active = active[:l]
				}
			default:
				cloned += weight
			}
		}
		if cloned > maxXMLNodes {
			return refusedf("the page's unclosed formatting elements would be re-created more than %d times", maxXMLNodes)
		}
	}
}

type htmlState struct {
	w      *writer
	rep    *Report
	blocks []string
	inline strings.Builder // text of the paragraph being collected
	nodes  int

	// Heading recovery (Options.InferHeadings): the formatting the text
	// being walked inherits, the current paragraph's text by formatting,
	// whether it is in a list item, every paragraph and real heading
	// written with the block it became, and how often each picture appears.
	infer  bool
	cur    runFmt
	runs   []fmtRun
	inList int
	paras  []inferPara
	paraAt []int
	decor  decorative
	sheet  cssSheet
}

// flush ends the paragraph being collected.
func (h *htmlState) flush() {
	if p := paragraph(collapseSpace(h.inline.String())); p != "" {
		h.blocks = append(h.blocks, p)
		if h.infer {
			h.paras = append(h.paras, inferPara{runs: h.runs, eligible: h.inList == 0})
			h.paraAt = append(h.paraAt, len(h.blocks)-1)
		}
	}
	h.inline.Reset()
	h.runs = nil
}

// text adds text to the paragraph being collected.
func (h *htmlState) text(s string) {
	h.inline.WriteString(s)
	if h.infer {
		h.runs = append(h.runs, fmtRun{text: s, fmt: h.cur})
	}
}

// promote rewrites the paragraphs inferLevels makes headings.
func (h *htmlState) promote() {
	res := inferLevels(h.paras)
	for i, lv := range res.levels {
		if lv > 0 {
			// Below the document's "#", as a real <hN> is.
			h.blocks[h.paraAt[i]] = heading(lv+1, h.paras[i].look().text)
		}
	}
	h.rep.InferredHeadings = res.headings
	res.note(h.rep)
}

var htmlSkip = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "head": true,
	"iframe": true, "object": true, "svg": true, "math": true, "form": true, "button": true, "select": true, "textarea": true,
}

var htmlBlock = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true, "header": true, "footer": true,
	"aside": true, "nav": true, "blockquote": true, "figure": true, "figcaption": true, "address": true,
	"dl": true, "dt": true, "dd": true, "hr": true, "center": true,
}

func (h *htmlState) walk(n *html.Node, depth int) error {
	if depth > maxXMLDepth {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		h.nodes++
		if h.nodes%4096 == 0 {
			if err := h.w.ctx.Err(); err != nil {
				return err
			}
		}
		if h.nodes > maxXMLNodes {
			return refusedf("the page holds more than %d elements", maxXMLNodes)
		}
		switch c.Type {
		case html.TextNode:
			h.text(sourceSpace(c.Data))
			continue
		case html.ElementNode:
		default:
			continue
		}
		tag := c.Data
		switch {
		case htmlSkip[tag]:
		case len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6':
			h.flush()
			if t := strings.TrimSpace(textOf(c)); t != "" {
				// Below the document's "#".
				h.blocks = append(h.blocks, heading(int(tag[1]-'0')+1, t))
				if h.infer {
					h.paras = append(h.paras, inferPara{real: int(tag[1] - '0')})
					h.paraAt = append(h.paraAt, len(h.blocks)-1)
				}
			}
		case tag == "br":
			h.text("\n")
		case tag == "ul" || tag == "ol":
			h.flush()
			if l := h.list(c, tag == "ol", 0); l != "" {
				h.blocks = append(h.blocks, l)
			}
		case tag == "table":
			h.flush()
			t, err := h.table(c)
			if err != nil {
				return err
			}
			if t != "" {
				h.rep.Tables++
				h.blocks = append(h.blocks, t)
			}
		case tag == "pre":
			h.flush()
			if t := preText(c); strings.TrimSpace(t) != "" {
				h.blocks = append(h.blocks, fenced(t))
			}
		case tag == "img":
			h.flush()
			if h.infer && h.decor.is(imageSrc(c)) {
				h.rep.DroppedDecorativeImages++
				continue
			}
			if l := imgLink(c); l != "" {
				h.blocks = append(h.blocks, l)
			}
		case htmlBlock[tag] || tag == "li":
			h.flush()
			if err := h.walkStyled(c, depth, tag == "li"); err != nil {
				return err
			}
			h.flush()
		default:
			if err := h.walkStyled(c, depth, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// list renders a <ul>/<ol>, nested lists indented under their item.
func (h *htmlState) list(n *html.Node, ordered bool, level int) string {
	var lines []string
	marker := "- "
	if ordered {
		marker = "1. "
	}
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.Data != "li" {
			continue
		}
		var text strings.Builder
		var nested []string
		for c := li.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.Data == "ul" || c.Data == "ol") && level < 8 {
				if l := h.list(c, c.Data == "ol", level+1); l != "" {
					nested = append(nested, l)
				}
				continue
			}
			text.WriteString(textOf(c))
		}
		item := escapeLine(oneLine(text.String()))
		lines = append(lines, strings.Repeat("  ", level)+marker+item)
		lines = append(lines, nested...)
	}
	return strings.Join(lines, "\n")
}

// table renders a <table>'s rows (from thead, tbody and tfoot alike) as a
// GFM table, the first row as its header, held to the tableRows caps. Its
// rows and cells count toward the page's element cap, as the elements the
// page walk reads do, and the context is read at every row.
func (h *htmlState) table(t *html.Node) (string, error) {
	rows := h.w.newTable()
	defer rows.done()
	var walk func(*html.Node, int) error
	walk = func(n *html.Node, depth int) error {
		if depth > 8 {
			return nil
		}
		for c := n.FirstChild; c != nil && !rows.full; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "tr":
				if err := h.w.ctx.Err(); err != nil {
					return err
				}
				var row []string
				for td := c.FirstChild; td != nil && len(row) <= maxSheetColumns; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.Data == "td" || td.Data == "th") {
						row = append(row, cell(collapseSpace(textOf(td))))
					}
				}
				if h.nodes += 1 + len(row); h.nodes > maxXMLNodes {
					return refusedf("the page holds more than %d elements", maxXMLNodes)
				}
				rows.add(row)
			case "thead", "tbody", "tfoot":
				if err := walk(c, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(t, 0); err != nil {
		return "", err
	}
	if len(rows.rows) == 0 {
		return "", nil
	}
	return table(rows.rows), nil
}

// imgLink writes an image as a link to its source; a data: URL is not
// copied into the text.
func imgLink(n *html.Node) string {
	alt, src := strings.TrimSpace(attrOf(n, "alt")), strings.TrimSpace(attrOf(n, "src"))
	label := "[image]"
	if alt != "" {
		label = "[image: " + escapeAlt(cleanAlt(alt)) + "]"
	}
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		return label
	}
	return label + "(" + linkTarget(src) + ")"
}

// linkTarget makes a URL safe inside "(...)": it may not hold spaces or
// parentheses, which would end the link.
func linkTarget(u string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "\n", "", "\r", "", "\t", "").Replace(u)
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElement(c, tag); f != nil {
			return f
		}
	}
	return nil
}

// sourceSpace turns the line breaks of HTML source into the spaces a
// browser shows; only <br> breaks a line.
func sourceSpace(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// textOf is the text under n as a browser shows it, scripts and styles left
// out; preText keeps the source's line breaks, for <pre>.
func textOf(n *html.Node) string { return nodeText(n, sourceSpace) }

func preText(n *html.Node) string { return nodeText(n, func(s string) string { return s }) }

func nodeText(n *html.Node, space func(string) string) string {
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(m *html.Node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		switch m.Type {
		case html.TextNode:
			b.WriteString(space(m.Data))
			return
		case html.ElementNode:
			if htmlSkip[m.Data] {
				return
			}
			if m.Data == "br" {
				b.WriteString("\n")
			}
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	return b.String()
}

// collapseSpace folds runs of spaces as a browser does, keeping explicit
// line breaks.
func collapseSpace(s string) string {
	lines := strings.Split(normalizeNewlines(s), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}
