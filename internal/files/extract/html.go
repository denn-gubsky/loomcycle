package extract

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// maxHTMLBytes caps the HTML a document may hold: the parser builds the
// whole tree in memory.
const maxHTMLBytes = 32 << 20

// HTML reads a web page: its headings, paragraphs, lists, tables and
// preformatted blocks. Scripts, styles and forms are dropped. An <img> is
// written as a link to where it points; nothing is fetched.
type HTML struct{}

func (HTML) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"text/html", "application/xhtml+xml"}, []string{".html", ".htm", ".xhtml"})
}

func (HTML) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "html"}
	if size > maxHTMLBytes {
		return rep, refusedf("the page is over %d bytes", maxHTMLBytes)
	}
	doc, err := html.Parse(io.NewSectionReader(r, 0, size))
	if err != nil {
		return rep, err
	}
	w := newWriter(ctx, sink, opts)
	h := &htmlState{w: w, rep: &rep}
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
	if err := h.walk(body, 0); err != nil {
		return rep, err
	}
	h.flush()
	if err := w.text(strings.Join(h.blocks, "\n\n")); err != nil {
		rep.finish(w)
		return rep, err
	}
	rep.finish(w)
	return rep, nil
}

type htmlState struct {
	w      *writer
	rep    *Report
	blocks []string
	inline strings.Builder // text of the paragraph being collected
	nodes  int
}

// flush ends the paragraph being collected.
func (h *htmlState) flush() {
	if p := paragraph(collapseSpace(h.inline.String())); p != "" {
		h.blocks = append(h.blocks, p)
	}
	h.inline.Reset()
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
			h.inline.WriteString(sourceSpace(c.Data))
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
			}
		case tag == "br":
			h.inline.WriteString("\n")
		case tag == "ul" || tag == "ol":
			h.flush()
			if l := h.list(c, tag == "ol", 0); l != "" {
				h.blocks = append(h.blocks, l)
			}
		case tag == "table":
			h.flush()
			if t := h.table(c); t != "" {
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
			if l := imgLink(c); l != "" {
				h.blocks = append(h.blocks, l)
			}
		case htmlBlock[tag] || tag == "li":
			h.flush()
			if err := h.walk(c, depth+1); err != nil {
				return err
			}
			h.flush()
		default:
			if err := h.walk(c, depth+1); err != nil {
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
// GFM table, the first row as its header.
func (h *htmlState) table(t *html.Node) string {
	var rows [][]string
	var walk func(*html.Node, int)
	walk = func(n *html.Node, depth int) {
		if depth > 8 {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "tr":
				var row []string
				for td := c.FirstChild; td != nil; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.Data == "td" || td.Data == "th") {
						row = append(row, cell(collapseSpace(textOf(td))))
					}
				}
				rows = append(rows, row)
			case "thead", "tbody", "tfoot":
				walk(c, depth+1)
			}
		}
	}
	walk(t, 0)
	if len(rows) == 0 {
		return ""
	}
	return table(rows)
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
