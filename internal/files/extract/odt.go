package extract

import (
	"context"
	"io"
	"path"
	"strconv"
	"strings"
)

// ODT reads an OpenDocument text document (LibreOffice, and Word's "save as
// ODT"): content.xml for the text, its automatic styles and styles.xml for
// which paragraphs are headings and which lists are numbered, meta.xml for
// the title and page count, and the Pictures/ folder for images.
//
// The output has the DOCX extractor's shape: the document title is the one
// "#" heading, an outline level N heading is one level below it, and a table
// is a GFM table whose cells hold their paragraphs on one line. Tracked
// deletions, comments and bookmarks are left out; footnotes and endnotes are
// marked [n] where they stand and listed under "Notes" at the end.
type ODT struct{}

const (
	odtType = "application/vnd.oasis.opendocument.text"
	ottType = "application/vnd.oasis.opendocument.text-template"
	// maxODTColumns caps the cells of one table row. A row may claim a cell
	// repeated a thousand times (an editor pads a table to the page); empty
	// trailing cells are dropped before the cap applies.
	maxODTColumns = 64
	// maxODTSpaces caps the spaces one text:s stands for.
	maxODTSpaces = 64
)

func (ODT) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{odtType, ottType}, []string{".odt", ".ott"})
}

func (ODT) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "odt"}
	a, err := openZip(ctx, r, size, opts.limits)
	if err != nil {
		return rep, err
	}
	// The package names its own type in a "mimetype" entry; a file named
	// .odt that holds a spreadsheet or something else entirely is refused
	// rather than read as text.
	if a.has("mimetype") {
		b, err := a.read("mimetype")
		if err != nil {
			return rep, err
		}
		if mt := strings.TrimSpace(string(b)); mt != odtType && mt != ottType {
			return rep, refusedf("the file is not an OpenDocument text document (its type is %q)", cutRunesTo(mt, 80))
		}
	}
	if a.has("META-INF/manifest.xml") {
		m, err := a.readXML("META-INF/manifest.xml")
		if err != nil {
			if IsRefused(err) || ctx.Err() != nil {
				return rep, err
			}
		} else if len(m.all("encryption-data")) > 0 {
			return rep, refusedf("the document is encrypted")
		}
	}
	if !a.has("content.xml") {
		return rep, refusedf("the document has no content.xml")
	}
	b, err := a.read("content.xml")
	if err != nil {
		return rep, err
	}
	root, err := parseXMLMixed(ctx, b)
	if err != nil {
		return rep, err
	}
	content := root.child("document-content")
	body := content.path("body", "text")
	if body == nil {
		return rep, refusedf("the document has no text body")
	}
	o := &odt{a: a, w: newWriter(ctx, sink, opts), rep: &rep, opts: opts,
		styles: map[string]odtStyle{}, lists: map[string]map[int]bool{}}
	// styles.xml first: the automatic styles of content.xml are based on
	// its named styles, and a name both use is the content's.
	if a.has("styles.xml") {
		if sb, err := a.read("styles.xml"); err == nil {
			if sr, err := parseXMLMixed(ctx, sb); err == nil {
				o.loadStyles(sr.child("document-styles"))
			} else if IsRefused(err) || ctx.Err() != nil {
				return rep, err
			}
		} else if IsRefused(err) || ctx.Err() != nil {
			return rep, err
		}
	}
	o.loadStyles(content)
	docTitle, pages, err := odtMeta(ctx, a)
	if err != nil {
		return rep, err
	}
	rep.Pages = pages

	// A Title paragraph names the document; one in deleted text does not.
	for _, p := range textBlocks(body) {
		if p.name == "p" && o.paraLevel(p) == levelTitle {
			if t := strings.TrimSpace(oneLine(o.plain(p))); t != "" {
				docTitle, o.titlePara = t, p
				break
			}
		}
	}
	if docTitle == "" {
		docTitle = title(opts.Name, "Document")
	}
	if err := o.emit(block{text: heading(1, docTitle)}); err != nil {
		return rep, err
	}
	if err := o.blocks(body); err != nil {
		rep.finish(o.w)
		return rep, err
	}
	if err := o.emitNotes(); err != nil {
		rep.finish(o.w)
		return rep, err
	}
	if o.cutColumns {
		rep.note("table rows wider than %d columns were cut", maxODTColumns)
	}
	if o.cutRows {
		rep.note("only the first %d rows of a table were read", opts.maxRows())
	}
	rep.finish(o.w)
	return rep, nil
}

func cutRunesTo(s string, n int) string {
	if t, cut := cutRunesPlain(s, n); cut {
		return t + "…"
	}
	return s
}

// odtMeta reads meta.xml: the document's title and its page count. Either
// may be missing, and so may the file.
func odtMeta(ctx context.Context, a *archive) (string, int, error) {
	if !a.has("meta.xml") {
		return "", 0, nil
	}
	b, err := a.read("meta.xml")
	if err != nil {
		if IsRefused(err) || ctx.Err() != nil {
			return "", 0, err
		}
		return "", 0, nil
	}
	root, err := parseXMLMixed(ctx, b)
	if err != nil {
		if IsRefused(err) || ctx.Err() != nil {
			return "", 0, err
		}
		return "", 0, nil
	}
	t := ""
	if n := root.all("title"); len(n) > 0 {
		t = strings.TrimSpace(oneLine(odfText(n[0])))
	}
	pages := 0
	if n := root.all("document-statistic"); len(n) > 0 {
		pages, _ = strconv.Atoi(n[0].attr("page-count"))
	}
	return t, max(pages, 0), nil
}

type odt struct {
	a    *archive
	w    *writer
	rep  *Report
	opts Options

	styles map[string]odtStyle     // paragraph styles, by style:name
	lists  map[string]map[int]bool // list style → level → numbered

	titlePara *node
	notes     []string
	inNote    bool
	// space is set while the text written last ended in white space that
	// ODF collapses, so the next run's leading white space is dropped.
	space      bool
	cutColumns bool
	cutRows    bool
	lastList   bool
	started    bool
}

type odtStyle struct {
	level  int // levelTitle, levelBody, or a heading level
	parent string
}

// loadStyles reads the paragraph and list styles of an office:styles or
// office:automatic-styles block under root (content.xml or styles.xml).
func (o *odt) loadStyles(root *node) {
	if root == nil {
		return
	}
	for _, block := range []string{"styles", "automatic-styles"} {
		for _, s := range root.child(block).kidsNamed("style") {
			name := s.attr("name")
			if name == "" || (s.attr("family") != "" && s.attr("family") != "paragraph") {
				continue
			}
			st := odtStyle{level: odtStyleLevel(name, s.attr("display-name")), parent: s.attr("parent-style-name")}
			if st.level == levelBody {
				if n, err := strconv.Atoi(s.attr("default-outline-level")); err == nil && n >= 1 && n <= 10 {
					st.level = n
				}
			}
			o.styles[name] = st
		}
		for _, ls := range root.child(block).kidsNamed("list-style") {
			levels := map[int]bool{}
			for _, l := range ls.kids {
				n, err := strconv.Atoi(l.attr("level"))
				if err != nil {
					continue
				}
				levels[n] = l.name == "list-level-style-number" && l.attr("num-format") != ""
			}
			if name := ls.attr("name"); name != "" {
				o.lists[name] = levels
			}
		}
	}
}

// odtStyleLevel reads a heading level from a style's name. LibreOffice
// writes "Heading_20_2" (the encoded form of "Heading 2") and keeps the
// English names whatever the document's language.
func odtStyleLevel(name, display string) int {
	for _, s := range []string{display, strings.ReplaceAll(name, "_20_", " ")} {
		s = strings.ToLower(strings.TrimSpace(s))
		switch {
		case s == "title":
			return levelTitle
		case strings.HasPrefix(s, "heading"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "heading")))
			if err == nil && n >= 1 && n <= 10 {
				return n
			}
		}
	}
	return levelBody
}

func (o *odt) styleLevel(name string) int {
	for range 16 { // parent chains are short; a cycle must not loop
		st, ok := o.styles[name]
		if !ok {
			return levelBody
		}
		if st.level != levelBody || st.parent == "" {
			return st.level
		}
		name = st.parent
	}
	return levelBody
}

// paraLevel is a text:p's or text:h's heading level.
func (o *odt) paraLevel(p *node) int {
	if p.name == "h" {
		if n, err := strconv.Atoi(p.attr("outline-level")); err == nil && n >= 1 {
			return min(n, 10)
		}
		if l := o.styleLevel(p.attr("style-name")); l > 0 {
			return l
		}
		return 1
	}
	return o.styleLevel(p.attr("style-name"))
}

func (o *odt) emit(b block) error {
	if b.text == "" {
		return nil
	}
	sep := "\n\n"
	switch {
	case !o.started:
		sep = ""
	case b.list && o.lastList:
		sep = "\n"
	}
	o.started, o.lastList = true, b.list
	return o.w.text(sep + b.text)
}

// blocks renders the block-level children of the body, a section, an index
// or a text box.
func (o *odt) blocks(parent *node) error {
	for _, k := range parent.kids {
		if err := o.w.ctx.Err(); err != nil {
			return err
		}
		switch k.name {
		case "p", "h":
			if k == o.titlePara {
				continue
			}
			b, err := o.para(k)
			if err != nil {
				return err
			}
			if err := o.emit(b); err != nil {
				return err
			}
		case "list":
			if err := o.list(k, 0, ""); err != nil {
				return err
			}
		case "table":
			t, err := o.table(k)
			if err != nil {
				return err
			}
			if err := o.emit(block{text: t}); err != nil {
				return err
			}
		case "frame":
			// a frame anchored to the page rather than to a paragraph
			var segs []seg
			if err := o.frame(k, &segs); err != nil {
				return err
			}
			if err := o.emit(block{text: renderSegs(segs)}); err != nil {
				return err
			}
		case textNode, "tracked-changes", "sequence-decls", "variable-decls", "user-field-decls", "dde-connection-decls",
			"forms", "soft-page-break", "table-of-content-source", "index-source", "illustration-index-source",
			"alphabetical-index-source", "table-index-source", "object-index-source", "user-index-source", "bibliography-source",
			"annotation", "annotation-end", "bookmark", "bookmark-start", "bookmark-end":
			// declarations, an index's template, editing marks and
			// deleted text are not content
		default:
			// sections, indexes (their index-body holds the text) and any
			// container this reader does not name
			if err := o.blocks(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// para renders a text:p or text:h.
func (o *odt) para(p *node) (block, error) {
	var segs []seg
	if err := o.paraRuns(p, &segs); err != nil {
		return block{}, err
	}
	level := o.paraLevel(p)
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
	return block{text: renderSegs(segs)}, nil
}

// runs collects a paragraph's text and images in order. ODF collapses runs
// of white space in its text to one space; a space it means to keep is a
// text:s.
func (o *odt) runs(n *node, out *[]seg) error {
	for _, k := range n.kids {
		switch k.name {
		case textNode:
			t := odfSpace(k.text)
			if o.space {
				t = strings.TrimPrefix(t, " ")
			}
			if t != "" {
				o.space = strings.HasSuffix(t, " ")
				*out = append(*out, seg{text: t})
			}
			continue
		case "s":
			c, err := strconv.Atoi(k.attr("c"))
			if err != nil || c < 1 {
				c = 1
			}
			*out = append(*out, seg{text: strings.Repeat(" ", min(c, maxODTSpaces))})
		case "tab":
			*out = append(*out, seg{text: " "})
		case "line-break":
			*out = append(*out, seg{text: "\n"})
		case "note":
			o.note(k, out)
		case "frame":
			if err := o.frame(k, out); err != nil {
				return err
			}
		case "annotation", "annotation-end", "bookmark", "bookmark-start", "bookmark-end",
			"reference-mark", "reference-mark-start", "reference-mark-end", "soft-page-break",
			"change", "change-start", "change-end", "toc-mark", "toc-mark-start", "toc-mark-end",
			"alphabetical-index-mark", "alphabetical-index-mark-start", "alphabetical-index-mark-end",
			"user-index-mark", "user-index-mark-start", "user-index-mark-end", "bibliography-mark":
			// comments, anchors and the ends of a tracked change are not
			// text; an insertion's text stands between its marks
		default:
			// spans, links, fields (their current value is their text)
			if err := o.runs(k, out); err != nil {
				return err
			}
			continue
		}
		// a space, a break, a note mark or a picture: what follows keeps
		// its own leading white space
		o.space = false
	}
	return nil
}

// paraRuns collects one paragraph's runs. ODF drops the white space a
// paragraph starts with.
func (o *odt) paraRuns(p *node, out *[]seg) error {
	o.space = true
	err := o.runs(p, out)
	o.space = false
	return err
}

func odfSpace(s string) string {
	if !strings.ContainsAny(s, " \t\r\n") {
		return s
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// note marks a footnote or endnote [n] in the text and keeps its body for
// the Notes section. A note inside a note is read as text.
func (o *odt) note(n *node, out *[]seg) {
	if o.inNote {
		return
	}
	cit := strings.TrimSpace(oneLine(odfText(n.child("note-citation"))))
	if cit == "" {
		cit = strconv.Itoa(len(o.notes) + 1)
	}
	o.inNote = true
	space := o.space
	var parts []string
	for _, p := range textBlocks(n.child("note-body")) {
		var segs []seg
		_ = o.paraRuns(p, &segs) // images inside a note are not kept
		var t strings.Builder
		for _, s := range segs {
			t.WriteString(s.text)
		}
		if s := strings.TrimSpace(oneLine(t.String())); s != "" {
			parts = append(parts, s)
		}
	}
	o.inNote, o.space = false, space
	*out = append(*out, seg{text: "[" + cit + "]"})
	o.notes = append(o.notes, "["+cit+"] "+strings.Join(parts, " "))
}

func (o *odt) emitNotes() error {
	if len(o.notes) == 0 {
		return nil
	}
	if err := o.emit(block{text: heading(2, "Notes")}); err != nil {
		return err
	}
	for _, n := range o.notes {
		if err := o.emit(block{text: paragraph(n)}); err != nil {
			return err
		}
	}
	return nil
}

// frame renders a draw:frame: its picture, or the paragraphs of its text
// box. An image's href is only ever a name inside the package; one that
// points outside it (a URL, an absolute path) is not followed.
func (o *odt) frame(n *node, out *[]seg) error {
	alt := strings.TrimSpace(oneLine(odfText(n.child("title"))))
	if alt == "" {
		alt = strings.TrimSpace(oneLine(odfText(n.child("desc"))))
	}
	for _, k := range n.kids {
		switch k.name {
		case "image":
			if o.inNote {
				continue
			}
			md, err := o.image(k.attr("href"), alt)
			if err != nil {
				return err
			}
			if md != "" {
				*out = append(*out, seg{image: md})
			}
			// A frame lists the same picture in several formats (an
			// object and its replacement image): the first one stands.
			return nil
		case "text-box":
			for _, p := range textBlocks(k) {
				*out = append(*out, seg{text: "\n"})
				if err := o.paraRuns(p, out); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (o *odt) image(href, alt string) (string, error) {
	name := strings.TrimPrefix(href, "./")
	if name == "" || strings.Contains(name, ":") || strings.HasPrefix(name, "/") || strings.HasPrefix(path.Clean(name), "..") {
		if strings.TrimSpace(alt) == "" {
			return "", nil
		}
		return altOnly(cleanAlt(alt)), nil
	}
	data, err := o.a.read(path.Clean(name))
	if err != nil {
		if IsRefused(err) || o.w.ctx.Err() != nil {
			return "", err
		}
		return altOnly(cleanAlt(alt)), nil
	}
	return o.w.image(data, alt)
}

// list renders a text:list: each paragraph of an item is an item line,
// indented by depth; a heading inside a list (numbered outline headings
// are written that way) stays a heading.
func (o *odt) list(n *node, depth int, style string) error {
	if s := n.attr("style-name"); s != "" {
		style = s
	}
	marker := "- "
	if o.lists[style][depth+1] {
		marker = "1. "
	}
	for _, item := range n.kids {
		if item.name != "list-item" && item.name != "list-header" {
			continue
		}
		for _, k := range item.kids {
			if err := o.w.ctx.Err(); err != nil {
				return err
			}
			switch k.name {
			case "p":
				var segs []seg
				if err := o.paraRuns(k, &segs); err != nil {
					return err
				}
				body := renderSegs(segs)
				if body == "" {
					continue
				}
				line := strings.Repeat("  ", min(depth, 8)) + marker + strings.ReplaceAll(body, "\n", " ")
				if err := o.emit(block{text: line, list: true}); err != nil {
					return err
				}
			case "h":
				b, err := o.para(k)
				if err != nil {
					return err
				}
				if err := o.emit(b); err != nil {
					return err
				}
			case "list":
				if err := o.list(k, depth+1, style); err != nil {
					return err
				}
			case "table":
				t, err := o.table(k)
				if err != nil {
					return err
				}
				if err := o.emit(block{text: t}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// table renders a table:table as a GFM table. A cell's paragraphs are joined
// on one line, a covered cell (under a merged one) is empty so the columns
// after it stay aligned, and repeated cells and rows are expanded under
// caps.
func (o *odt) table(t *node) (string, error) {
	var rows [][]string
	maxRows := o.opts.maxRows()
	// Empty rows are held back until a row with text follows them: an
	// editor pads a table with a million empty rows, which end the table
	// rather than count against its cap.
	pending := 0
	add := func(row []string, n int) bool {
		if allEmpty(row) {
			pending = min(pending+n, maxRows)
			return true
		}
		for range pending + n {
			if len(rows) >= maxRows {
				o.cutRows = true
				return false
			}
			if pending > 0 {
				rows, pending = append(rows, nil), pending-1
				continue
			}
			rows = append(rows, row)
		}
		return true
	}
	var walk func(n *node) error
	walk = func(n *node) error {
		for _, k := range n.kids {
			switch k.name {
			case "table-header-rows", "table-rows", "table-row-group":
				if err := walk(k); err != nil {
					return err
				}
			case "table-row":
				row, err := o.row(k)
				if err != nil {
					return err
				}
				if !add(row, odfRepeat(k.attr("number-rows-repeated"))) {
					return nil
				}
			}
		}
		return nil
	}
	if err := walk(t); err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	o.rep.Tables++
	return table(rows), nil
}

type odtCell struct {
	text   string
	repeat int
}

func (o *odt) row(tr *node) ([]string, error) {
	var cells []odtCell
	for _, tc := range tr.kids {
		switch tc.name {
		case "table-cell":
			txt, err := o.cell(tc)
			if err != nil {
				return nil, err
			}
			cells = append(cells, odtCell{text: txt, repeat: odfRepeat(tc.attr("number-columns-repeated"))})
		case "covered-table-cell":
			cells = append(cells, odtCell{repeat: odfRepeat(tc.attr("number-columns-repeated"))})
		}
	}
	for len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1].text) == "" {
		cells = cells[:len(cells)-1]
	}
	var row []string
	for _, c := range cells {
		for range c.repeat {
			if len(row) == maxODTColumns {
				o.cutColumns = true
				return row, nil
			}
			row = append(row, c.text)
		}
	}
	return row, nil
}

// cell renders a table cell's paragraphs (a nested table's included) on
// one line; an image in a cell stays in the cell.
func (o *odt) cell(tc *node) (string, error) {
	var parts []string
	for _, p := range textBlocks(tc) {
		var segs []seg
		if err := o.paraRuns(p, &segs); err != nil {
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
	return strings.Join(nonEmpty(parts), " "), nil
}

// odfRepeat reads a number-*-repeated attribute: at least one.
func odfRepeat(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func allEmpty(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// textBlocks returns the text:p and text:h elements under n in document
// order, without looking inside them (a note's paragraphs belong to the
// paragraph that cites it) or into deleted text.
func textBlocks(n *node) []*node {
	var out []*node
	var walk func(*node, int)
	walk = func(m *node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for _, k := range m.kids {
			switch k.name {
			case "p", "h":
				out = append(out, k)
			case "tracked-changes", "annotation":
			default:
				walk(k, depth+1)
			}
		}
	}
	if n != nil {
		walk(n, 0)
	}
	return out
}

// plain is a paragraph's text without its notes, for the title.
func (o *odt) plain(p *node) string {
	saved := o.inNote
	o.inNote = true // a title does not cite notes or keep pictures
	var segs []seg
	_ = o.paraRuns(p, &segs)
	o.inNote = saved
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.text)
	}
	return b.String()
}

// odfText is all the character data under n, in order.
func odfText(n *node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*node, int)
	walk = func(m *node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for _, k := range m.kids {
			if k.name == textNode {
				b.WriteString(k.text)
				continue
			}
			walk(k, depth+1)
		}
	}
	walk(n, 0)
	return b.String()
}

// kidsNamed returns the direct children called local.
func (n *node) kidsNamed(local string) []*node {
	if n == nil {
		return nil
	}
	var out []*node
	for _, k := range n.kids {
		if k.name == local {
			out = append(out, k)
		}
	}
	return out
}
