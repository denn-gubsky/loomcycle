package render

import (
	"context"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// writeMarkdown writes the Doc as clean CommonMark with GFM tables. Every
// piece of text is escaped, so a body's own syntax comes out as the structure
// the model holds and never as anything else. Pictures are linked under
// opts.AssetDir, never inlined: the caller writes them beside the file.
func writeMarkdown(ctx context.Context, w io.Writer, d *Doc, opts Options) error {
	m := &mdWriter{w: &capWriter{w: w, ctx: ctx}, opts: opts, used: usedAnchors(d, opts)}
	m.title(d)
	if opts.TOC && len(d.Sections) > 0 {
		m.toc(d.Sections, 0)
		m.blank()
	}
	m.blocks(d.Blocks, "", 0)
	for _, s := range d.Sections {
		m.section(s)
	}
	return m.w.err
}

type mdWriter struct {
	w    *capWriter
	opts Options
	used map[string]bool
}

func (m *mdWriter) s(s string) { m.w.WriteString(s) }

func (m *mdWriter) blank() { m.s("\n") }

func (m *mdWriter) title(d *Doc) {
	m.anchor(d.Anchor)
	m.s("# " + escapeInline(oneLine(d.Title)) + "\n\n")
	var meta []string
	if l := statusLabel(d.Status); l != "" {
		meta = append(meta, "**"+l+"**")
	}
	if d.Provenance != "" {
		meta = append(meta, escapeInline(oneLine(d.Provenance)))
	}
	if d.Date != "" {
		meta = append(meta, escapeInline(d.Date))
	}
	if len(meta) > 0 {
		m.s(strings.Join(meta, " · ") + "\n\n")
	}
	if m.opts.TitlePage {
		m.s("---\n\n")
	}
}

func (m *mdWriter) toc(ss []*Section, depth int) {
	for _, s := range ss {
		m.s(strings.Repeat("  ", depth) + "- [" + escapeInline(oneLine(s.Title)) + "](#" + s.Anchor + ")\n")
		m.toc(s.Children, depth+1)
	}
}

// anchor writes an HTML anchor for a heading that a link or the table of
// contents points at; CommonMark has no heading ids of its own.
func (m *mdWriter) anchor(a string) {
	if a != "" && m.used[a] {
		m.s(`<a id="` + a + `"></a>` + "\n\n")
	}
}

func (m *mdWriter) section(s *Section) {
	m.anchor(s.Anchor)
	lvl := clampLevel(s.Level)
	m.s(strings.Repeat("#", lvl) + " " + escapeInline(oneLine(s.Title)) + "\n\n")
	m.blocks(s.Blocks, "", 0)
	for _, c := range s.Children {
		m.section(c)
	}
}

// blocks writes blocks, each line prefixed with prefix (a quote's "> " or a
// list item's indent).
func (m *mdWriter) blocks(ns []Node, prefix string, depth int) {
	for i, n := range ns {
		if i > 0 {
			m.s(strings.TrimRight(prefix, " ") + "\n")
		}
		m.block(n, prefix, depth)
	}
	if len(ns) > 0 && prefix == "" {
		m.blank()
	}
}

func (m *mdWriter) block(n Node, prefix string, depth int) {
	if depth > maxDepth {
		return
	}
	switch n.Kind {
	case KindParagraph:
		for _, l := range strings.Split(escapeLineStart(m.inlines(n.Children)), "\n") {
			m.s(prefix + l + "\n")
		}
	case KindHeading:
		// A heading inside a body sits below the section's own level.
		m.s(prefix + strings.Repeat("#", clampLevel(n.Level)) + " " + oneLine(m.inlines(n.Children)) + "\n")
	case KindRule:
		m.s(prefix + "---\n")
	case KindCode, KindDiagram:
		m.fence(n.Lang, n.Text, prefix)
	case KindImage:
		m.s(prefix + m.image(n) + "\n")
	case KindQuote:
		if len(n.Children) == 0 {
			m.s(prefix + ">\n")
			return
		}
		m.blocks(n.Children, prefix+"> ", depth+1)
	case KindList:
		num := n.Start
		if num < 1 {
			num = 1
		}
		for i, it := range n.Children {
			marker := "- "
			if n.Ordered {
				marker = strconv.Itoa(num+i) + ". "
			}
			m.item(it, prefix, marker, depth+1)
		}
	case KindTable:
		m.table(n, prefix)
	}
}

// item writes one list item: its first block after the marker, the rest
// indented under it.
func (m *mdWriter) item(it Node, prefix, marker string, depth int) {
	indent := prefix + strings.Repeat(" ", len(marker))
	if len(it.Children) == 0 {
		m.s(prefix + strings.TrimRight(marker, " ") + "\n")
		return
	}
	for i, b := range it.Children {
		if i == 0 {
			var sub capWriter
			sub.w = &strings.Builder{}
			inner := &mdWriter{w: &sub, opts: m.opts, used: m.used}
			inner.block(b, "", depth)
			lines := strings.Split(strings.TrimRight(sub.w.(*strings.Builder).String(), "\n"), "\n")
			for j, l := range lines {
				if j == 0 {
					m.s(prefix + marker + l + "\n")
				} else if l == "" {
					m.s(strings.TrimRight(indent, " ") + "\n")
				} else {
					m.s(indent + l + "\n")
				}
			}
			continue
		}
		m.s(strings.TrimRight(indent, " ") + "\n")
		m.block(b, indent, depth)
	}
}

func (m *mdWriter) fence(lang, body, prefix string) {
	ticks := "```"
	for strings.Contains(body, ticks) {
		ticks += "`"
	}
	m.s(prefix + ticks + oneLine(strings.ReplaceAll(lang, "`", "")) + "\n")
	for _, l := range strings.Split(body, "\n") {
		m.s(strings.TrimRight(prefix+l, " ") + "\n")
	}
	m.s(prefix + ticks + "\n")
}

func (m *mdWriter) table(t Node, prefix string) {
	cols := 0
	for _, r := range t.Children {
		if len(r.Children) > cols {
			cols = len(r.Children)
		}
	}
	if cols == 0 {
		return
	}
	row := func(r Node) {
		cells := make([]string, cols)
		for i, c := range r.Children {
			cells[i] = escapeCellPipes(m.cellInlines(c.Children))
		}
		m.s(prefix + "| " + strings.Join(cells, " | ") + " |\n")
	}
	rows := t.Children
	if len(rows) > 0 && rows[0].Header {
		row(rows[0])
		rows = rows[1:]
	} else {
		row(Node{Kind: KindRow})
	}
	sep := make([]string, cols)
	for i := range sep {
		a := ""
		if i < len(t.Align) {
			a = t.Align[i]
		}
		switch a {
		case "left":
			sep[i] = ":---"
		case "center":
			sep[i] = ":---:"
		case "right":
			sep[i] = "---:"
		default:
			sep[i] = "---"
		}
	}
	m.s(prefix + "| " + strings.Join(sep, " | ") + " |\n")
	for _, r := range rows {
		row(r)
	}
}

func (m *mdWriter) image(n Node) string {
	p := m.opts.ImagePaths[n.Asset]
	if p == "" {
		p = path.Join(m.opts.AssetDir, n.Asset)
	}
	return "![" + escapeAlt(n.Alt) + "](<" + escapeDest(p) + ">)"
}

// cellInlines renders a table cell's inline nodes on one line. A line break
// is written as <br>, the one break a GFM cell can hold (Markdown's own form
// ends the row), which the builder reads back as a break.
func (m *mdWriter) cellInlines(ns []Node) string {
	var parts []string
	start := 0
	for i, n := range ns {
		if n.Kind == KindBreak {
			parts = append(parts, oneLine(m.inlines(ns[start:i])))
			start = i + 1
		}
	}
	parts = append(parts, oneLine(m.inlines(ns[start:])))
	return strings.Join(parts, "<br>")
}

// inlines renders inline nodes as one escaped line of Markdown.
func (m *mdWriter) inlines(ns []Node) string {
	var b strings.Builder
	for _, n := range ns {
		switch n.Kind {
		case KindText:
			b.WriteString(escapeInline(n.Text))
		case KindBreak:
			b.WriteString("\\\n")
		case KindEmph:
			b.WriteString("*" + m.inlines(n.Children) + "*")
		case KindStrong:
			b.WriteString("**" + m.inlines(n.Children) + "**")
		case KindStrike:
			b.WriteString("~~" + m.inlines(n.Children) + "~~")
		case KindCodeSpan:
			b.WriteString(codeSpan(n.Text))
		case KindLink:
			dest := n.Href
			if n.Anchor != "" {
				dest = "#" + n.Anchor
			}
			b.WriteString("[" + m.inlines(n.Children) + "](<" + escapeDest(dest) + ">)")
		case KindImage:
			b.WriteString(m.image(n))
		}
	}
	return b.String()
}

// escapeCellPipes escapes the pipes text escaping left bare (inside a code
// span), which would split a table cell; GFM reads \| there as a pipe.
func escapeCellPipes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && (i == 0 || s[i-1] != '\\') {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// codeSpan wraps text in enough backticks that none inside closes it.
func codeSpan(t string) string {
	t = strings.ReplaceAll(t, "\n", " ")
	ticks := "`"
	for strings.Contains(t, ticks) {
		ticks += "`"
	}
	if strings.HasPrefix(t, "`") || strings.HasSuffix(t, "`") || (strings.HasPrefix(t, " ") && strings.HasSuffix(t, " ") && strings.TrimSpace(t) != "") {
		t = " " + t + " "
	}
	return ticks + t + ticks
}

// mdPunct are the characters escaped in text. CommonMark lets any ASCII
// punctuation be backslash-escaped, so escaping more than is needed is safe.
var mdPunct = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`,
	"#", `\#`, "!", `\!`, "|", `\|`, "~", `\~`, "&", `\&`, "\n", " ", "\r", " ",
)

func escapeInline(s string) string { return mdPunct.Replace(s) }

// lineStartRe matches what makes a block of a line's start once the inline
// escapes are done: a list marker, an ordered marker, a setext underline.
var lineStartRe = regexp.MustCompile(`^( {0,3})([-+=]|\d{1,9}([.)]))`)

// escapeLineStart escapes a paragraph's first characters where they would
// start a list or a heading; text after a break starts a line too.
func escapeLineStart(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if m := lineStartRe.FindStringSubmatchIndex(l); m != nil {
			// The marker is the last character of the match.
			at := m[1] - 1
			l = l[:at] + `\` + l[at:]
		}
		lines[i] = strings.TrimLeft(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// escapeDest makes a link destination safe inside <...>.
func escapeDest(d string) string {
	return strings.NewReplacer("<", "%3C", ">", "%3E", "\n", "", "\r", "", " ", "%20").Replace(d)
}

func escapeAlt(s string) string {
	return escapeInline(oneLine(s))
}

func clampLevel(l int) int {
	if l < 1 {
		return 1
	}
	if l > 6 {
		return 6
	}
	return l
}

// usedAnchors are the anchors a link or the table of contents points at.
func usedAnchors(d *Doc, opts Options) map[string]bool {
	used := map[string]bool{}
	var sections func(ss []*Section)
	var nodes func(ns []Node, depth int)
	nodes = func(ns []Node, depth int) {
		if depth > maxDepth {
			return
		}
		for _, n := range ns {
			if n.Kind == KindLink && n.Anchor != "" {
				used[n.Anchor] = true
			}
			nodes(n.Children, depth+1)
		}
	}
	sections = func(ss []*Section) {
		for _, s := range ss {
			if opts.TOC {
				used[s.Anchor] = true
			}
			nodes(s.Blocks, 0)
			sections(s.Children)
		}
	}
	nodes(d.Blocks, 0)
	sections(d.Sections)
	return used
}

// capWriter stops at MaxOutput, or when its context ends, and remembers the
// first error.
type capWriter struct {
	w   io.Writer
	ctx context.Context
	n   int
	err error
}

func (c *capWriter) WriteString(s string) {
	if c.err != nil {
		return
	}
	if c.ctx != nil {
		if c.err = c.ctx.Err(); c.err != nil {
			return
		}
	}
	if c.n+len(s) > MaxOutput {
		c.err = ErrTooLarge
		return
	}
	c.n += len(s)
	_, c.err = io.WriteString(c.w, s)
}

// Write lets an encoder stream into the capped writer.
func (c *capWriter) Write(p []byte) (int, error) {
	c.WriteString(string(p))
	if c.err != nil {
		return 0, c.err
	}
	return len(p), nil
}
