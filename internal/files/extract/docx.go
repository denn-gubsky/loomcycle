package extract

import (
	"context"
	"io"
	"strconv"
	"strings"
)

// DOCX reads a Word document: word/document.xml for the text, styles.xml for
// which paragraphs are headings, numbering.xml for lists, and the document's
// relationships for its images.
//
// The document's title is the Markdown's one "#" heading (a Title-style
// paragraph, else the document properties' title, else the file name), and a
// "Heading N" paragraph is one level below it, so the Markdown imports as one
// document whose sections are the Word headings.
type DOCX struct{}

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

func (DOCX) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{docxType}, []string{".docx", ".docm"})
}

func (DOCX) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "docx"}
	a, err := openZip(ctx, r, size, opts.limits)
	if err != nil {
		return rep, err
	}
	pkg, err := a.rels("")
	if err != nil {
		return rep, err
	}
	main := "word/document.xml"
	for _, rl := range pkg {
		if relType(rl.Type, "officeDocument") && !rl.External {
			main = rl.Target
		}
	}
	doc, err := a.readXML(main)
	if err != nil {
		return rep, err
	}
	rels, err := a.rels(main)
	if err != nil {
		return rep, err
	}
	d := &docx{a: a, rels: rels, w: newWriter(ctx, sink, opts), rep: &rep}
	for _, rl := range rels {
		if rl.External {
			continue
		}
		switch {
		case relType(rl.Type, "styles"):
			if err := d.loadStyles(rl.Target); err != nil {
				return rep, err
			}
		case relType(rl.Type, "numbering"):
			if err := d.loadNumbering(rl.Target); err != nil {
				return rep, err
			}
		}
	}
	docTitle, pages := packageProps(a, pkg)
	rep.Pages = pages

	body := doc.path("document", "body")
	if body == nil {
		return rep, refusedf("the document has no body")
	}
	// A Title-style paragraph names the document: it becomes the "#" heading
	// and is not repeated below it.
	for _, p := range body.all("p") {
		if d.paraLevel(p) == levelTitle {
			if t := strings.TrimSpace(plainText(p)); t != "" {
				docTitle, d.titlePara = t, p
				break
			}
		}
	}
	if docTitle == "" {
		docTitle = title(opts.Name, "Document")
	}
	if err := d.emit(block{text: heading(1, docTitle)}); err != nil {
		return rep, err
	}
	if err := d.blocks(body); err != nil {
		rep.finish(d.w)
		return rep, err
	}
	rep.finish(d.w)
	return rep, nil
}

// packageProps reads the title from docProps/core.xml and the page count
// Word cached in docProps/app.xml. Either may be missing.
func packageProps(a *archive, pkg map[string]rel) (string, int) {
	var t string
	pages := 0
	for _, rl := range pkg {
		if rl.External {
			continue
		}
		switch {
		case relType(rl.Type, "core-properties") || strings.HasSuffix(rl.Type, "/metadata/core-properties"):
			if root, err := a.readXML(rl.Target); err == nil {
				if n := root.all("title"); len(n) > 0 {
					t = strings.TrimSpace(n[0].text)
				}
			}
		case relType(rl.Type, "extended-properties"):
			if root, err := a.readXML(rl.Target); err == nil {
				if n := root.all("Pages"); len(n) > 0 {
					pages, _ = strconv.Atoi(strings.TrimSpace(n[0].text))
				}
			}
		}
	}
	return t, max(pages, 0)
}

const (
	levelBody  = 0
	levelTitle = -1
)

type docx struct {
	a         *archive
	rels      map[string]rel
	w         *writer
	rep       *Report
	styles    map[string]docxStyle
	ordered   map[string]map[string]bool // numId → ilvl → ordered
	titlePara *node
	lastList  bool
	started   bool
}

type docxStyle struct {
	level   int // levelTitle, levelBody, or a heading level 1..9
	basedOn string
}

func (d *docx) loadStyles(part string) error {
	root, err := d.a.readXML(part)
	if err != nil {
		if IsRefused(err) || d.w.ctx.Err() != nil {
			return err
		}
		return nil // a document without readable styles is all body text
	}
	d.styles = map[string]docxStyle{}
	for _, s := range root.all("style") {
		id := s.attr("styleId")
		if id == "" {
			continue
		}
		st := docxStyle{level: styleLevel(s.child("name").attr("val"), id)}
		if st.level == levelBody {
			if o := s.path("pPr", "outlineLvl"); o != nil {
				st.level = outlineLevel(o.attr("val"))
			}
		}
		if b := s.child("basedOn"); b != nil {
			st.basedOn = b.attr("val")
		}
		d.styles[id] = st
	}
	return nil
}

// styleLevel reads a heading level from a style's name ("heading 2",
// "Title") or, for a document whose names are missing, its id. Built-in
// style names are English whatever the language of the document.
func styleLevel(name, id string) int {
	for _, s := range []string{strings.ToLower(strings.TrimSpace(name)), strings.ToLower(id)} {
		switch {
		case s == "title":
			return levelTitle
		case strings.HasPrefix(s, "heading"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "heading")))
			if err == nil && n >= 1 && n <= 9 {
				return n
			}
		}
	}
	return levelBody
}

// outlineLevel maps w:outlineLvl (0-based, 9 = body text) to a heading level.
func outlineLevel(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 8 {
		return levelBody
	}
	return n + 1
}

func (d *docx) styleLevel(id string) int {
	for range 16 { // basedOn chains are short; a cycle must not loop
		st, ok := d.styles[id]
		if !ok {
			return levelBody
		}
		if st.level != levelBody || st.basedOn == "" {
			return st.level
		}
		id = st.basedOn
	}
	return levelBody
}

func (d *docx) loadNumbering(part string) error {
	root, err := d.a.readXML(part)
	if err != nil {
		if IsRefused(err) || d.w.ctx.Err() != nil {
			return err
		}
		return nil
	}
	abstract := map[string]map[string]bool{}
	for _, an := range root.all("abstractNum") {
		lv := map[string]bool{}
		for _, l := range an.all("lvl") {
			f := l.child("numFmt").attr("val")
			lv[l.attr("ilvl")] = f != "" && f != "bullet" && f != "none"
		}
		abstract[an.attr("abstractNumId")] = lv
	}
	d.ordered = map[string]map[string]bool{}
	for _, n := range root.all("num") {
		if ref := n.child("abstractNumId"); ref != nil {
			d.ordered[n.attr("numId")] = abstract[ref.attr("val")]
		}
	}
	return nil
}

// paraLevel is a paragraph's heading level: its own outline level, else its
// style's.
func (d *docx) paraLevel(p *node) int {
	ppr := p.child("pPr")
	if o := ppr.child("outlineLvl"); o != nil {
		if l := outlineLevel(o.attr("val")); l != levelBody {
			return l
		}
	}
	if s := ppr.child("pStyle"); s != nil {
		return d.styleLevel(s.attr("val"))
	}
	return levelBody
}

// block is one Markdown block; list items are joined without a blank line.
type block struct {
	text string
	list bool
}

func (d *docx) emit(b block) error {
	if b.text == "" {
		return nil
	}
	sep := "\n\n"
	switch {
	case !d.started:
		sep = ""
	case b.list && d.lastList:
		sep = "\n"
	}
	d.started, d.lastList = true, b.list
	return d.w.text(sep + b.text)
}

// blocks renders the block-level children of a body, a content control or
// a table cell.
func (d *docx) blocks(parent *node) error {
	for _, k := range parent.kids {
		if err := d.w.ctx.Err(); err != nil {
			return err
		}
		switch k.name {
		case "p":
			if k == d.titlePara {
				continue
			}
			b, err := d.para(k)
			if err != nil {
				return err
			}
			if err := d.emit(b); err != nil {
				return err
			}
		case "tbl":
			t, err := d.table(k)
			if err != nil {
				return err
			}
			if err := d.emit(block{text: t}); err != nil {
				return err
			}
		case "sdt":
			if c := k.child("sdtContent"); c != nil {
				if err := d.blocks(c); err != nil {
					return err
				}
			}
		case "customXml", "ins", "smartTag":
			if err := d.blocks(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// seg is a piece of a paragraph: document text, or the Markdown of an image.
type seg struct {
	text  string
	image string
}

// runs collects a paragraph's text and images in order.
func (d *docx) runs(n *node, out *[]seg) error {
	for _, k := range n.kids {
		switch k.name {
		case "t":
			*out = append(*out, seg{text: k.text})
		case "tab", "ptab":
			*out = append(*out, seg{text: " "})
		case "br", "cr":
			*out = append(*out, seg{text: "\n"})
		case "noBreakHyphen":
			*out = append(*out, seg{text: "-"})
		case "delText", "instrText", "del", "rPr", "pPr", "fldData":
			// deleted text, field codes and formatting are not content
		case "drawing", "pict", "object":
			if err := d.drawing(k, out); err != nil {
				return err
			}
		default:
			if err := d.runs(k, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// drawing renders an embedded picture (DrawingML a:blip, or VML
// v:imagedata) and the text of a text box.
func (d *docx) drawing(n *node, out *[]seg) error {
	alt := ""
	if p := n.all("docPr"); len(p) > 0 {
		alt = p[0].attr("descr")
		if alt == "" {
			alt = p[0].attr("title")
		}
	}
	var ids []string
	for _, b := range n.all("blip") {
		ids = append(ids, b.attrRel("embed"))
	}
	for _, v := range n.all("imagedata") {
		ids = append(ids, v.attrRel("id"))
		if alt == "" {
			alt = v.attr("title")
		}
	}
	for _, id := range ids {
		md, err := d.image(id, alt)
		if err != nil {
			return err
		}
		if md != "" {
			*out = append(*out, seg{image: md})
		}
	}
	for _, box := range n.all("txbxContent") {
		for _, p := range box.all("p") {
			*out = append(*out, seg{text: "\n" + plainText(p)})
		}
	}
	return nil
}

// image reads a related image part and hands it to the writer.
func (d *docx) image(id, alt string) (string, error) {
	return relImage(d.a, d.w, d.rels, id, alt)
}

// relImage resolves an image relationship and hands its bytes to the
// writer. A missing or external target leaves only the alt text; a refusal
// (the archive's bounds) stops the extraction.
func relImage(a *archive, w *writer, rels map[string]rel, id, alt string) (string, error) {
	rl, ok := rels[id]
	if id == "" || !ok || rl.External || !relType(rl.Type, "image") {
		if strings.TrimSpace(alt) == "" {
			return "", nil
		}
		return altOnly(cleanAlt(alt)), nil
	}
	data, err := a.read(rl.Target)
	if err != nil {
		if IsRefused(err) || w.ctx.Err() != nil {
			return "", err
		}
		return altOnly(cleanAlt(alt)), nil
	}
	return w.image(data, alt)
}

func (d *docx) para(p *node) (block, error) {
	var segs []seg
	if err := d.runs(p, &segs); err != nil {
		return block{}, err
	}
	level := d.paraLevel(p)
	if level != levelBody {
		var text strings.Builder
		var images []string
		for _, s := range segs {
			text.WriteString(s.text)
			if s.image != "" {
				images = append(images, s.image)
			}
		}
		h := ""
		if t := strings.TrimSpace(text.String()); t != "" {
			lv := level + 1 // under the document's "#"
			if level == levelTitle {
				lv = 2
			}
			h = heading(lv, t)
		}
		parts := append([]string{h}, images...)
		return block{text: strings.Trim(strings.Join(parts, "\n"), "\n")}, nil
	}
	body := renderSegs(segs)
	if body == "" {
		return block{}, nil
	}
	if np := p.path("pPr", "numPr"); np != nil {
		numID := np.child("numId").attr("val")
		if numID != "" && numID != "0" {
			ilvl := np.child("ilvl").attr("val")
			n, _ := strconv.Atoi(ilvl)
			marker := "- "
			if d.ordered[numID][ilvl] {
				marker = "1. "
			}
			item := strings.ReplaceAll(body, "\n", " ")
			return block{text: strings.Repeat("  ", min(max(n, 0), 8)) + marker + item, list: true}, nil
		}
	}
	return block{text: body}, nil
}

// renderSegs renders a paragraph: its text escaped line by line, each image
// on a line of its own.
func renderSegs(segs []seg) string {
	var lines []string
	var text strings.Builder
	flush := func() {
		if p := paragraph(text.String()); p != "" {
			lines = append(lines, p)
		}
		text.Reset()
	}
	for _, s := range segs {
		if s.image != "" {
			flush()
			lines = append(lines, s.image)
			continue
		}
		text.WriteString(s.text)
	}
	flush()
	return strings.Join(lines, "\n")
}

// table renders a w:tbl as a GFM table. A cell's paragraphs are joined on
// one line; an image in a cell stays in the cell.
func (d *docx) table(t *node) (string, error) {
	var rows [][]string
	for _, tr := range t.kids {
		if tr.name != "tr" {
			continue
		}
		var row []string
		for _, tc := range tr.kids {
			if tc.name != "tc" {
				continue
			}
			var parts []string
			for _, p := range tc.all("p") {
				var segs []seg
				if err := d.runs(p, &segs); err != nil {
					return "", err
				}
				var text strings.Builder
				for _, s := range segs {
					if s.image != "" {
						parts = append(parts, cell(text.String()), s.image)
						text.Reset()
						continue
					}
					text.WriteString(s.text)
				}
				parts = append(parts, cell(text.String()))
			}
			row = append(row, strings.Join(nonEmpty(parts), " "))
			// A merged cell spans several grid columns; empty cells keep
			// the columns after it aligned.
			if span, _ := strconv.Atoi(tc.path("tcPr", "gridSpan").attr("val")); span > 1 {
				for range min(span-1, 64) {
					row = append(row, "")
				}
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return "", nil
	}
	d.rep.Tables++
	return table(rows), nil
}

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// plainText is the text of an element's w:t / a:t descendants.
func plainText(n *node) string {
	var b strings.Builder
	var walk func(*node, int)
	walk = func(m *node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for _, k := range m.kids {
			switch k.name {
			case "t":
				b.WriteString(k.text)
			case "tab":
				b.WriteString(" ")
			case "br":
				b.WriteString("\n")
			case "delText", "instrText":
			default:
				walk(k, depth+1)
			}
		}
	}
	walk(n, 0)
	return b.String()
}
