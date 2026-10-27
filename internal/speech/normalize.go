package speech

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Notes are the short spoken sentences that stand in for content an engine
// cannot read aloud. A zero field takes the English default, so a caller
// passes only the notes it translates.
type Notes struct {
	CodeBlock string // replaces a fenced or indented code block
	Table     string // replaces a table
}

// DefaultNotes are the English notes Normalize uses.
func DefaultNotes() Notes {
	return Notes{CodeBlock: "A code block is skipped.", Table: "A table is skipped."}
}

func (n Notes) withDefaults() Notes {
	d := DefaultNotes()
	if strings.TrimSpace(n.CodeBlock) != "" {
		d.CodeBlock = n.CodeBlock
	}
	if strings.TrimSpace(n.Table) != "" {
		d.Table = n.Table
	}
	return d
}

// Normalize is NormalizeWith(markdown, DefaultNotes()).
func Normalize(markdown string) string {
	return NormalizeWith(markdown, Notes{})
}

// NormalizeWith turns Markdown into plain text an engine reads aloud without
// saying "asterisk" or spelling out a URL. The result is paragraphs separated
// by one blank line ("\n\n"), each on one line with single spaces — the shape
// Split cuts.
//
//   - A heading (ATX or setext) becomes its own paragraph ending in sentence
//     punctuation, so the engine pauses after it.
//   - A list item becomes a sentence; the items of one list share a paragraph.
//     A task-list box ("[ ]", "[x]") is dropped.
//   - Emphasis, strong, strikethrough and code-span markers are dropped and
//     their text kept. A '*' or '_' between spaces (2 * 3) and an underscore
//     inside a word (snake_case) are text, not markers, and stay.
//   - A link or image keeps its text (alt text) and drops its destination;
//     a reference link keeps its text; a footnote reference is dropped.
//     Link reference definitions are dropped.
//   - A bare URL (http://, https://, www.) and an autolink (<https://…>) are
//     dropped entirely: an engine reads one character by character, and a
//     spoken word in its place would be a language-specific guess.
//   - A fenced or indented code block becomes notes.CodeBlock; a GFM table
//     (a header row, then a delimiter row) becomes notes.Table.
//   - HTML tags and comments are stripped (block-level tags leave a space)
//     and character references decoded (&amp; → &).
//   - Blockquote markers are dropped; a thematic break ends a paragraph.
//   - Whitespace collapses to single spaces; letters are untouched, so
//     Ukrainian text and its combining stress mark (U+0301) pass through.
//
// Deliberately not handled: numbers, dates and abbreviations (left to the
// engine); emoji and :shortcodes:; math; footnote definitions beyond
// dropping their label; the contents of <script>/<style> (tags stripped,
// text kept); nested-construct corner cases of CommonMark (an unmatched '*'
// next to a word is dropped like a marker; a code fence inside a blockquote
// closes only at a quoted fence). The goal is speakable text, not a
// conforming parser.
func NormalizeWith(markdown string, notes Notes) string {
	notes = notes.withDefaults()
	text := strings.ReplaceAll(markdown, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = stripHTMLComments(text)
	lines := strings.Split(text, "\n")

	var n normalizer
	for i := 0; i < len(lines); i++ {
		quoted, line := stripQuote(lines[i])
		indent, body := leadingIndent(line)
		blank := n.blank
		n.blank = false

		// A fenced code block runs to a closing fence of the same character
		// and at least the opening's length, or to the end of the text.
		if fc, flen, ok := fenceOpen(body); ok && (indent < 4 || n.inList) {
			n.flush()
			n.note(notes.CodeBlock)
			for i+1 < len(lines) {
				next := lines[i+1]
				if quoted {
					_, next = stripQuote(next)
				}
				i++
				if fenceCloses(strings.TrimLeft(next, " \t"), fc, flen) {
					break
				}
			}
			continue
		}

		if strings.TrimSpace(body) == "" {
			n.flush()
			n.blank = true
			continue
		}
		if i+1 < len(lines) && strings.Contains(body, "|") {
			_, next := stripQuote(lines[i+1])
			if isTableDelimiter(next) {
				n.flush()
				n.note(notes.Table)
				i++ // the delimiter row
				for i+1 < len(lines) {
					_, row := stripQuote(lines[i+1])
					_, rowBody := leadingIndent(row)
					if strings.TrimSpace(rowBody) == "" || atxHeading(rowBody) != nil {
						break
					}
					if _, _, ok := fenceOpen(rowBody); ok {
						break
					}
					i++
				}
				continue
			}
		}

		if indent < 4 || n.inList {
			if h := atxHeading(body); h != nil {
				n.flush()
				n.inList = false
				n.heading(*h)
				continue
			}
			if n.kind == unitParagraph && len(n.buf) > 0 && indent < 4 && isSetextUnderline(body) {
				text := strings.Join(n.buf, " ")
				n.buf = nil
				n.heading(text)
				continue
			}
			if isThematicBreak(body) {
				n.flush()
				n.inList = false
				continue
			}
			if isLinkDefinition(body) {
				n.flush()
				continue
			}
			if item, ok := listItem(body); ok {
				n.flush()
				n.inList = true
				n.kind = unitListItem
				n.buf = append(n.buf, item)
				continue
			}
		}

		if indent >= 4 && len(n.buf) == 0 && !n.inList {
			// An indented code block: it runs over indented and blank lines.
			n.note(notes.CodeBlock)
			for i+1 < len(lines) {
				_, next := stripQuote(lines[i+1])
				ni, nb := leadingIndent(next)
				if strings.TrimSpace(nb) != "" && ni < 4 {
					break
				}
				i++
			}
			continue
		}

		// Text: it continues the open paragraph or list item (a lazy
		// continuation line), else it starts a paragraph. After a blank line
		// an unindented line ends the list.
		if n.inList && blank && indent == 0 {
			n.inList = false
		}
		if len(n.buf) == 0 {
			n.kind = unitParagraph
			if n.inList {
				n.kind = unitListItem
			}
		}
		n.buf = append(n.buf, strings.TrimSpace(body))
	}
	n.flush()
	return n.result()
}

type unitKind int

const (
	unitParagraph unitKind = iota
	unitListItem
	unitHeading
	unitNote
)

// normalizer collects the speakable units of a document in order.
type normalizer struct {
	buf    []string // lines of the open paragraph or list item
	kind   unitKind // what buf is
	inList bool     // inside a list (nested items, continuations)
	blank  bool     // the previous line was blank
	units  []unit
}

type unit struct {
	kind unitKind
	text string
}

func (n *normalizer) flush() {
	if len(n.buf) == 0 {
		return
	}
	text := tidy(inline(strings.Join(n.buf, "\n")))
	n.buf = nil
	if text == "" {
		return
	}
	if n.kind == unitListItem {
		text = sentence(text)
	}
	n.units = append(n.units, unit{kind: n.kind, text: text})
}

func (n *normalizer) heading(raw string) {
	if text := tidy(inline(raw)); text != "" {
		n.units = append(n.units, unit{kind: unitHeading, text: sentence(text)})
	}
}

func (n *normalizer) note(text string) {
	n.units = append(n.units, unit{kind: unitNote, text: collapse(text)})
}

// result joins the units: the items of one list share a paragraph, every
// other unit is its own.
func (n *normalizer) result() string {
	var b strings.Builder
	for i, u := range n.units {
		if i > 0 {
			if u.kind == unitListItem && n.units[i-1].kind == unitListItem {
				b.WriteByte(' ')
			} else {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(u.text)
	}
	return b.String()
}

// collapse folds every run of whitespace to one space and trims the ends.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// tidy collapses whitespace and removes a space left before punctuation
// where dropped markup or a dropped URL stood ("see <url>." → "see.").
func tidy(s string) string {
	s = collapse(s)
	for _, p := range []string{".", ",", ";", ":", "!", "?", "…"} {
		s = strings.ReplaceAll(s, " "+p, p)
	}
	return s
}

// sentence ends s with sentence punctuation unless it already ends with some
// (looking through closing quotes and brackets).
func sentence(s string) string {
	t := strings.TrimRight(s, "\"'”’»)]")
	r, _ := utf8.DecodeLastRuneInString(t)
	switch r {
	case '.', '!', '?', '…', ':', ';':
		return s
	}
	return s + "."
}

// stripQuote removes blockquote markers ("> ", ">>", "> > ") from a line and
// reports whether there were any.
func stripQuote(line string) (bool, string) {
	quoted := false
	for {
		indent, body := leadingIndent(line)
		if indent > 3 || !strings.HasPrefix(body, ">") {
			return quoted, line
		}
		quoted = true
		line = strings.TrimPrefix(body[1:], " ")
	}
}

// leadingIndent is a line's indentation in columns (a tab counts to the next
// multiple of 4) and the rest of the line.
func leadingIndent(line string) (int, string) {
	col := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, line[i:]
		}
	}
	return col, ""
}

func fenceOpen(body string) (byte, int, bool) {
	if len(body) < 3 || (body[0] != '`' && body[0] != '~') {
		return 0, 0, false
	}
	c := body[0]
	n := 0
	for n < len(body) && body[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	// A backtick fence's info string cannot contain a backtick (that is an
	// inline code span instead).
	if c == '`' && strings.IndexByte(body[n:], '`') >= 0 {
		return 0, 0, false
	}
	return c, n, true
}

func fenceCloses(body string, c byte, minLen int) bool {
	n := 0
	for n < len(body) && body[n] == c {
		n++
	}
	return n >= minLen && strings.TrimSpace(body[n:]) == ""
}

// atxHeading returns the heading text of "# Title" (with an optional closing
// run of '#'), or nil when body is not an ATX heading.
func atxHeading(body string) *string {
	n := 0
	for n < len(body) && body[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(body) && body[n] != ' ' && body[n] != '\t') {
		return nil
	}
	text := strings.TrimSpace(body[n:])
	if t := strings.TrimRight(text, "#"); t != text && (t == "" || strings.HasSuffix(t, " ") || strings.HasSuffix(t, "\t")) {
		text = strings.TrimSpace(t)
	}
	return &text
}

func isSetextUnderline(body string) bool {
	t := strings.TrimRight(body, " \t")
	if t == "" || (t[0] != '=' && t[0] != '-') {
		return false
	}
	return strings.Trim(t, t[:1]) == ""
}

// isThematicBreak reports a line of three or more '-', '*' or '_' (spaces
// allowed between them) and nothing else.
func isThematicBreak(body string) bool {
	var c byte
	n := 0
	for i := 0; i < len(body); i++ {
		switch b := body[i]; b {
		case ' ', '\t':
		case '-', '*', '_':
			if c != 0 && b != c {
				return false
			}
			c = b
			n++
		default:
			return false
		}
	}
	return n >= 3
}

// isLinkDefinition reports a link reference definition, "[label]: url".
// A footnote definition ("[^1]: text") is not one: its text is content.
func isLinkDefinition(body string) bool {
	if !strings.HasPrefix(body, "[") || strings.HasPrefix(body, "[^") {
		return false
	}
	end := strings.Index(body, "]:")
	return end > 1 && !strings.ContainsAny(body[1:end], "[]") && strings.TrimSpace(body[end+2:]) != ""
}

// isTableDelimiter reports a GFM table delimiter row, "| --- | :-: |". It
// must contain a pipe: a bare "---" is a setext underline or a break.
func isTableDelimiter(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return false
	}
	t = strings.TrimSuffix(strings.TrimPrefix(t, "|"), "|")
	cells := strings.Split(t, "|")
	for _, c := range cells {
		c = strings.TrimSpace(c)
		c = strings.TrimSuffix(strings.TrimPrefix(c, ":"), ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// listItem returns the text of a bullet ("- ", "* ", "+ ") or ordered ("1. ",
// "1) ") list item, without a task-list box.
func listItem(body string) (string, bool) {
	rest := ""
	switch {
	case body == "-" || body == "*" || body == "+": // an empty item
		return "", true
	case len(body) >= 2 && strings.ContainsRune("-*+", rune(body[0])) && (body[1] == ' ' || body[1] == '\t'):
		rest = body[2:]
	default:
		n := 0
		for n < len(body) && n < 9 && body[n] >= '0' && body[n] <= '9' {
			n++
		}
		// "2026." alone stays text: an empty ordered item is too easily a
		// number on its own line.
		if n == 0 || n+1 >= len(body) || (body[n] != '.' && body[n] != ')') || (body[n+1] != ' ' && body[n+1] != '\t') {
			return "", false
		}
		rest = body[n+2:]
	}
	rest = strings.TrimSpace(rest)
	for _, box := range []string{"[ ] ", "[x] ", "[X] "} {
		rest = strings.TrimPrefix(rest, box)
	}
	return rest, true
}

// stripHTMLComments removes every terminated <!-- … --> (across lines). An
// unterminated opener is left for the tag stripper.
func stripHTMLComments(s string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(s[start+4:], "-->")
		if end < 0 {
			break
		}
		b.WriteString(s[:start])
		b.WriteByte(' ')
		s = s[start+4+end+3:]
	}
	b.WriteString(s)
	return b.String()
}

// maxLinkDestination bounds how far a link destination is scanned, so text
// full of unclosed "[x](" stays linear.
const maxLinkDestination = 2048

// inline drops inline Markdown and HTML markup from s, keeping its text.
func inline(s string) string {
	codeEnd := codeSpans(s)
	jump := linkJumps(s, codeEnd)

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if end, ok := codeEnd[i]; ok {
			// A code span: its text is literal. CommonMark strips one space
			// from each side; collapse handles the rest.
			n := runLen(s, i, '`')
			b.WriteByte(' ')
			b.WriteString(s[i+n : end])
			b.WriteByte(' ')
			i = end + n
			continue
		}
		if to, ok := jump[i]; ok {
			i = to
			continue
		}
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) && isASCIIPunct(s[i+1]) {
				b.WriteByte(s[i+1])
				i += 2
				continue
			}
			if i+1 == len(s) || s[i+1] == '\n' { // a hard line break
				b.WriteByte(' ')
				i++
				continue
			}
		case '`':
			i += runLen(s, i, '`') // an unmatched backtick run
			continue
		case '!':
			if i+1 < len(s) && s[i+1] == '[' {
				i++ // an image: its alt text is kept like a link's text
				continue
			}
		case '[', ']':
			i++ // brackets of link text, or of plain bracketed text
			continue
		case '*', '_', '~':
			n := runLen(s, i, c)
			if isEmphasisRun(s, i, n, c) {
				i += n
				continue
			}
			b.WriteString(s[i : i+n])
			i += n
			continue
		case '<':
			if end, block, ok := htmlTag(s, i); ok {
				if block {
					b.WriteByte(' ')
				}
				i = end
				continue
			}
		case 'h', 'w', 'H', 'W':
			if end, ok := bareURL(s, i); ok {
				i = end
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return html.UnescapeString(b.String())
}

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

// codeSpans maps the start of each code span's opening backtick run to the
// start of its closing run (of the same length).
func codeSpans(s string) map[int]int {
	spans := map[int]int{}
	noCloser := map[int]bool{} // run lengths with no closer after the scan point
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '`' {
			i += 2
			continue
		}
		if s[i] != '`' {
			i++
			continue
		}
		n := runLen(s, i, '`')
		if noCloser[n] {
			i += n
			continue
		}
		closed := false
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := runLen(s, j, '`')
			if m == n {
				spans[i] = j
				i = j + m
				closed = true
				break
			}
			j += m
		}
		if !closed {
			noCloser[n] = true
			i += n
		}
	}
	return spans
}

// linkJumps maps the closing ']' of each link's text to the index just past
// its destination — "(url "title")" or a reference "[ref]" — so the scanner
// skips the destination. A footnote reference "[^1]" is skipped whole.
func linkJumps(s string, code map[int]int) map[int]int {
	jumps := map[int]int{}
	var open []int
	for i := 0; i < len(s); i++ {
		if end, ok := code[i]; ok {
			i = end + runLen(s, end, '`') - 1
			continue
		}
		switch s[i] {
		case '\\':
			i++
		case '[':
			open = append(open, i)
		case ']':
			if len(open) == 0 {
				continue
			}
			start := open[len(open)-1]
			open = open[:len(open)-1]
			if strings.HasPrefix(s[start:], "[^") {
				jumps[start] = i + 1
				continue
			}
			if end, ok := linkDestination(s, i+1); ok {
				jumps[i] = end
			}
		}
	}
	return jumps
}

// linkDestination returns the index past "(dest "title")" or "[ref]"
// starting at i, if one is there.
func linkDestination(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	limit := min(len(s), i+maxLinkDestination)
	switch s[i] {
	case '[':
		for j := i + 1; j < limit; j++ {
			switch s[j] {
			case ']':
				return j + 1, true
			case '[', '\n':
				return 0, false
			}
		}
	case '(':
		depth := 0
		quote := byte(0)
		for j := i + 1; j < limit; j++ {
			c := s[j]
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				}
			case c == '\\':
				j++
			case c == '"' || c == '\'':
				quote = c
			case c == '(':
				depth++
			case c == ')':
				if depth == 0 {
					return j + 1, true
				}
				depth--
			case c == '\n':
				return 0, false
			}
		}
	}
	return 0, false
}

// isEmphasisRun reports whether the run of n c's at i is an emphasis or
// strikethrough marker: it touches text on one side (it flanks a word). A
// run with whitespace (or nothing) on both sides is text ("2 * 3"), as is an
// '_' run inside a word ("snake_case") and a '~' run longer than two.
func isEmphasisRun(s string, i, n int, c byte) bool {
	if c == '~' && n > 2 {
		return false
	}
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[i+n:])
	spaceBefore := i == 0 || unicode.IsSpace(before)
	spaceAfter := i+n == len(s) || unicode.IsSpace(after)
	if spaceBefore && spaceAfter {
		return false
	}
	if c == '_' && isWordRune(before) && isWordRune(after) && i > 0 && i+n < len(s) {
		return false
	}
	return true
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// isBlockTag reports the HTML elements whose tags separate words; any other
// tag is dropped without a space so "<b>bold</b>er" stays one word.
func isBlockTag(name string) bool {
	switch name {
	case "address", "article", "aside", "blockquote", "br", "dd", "div", "dl", "dt",
		"figcaption", "figure", "footer", "h1", "h2", "h3", "h4", "h5", "h6", "header",
		"hr", "li", "ol", "p", "pre", "section", "table", "td", "th", "tr", "ul":
		return true
	}
	return false
}

// htmlTag recognises an HTML tag, declaration or autolink at i ("<b>",
// "</p>", "<br/>", "<!DOCTYPE …>", "<https://…>", "<a@b.c>") and returns the
// index past it and whether it should leave a space. Something that only
// looks like a tag — "a < b", "<3" — is not one.
func htmlTag(s string, i int) (int, bool, bool) {
	if i+1 >= len(s) {
		return 0, false, false
	}
	j := i + 1
	if s[j] == '/' || s[j] == '!' || s[j] == '?' {
		j++
	}
	if j >= len(s) || !(s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
		return 0, false, false
	}
	nameStart := j
	for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9' || s[j] == '-') {
		j++
	}
	name := strings.ToLower(s[nameStart:j])
	end := strings.IndexAny(s[j:], "<>")
	if end < 0 || s[j+end] == '<' {
		return 0, false, false
	}
	return j + end + 1, isBlockTag(name), true
}

// bareURL recognises a URL starting a word at i (http://, https://, www.) and
// returns the index past it. Trailing punctuation that ends the sentence
// ("see https://x.org.") is left in the text.
func bareURL(s string, i int) (int, bool) {
	if i > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:i]); isWordRune(r) {
			return 0, false
		}
	}
	rest := s[i:]
	lower := strings.ToLower(rest[:min(len(rest), 8)])
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "www.") {
		return 0, false
	}
	end := i
	for end < len(s) {
		r, size := utf8.DecodeRuneInString(s[end:])
		if unicode.IsSpace(r) || r == '<' || r == '>' {
			break
		}
		end += size
	}
	for end > i && strings.IndexByte(".,;:!?'\")]*_~", s[end-1]) >= 0 {
		end--
	}
	return end, true
}
