package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// LinkResolver decides where a link in a body points. It returns the external
// href to keep, or the anchor of a part of the Doc, or ok false to keep the
// link's text and drop the link. nil keeps http, https and mailto links only.
type LinkResolver func(dest string) (href, anchor string, ok bool)

// Builder builds a Doc. The core walks a Document and hands each chunk's body
// to Body, each picture to Image; the Builder parses, counts and caps.
type Builder struct {
	Doc    Doc
	Report Report
	// Links resolves a body's links; nil keeps http, https and mailto.
	Links LinkResolver

	md         goldmark.Markdown
	bodyBytes  int
	imageBytes int
	// dataURLs is each data: picture decoded so far, by its URL, so a body
	// that names one picture many times (a reference definition used over
	// and over) decodes and stores it once.
	dataURLs    map[string]Node
	full        bool // the body cap was reached: later bodies are left out
	sectionsCut bool
}

// NewBuilder starts a Doc titled title.
func NewBuilder(title string) *Builder {
	return &Builder{
		Doc: Doc{Title: oneLine(title)},
		// Raw HTML is parsed (so it can be shown as text), never passed on:
		// every node kind this package does not map becomes text.
		md: goldmark.New(goldmark.WithExtensions(east.Table, east.Strikethrough, east.Linkify, east.TaskList)),
	}
}

// AddSection counts a section toward the cap. false means the Doc holds
// MaxSections already and this one is left out.
func (b *Builder) AddSection() bool {
	if b.Report.Sections >= MaxSections {
		if !b.sectionsCut {
			b.sectionsCut = true
			b.Report.Notes = append(b.Report.Notes, fmt.Sprintf("only the first %d sections were rendered", MaxSections))
		}
		return false
	}
	b.Report.Sections++
	return true
}

// Finish returns the Doc and its report.
func (b *Builder) Finish() (*Doc, Report) { return &b.Doc, b.Report }

// Body parses one chunk's Markdown body into blocks. Past MaxBodyBytes of
// bodies in all, a body is left out and counted in bodies_cut.
func (b *Builder) Body(md string) []Node {
	if md == "" {
		return nil
	}
	if b.full || b.bodyBytes+len(md) > MaxBodyBytes {
		if !b.full {
			b.Report.Notes = append(b.Report.Notes, fmt.Sprintf("the text was cut at %d bytes", MaxBodyBytes))
		}
		b.full = true
		b.Report.BodiesCut++
		return nil
	}
	b.bodyBytes += len(md)
	src := []byte(md)
	root := b.md.Parser().Parse(text.NewReader(src))
	c := converter{b: b, src: src}
	return c.blocks(root, 0)
}

// Image adds a picture as a block. A picture of a type the writers do not
// embed, or larger than MaxImage, or past MaxImages, is left out: its alt
// text stands in its place and it is counted in images_skipped.
func (b *Builder) Image(mediaType string, data []byte, alt string) Node {
	alt = oneLine(alt)
	ext, ok := imageExt[mediaType]
	switch {
	case !ok:
		return b.skipImage(alt, "a picture of type "+mediaType+" was left out")
	case len(data) == 0:
		return b.skipImage(alt, "an empty picture was left out")
	case len(data) > MaxImage:
		return b.skipImage(alt, fmt.Sprintf("a picture larger than %d MiB was left out", MaxImage>>20))
	case len(b.Doc.Images) >= MaxImages:
		return b.skipImage(alt, fmt.Sprintf("pictures past the first %d were left out", MaxImages))
	case b.imageBytes+len(data) > MaxImageBytes:
		return b.skipImage(alt, fmt.Sprintf("pictures past %d MiB in all were left out", MaxImageBytes>>20))
	}
	b.imageBytes += len(data)
	name := fmt.Sprintf("img-%d.%s", len(b.Doc.Images)+1, ext)
	b.Doc.Images = append(b.Doc.Images, Image{Name: name, MediaType: mediaType, Data: data})
	b.Report.Images++
	return Node{Kind: KindImage, Asset: name, Alt: alt}
}

// Diagram adds a diagram's source as a block, shown as code (D10).
func (b *Builder) Diagram(lang, src string) Node {
	return Node{Kind: KindDiagram, Lang: lang, Text: src}
}

func (b *Builder) skipImage(alt, note string) Node {
	b.Report.ImagesSkipped++
	if !containsNote(b.Report.Notes, note) && len(b.Report.Notes) < 32 {
		b.Report.Notes = append(b.Report.Notes, note)
	}
	if alt == "" {
		alt = "picture"
	}
	return Node{Kind: KindParagraph, Children: []Node{{Kind: KindText, Text: "[" + alt + "]"}}}
}

func containsNote(notes []string, n string) bool {
	for _, x := range notes {
		if x == n {
			return true
		}
	}
	return false
}

// imageExt are the picture types a rendering embeds, and their extensions.
var imageExt = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// converter maps one parsed body to Nodes.
type converter struct {
	b   *Builder
	src []byte
}

// blocks maps the block children of n. Past maxDepth the nesting is cut and
// what is below it kept as plain text.
func (c *converter) blocks(n gast.Node, depth int) []Node {
	var out []Node
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		if nd, ok := c.block(ch, depth); ok {
			out = append(out, nd...)
		}
	}
	return out
}

func (c *converter) block(n gast.Node, depth int) ([]Node, bool) {
	if depth >= maxDepth {
		return []Node{{Kind: KindParagraph, Children: []Node{{Kind: KindText, Text: plainText(n, c.src)}}}}, true
	}
	switch n := n.(type) {
	case *gast.Paragraph, *gast.TextBlock:
		inl := c.inlines(n, depth)
		// A paragraph that is one picture is a picture block.
		if len(inl) == 1 && inl[0].Kind == KindImage {
			return inl, true
		}
		if len(inl) == 0 {
			return nil, false
		}
		return []Node{{Kind: KindParagraph, Children: inl}}, true
	case *gast.Heading:
		lvl := n.Level
		if lvl < 1 {
			lvl = 1
		}
		return []Node{{Kind: KindHeading, Level: lvl, Children: c.inlines(n, depth)}}, true
	case *gast.ThematicBreak:
		return []Node{{Kind: KindRule}}, true
	case *gast.FencedCodeBlock:
		lang := strings.TrimSpace(string(n.Language(c.src)))
		body := linesText(n.Lines(), c.src)
		if strings.EqualFold(lang, "mermaid") {
			return []Node{c.b.Diagram("mermaid", body)}, true
		}
		return []Node{{Kind: KindCode, Lang: oneLine(lang), Text: body}}, true
	case *gast.CodeBlock:
		return []Node{{Kind: KindCode, Text: linesText(n.Lines(), c.src)}}, true
	case *gast.HTMLBlock:
		// Shown as the text it is, never passed on as markup.
		t := linesText(n.Lines(), c.src)
		if n.HasClosure() {
			t += string(n.ClosureLine.Value(c.src))
		}
		t = strings.TrimRight(t, "\n")
		if t == "" {
			return nil, false
		}
		return []Node{{Kind: KindCode, Text: t}}, true
	case *gast.Blockquote:
		return []Node{{Kind: KindQuote, Children: c.blocks(n, depth+1)}}, true
	case *gast.List:
		l := Node{Kind: KindList, Ordered: n.IsOrdered()}
		if l.Ordered {
			l.Start = n.Start
		}
		for it := n.FirstChild(); it != nil; it = it.NextSibling() {
			l.Children = append(l.Children, Node{Kind: KindItem, Children: c.blocks(it, depth+1)})
		}
		return []Node{l}, true
	case *extast.Table:
		c.b.Report.Tables++
		t := Node{Kind: KindTable}
		for _, a := range n.Alignments {
			t.Align = append(t.Align, alignName(a))
		}
		for r := n.FirstChild(); r != nil; r = r.NextSibling() {
			_, header := r.(*extast.TableHeader)
			row := Node{Kind: KindRow, Header: header}
			for cell := r.FirstChild(); cell != nil; cell = cell.NextSibling() {
				row.Children = append(row.Children, Node{Kind: KindCell, Children: c.inlines(cell, depth)})
			}
			t.Children = append(t.Children, row)
		}
		return []Node{t}, true
	}
	// Anything else (a definition list, a footnote, a kind a later goldmark
	// adds) is kept as its text.
	if t := plainText(n, c.src); t != "" {
		return []Node{{Kind: KindParagraph, Children: []Node{{Kind: KindText, Text: t}}}}, true
	}
	return nil, false
}

func alignName(a extast.Alignment) string {
	switch a {
	case extast.AlignLeft:
		return "left"
	case extast.AlignCenter:
		return "center"
	case extast.AlignRight:
		return "right"
	}
	return ""
}

// inlines maps the inline children of n, merging adjacent text.
func (c *converter) inlines(n gast.Node, depth int) []Node {
	var out []Node
	add := func(nd Node) {
		if nd.Kind == KindText {
			if nd.Text == "" {
				return
			}
			if k := len(out); k > 0 && out[k-1].Kind == KindText {
				out[k-1].Text += nd.Text
				return
			}
		}
		out = append(out, nd)
	}
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		for _, nd := range c.inline(ch, depth+1) {
			add(nd)
		}
	}
	return out
}

func (c *converter) inline(n gast.Node, depth int) []Node {
	if depth >= maxDepth {
		return []Node{{Kind: KindText, Text: plainText(n, c.src)}}
	}
	switch n := n.(type) {
	case *gast.Text:
		v := n.Segment.Value(c.src)
		if !n.IsRaw() {
			v = unescape(v)
		}
		out := []Node{{Kind: KindText, Text: string(v)}}
		switch {
		case n.HardLineBreak():
			out = append(out, Node{Kind: KindBreak})
		case n.SoftLineBreak():
			out[0].Text += " "
		}
		return out
	case *gast.String:
		v := n.Value
		if !n.IsRaw() && !n.IsCode() {
			v = unescape(v)
		}
		return []Node{{Kind: KindText, Text: string(v)}}
	case *gast.CodeSpan:
		var bb bytes.Buffer
		for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
			switch t := ch.(type) {
			case *gast.Text:
				bb.Write(t.Segment.Value(c.src))
			case *gast.String:
				bb.Write(t.Value)
			}
		}
		return []Node{{Kind: KindCodeSpan, Text: bb.String()}}
	case *gast.Emphasis:
		k := KindEmph
		if n.Level >= 2 {
			k = KindStrong
		}
		return []Node{{Kind: k, Children: c.inlines(n, depth)}}
	case *extast.Strikethrough:
		return []Node{{Kind: KindStrike, Children: c.inlines(n, depth)}}
	case *gast.Link:
		return c.link(string(n.Destination), c.inlines(n, depth))
	case *gast.AutoLink:
		url := string(n.URL(c.src))
		label := string(n.Label(c.src))
		if n.AutoLinkType == gast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(url), "mailto:") {
			url = "mailto:" + url
		}
		return c.link(url, []Node{{Kind: KindText, Text: label}})
	case *gast.Image:
		alt := plainText(n, c.src)
		return []Node{c.image(string(n.Destination), alt)}
	case *gast.RawHTML:
		// Raw HTML is text (G5).
		return []Node{{Kind: KindText, Text: string(n.Segments.Value(c.src))}}
	case *extast.TaskCheckBox:
		if n.IsChecked {
			return []Node{{Kind: KindText, Text: "[x] "}}
		}
		return []Node{{Kind: KindText, Text: "[ ] "}}
	}
	return []Node{{Kind: KindText, Text: plainText(n, c.src)}}
}

// link resolves a link. One that is not kept leaves its text.
func (c *converter) link(dest string, children []Node) []Node {
	resolve := c.b.Links
	if resolve == nil {
		resolve = ExternalOnly
	}
	href, anchor, ok := resolve(dest)
	if !ok {
		c.b.Report.LinksFlattened++
		return children
	}
	if len(children) == 0 {
		children = []Node{{Kind: KindText, Text: dest}}
	}
	return []Node{{Kind: KindLink, Href: href, Anchor: anchor, Children: children}}
}

// image takes a picture a body names. A data: URL is the picture itself and
// is added; any other reference (a URL, a volume) is left out, since a
// rendering never fetches.
func (c *converter) image(dest, alt string) Node {
	if strings.HasPrefix(dest, "data:") {
		if n, ok := c.b.dataURLs[dest]; ok {
			if n.Kind == KindImage {
				n.Alt = oneLine(alt)
			}
			return n
		}
	}
	if mt, data, tooLarge, ok := decodeDataURL(dest); ok {
		// Distinct data: URLs are body text, so what is decoded here is
		// bounded by the body cap; a repeat is answered from dataURLs.
		var n Node
		if tooLarge {
			n = c.b.skipImage(oneLine(alt), fmt.Sprintf("a picture larger than %d MiB was left out", MaxImage>>20))
		} else {
			n = c.b.Image(mt, data, alt)
		}
		if c.b.dataURLs == nil {
			c.b.dataURLs = map[string]Node{}
		}
		c.b.dataURLs[dest] = n
		return n
	}
	return c.b.skipImage(oneLine(alt), "a picture the text links to rather than holds was left out")
}

// decodeDataURL reads data:<media type>;base64,<data>. tooLarge reports a
// picture past MaxImage, which is not decoded.
func decodeDataURL(s string) (mt string, data []byte, tooLarge, ok bool) {
	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return "", nil, false, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, false, false
	}
	mt, enc, ok := strings.Cut(meta, ";")
	if !ok || !strings.EqualFold(enc, "base64") {
		return "", nil, false, false
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	if base64.StdEncoding.DecodedLen(len(payload)) > MaxImage+3 {
		return mt, nil, true, true
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(payload), ""))
	if err != nil {
		return "", nil, false, false
	}
	return mt, data, false, true
}

// ExternalOnly keeps a link whose scheme is http, https or mailto (G5).
func ExternalOnly(dest string) (string, string, bool) {
	d := strings.TrimSpace(dest)
	l := strings.ToLower(d)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "mailto:") {
		return d, "", true
	}
	return "", "", false
}

// unescape is what goldmark's HTML writer does to text: backslash escapes
// removed, character references resolved.
func unescape(v []byte) []byte {
	return util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(v)))
}

func linesText(lines *text.Segments, src []byte) string {
	var bb bytes.Buffer
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		bb.Write(seg.Value(src))
	}
	return strings.TrimRight(bb.String(), "\n")
}

// plainText is the text under n, for a node kept only as text.
func plainText(n gast.Node, src []byte) string {
	var bb bytes.Buffer
	_ = gast.Walk(n, func(m gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch t := m.(type) {
		case *gast.Text:
			bb.Write(unescape(t.Segment.Value(src)))
			if t.SoftLineBreak() || t.HardLineBreak() {
				bb.WriteByte(' ')
			}
		case *gast.String:
			bb.Write(t.Value)
		case *gast.RawHTML:
			bb.Write(t.Segments.Value(src))
		}
		if m.Type() == gast.TypeBlock && m.Lines() != nil && m.Lines().Len() > 0 && m.FirstChild() == nil {
			bb.WriteString(linesText(m.Lines(), src))
		}
		return gast.WalkContinue, nil
	})
	return strings.TrimSpace(bb.String())
}

// oneLine folds a title or alt text onto one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
