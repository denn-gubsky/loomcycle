package extract

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Heading recovery for HTML (Options.InferHeadings). Formatting is read from
// <b> and <strong>, from simple class rules in the page's <style> blocks and
// from inline style attributes (font-weight, font-size, color), inherited
// down the tree as a browser does. An editor's HTML export may write all of
// its formatting as class rules (`.c4{font-weight:700}` and
// `<span class="c4">`), so without them such a page has no formatting to
// find headings by.

// htmlBodyPt is a browser's default font size: 16px.
const htmlBodyPt = 12

// walkStyled walks an element's children under the formatting it sets. A
// list item's text is never a heading.
func (h *htmlState) walkStyled(n *html.Node, depth int, li bool) error {
	if !h.infer {
		return h.walk(n, depth+1)
	}
	saved := h.cur
	h.cur = htmlStyled(n, saved, h.sheet)
	if li {
		h.inList++
	}
	err := h.walk(n, depth+1)
	if li {
		h.inList--
	}
	h.cur = saved
	return err
}

// htmlStyled is the formatting text inside n has, given its parent's: the
// element's own meaning (<b>), then its class rules, then its inline style,
// as a browser cascades them.
func htmlStyled(n *html.Node, parent runFmt, sheet cssSheet) runFmt {
	f := parent
	switch n.Data {
	case "b", "strong":
		f.bold = true
	case "font":
		if c := attrOf(n, "color"); c != "" {
			f.color = cssColor(c, parent.color)
		}
	}
	classes, _ := sheet.match(n)
	f = classes.apply(f, parent)
	return parseDecls(attrOf(n, "style"), 0).apply(f, parent)
}

// cssProp is one property's value as a declaration block set it, and where
// its rule stood in the sheets (a later rule wins). set is false when no
// rule set it.
type cssProp struct {
	val   string
	order int
	set   bool
}

// over is p, or q when q was set by a later rule.
func (p cssProp) over(q cssProp) cssProp {
	if q.set && (!p.set || q.order > p.order) {
		return q
	}
	return p
}

// cssDecls is what a declaration block says about font-weight, font-size and
// color, read once: applying it to an element is a few comparisons, never a
// new read of the block's text.
type cssDecls struct {
	bold, size, color cssProp
}

func (d cssDecls) over(e cssDecls) cssDecls {
	return cssDecls{bold: d.bold.over(e.bold), size: d.size.over(e.size), color: d.color.over(e.color)}
}

// parseDecls reads a declaration block standing at order in the sheets. A
// block is someone else's text: only its first maxCSSDeclBytes and the
// first few declarations are read. A value that cannot be read sets
// nothing, as a browser drops an invalid declaration.
func parseDecls(block string, order int) cssDecls {
	var d cssDecls
	if block == "" {
		return d
	}
	block = block[:min(len(block), maxCSSDeclBytes)]
	for i, decl := range strings.SplitN(block, ";", 64) {
		if i == 63 {
			break
		}
		prop, val, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		val = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), "!important")))
		switch prop {
		case "font-weight":
			if b, ok := cssBold(val); ok {
				v := ""
				if b {
					v = "bold"
				}
				d.bold = cssProp{val: v, order: order, set: true}
			}
		case "font-size":
			// Validity does not depend on the parent's size; the value is
			// resolved against it when applied.
			if _, ok := cssSize(val, htmlBodyPt); ok {
				d.size = cssProp{val: val, order: order, set: true}
			}
		case "color":
			if len(val) <= 64 {
				d.color = cssProp{val: val, order: order, set: true}
			}
		}
	}
	return d
}

// apply sets what d declares on f; a relative size is relative to the
// parent's, and inherit keeps the parent's colour.
func (d cssDecls) apply(f, parent runFmt) runFmt {
	if d.bold.set {
		f.bold = d.bold.val != ""
	}
	if d.size.set {
		if s, ok := cssSize(d.size.val, parent.size); ok {
			f.size = s
		}
	}
	if d.color.set {
		f.color = cssColor(d.color.val, parent.color)
	}
	return f
}

// cssBold reads a font-weight; ok is false for a value it cannot read.
func cssBold(v string) (bold, ok bool) {
	switch v {
	case "bold", "bolder":
		return true, true
	case "normal", "lighter":
		return false, true
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n >= 600, true
	}
	return false, false
}

var cssSizeKeywords = map[string]float64{
	"xx-small": 7, "x-small": 7.5, "small": 10, "medium": 12, "large": 13.5,
	"x-large": 18, "xx-large": 24, "xxx-large": 36,
}

// cssSize reads a font-size in points: pt, px, em, rem, %, or a keyword.
// Anything else, or a size no text has, is not read.
func cssSize(v string, parent float64) (float64, bool) {
	if parent <= 0 {
		parent = htmlBodyPt
	}
	if k, ok := cssSizeKeywords[v]; ok {
		return k, true
	}
	switch v {
	case "smaller":
		return parent / 1.2, true
	case "larger":
		return parent * 1.2, true
	}
	scale := 0.0
	for _, u := range []struct {
		suffix string
		scale  float64
	}{{"rem", htmlBodyPt}, {"em", parent}, {"pt", 1}, {"px", 0.75}, {"%", parent / 100}} {
		if strings.HasSuffix(v, u.suffix) {
			v, scale = strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), u.scale
			break
		}
	}
	if scale == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	pt := n * scale
	if pt <= 0 || pt > 2000 {
		return 0, false
	}
	return pt, true
}

// cssColor reads a colour: hex, rgb()/rgba(), or a name, normalised as
// normalizeColor does; inherit and currentColor keep the parent's.
func cssColor(v, parent string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "inherit", "currentcolor", "initial", "unset", "":
		return parent
	}
	if strings.HasPrefix(v, "rgb(") || strings.HasPrefix(v, "rgba(") {
		inner := v[strings.IndexByte(v, '(')+1:]
		inner = strings.TrimSuffix(inner, ")")
		parts := strings.FieldsFunc(inner, func(r rune) bool { return r == ',' || r == ' ' || r == '/' })
		if len(parts) < 3 {
			return parent
		}
		var rgb [3]int
		for i := range 3 {
			n, err := strconv.ParseFloat(strings.TrimSuffix(parts[i], "%"), 64)
			if err != nil || math.IsNaN(n) {
				return parent
			}
			if strings.HasSuffix(parts[i], "%") {
				n = n * 255 / 100
			}
			rgb[i] = int(math.Round(math.Max(0, math.Min(255, n))))
		}
		return normalizeColor(fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2]))
	}
	if len(v) > 64 {
		return parent
	}
	return normalizeColor(v)
}

// imageSrc is what names a picture on a page: its source, as written.
func imageSrc(n *html.Node) string { return strings.TrimSpace(attrOf(n, "src")) }

// countImages counts the pictures the page walk writes, by source: those
// outside the parts it drops (scripts, forms) and those it reads as text
// (headings, lists, tables, preformatted blocks). budget bounds the nodes
// visited, as the walk's own element cap does.
func countImages(n *html.Node, counts decorative, budget *int, depth int) {
	if depth > maxXMLDepth {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if *budget--; *budget < 0 {
			return
		}
		if c.Type != html.ElementNode {
			continue
		}
		switch tag := c.Data; {
		case htmlSkip[tag], tag == "table", tag == "ul", tag == "ol", tag == "pre",
			len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6':
		case tag == "img":
			counts[imageSrc(c)]++
		default:
			countImages(c, counts, budget, depth+1)
		}
	}
}

const (
	// maxCSSBytes caps the style sheet text read from a page's <style>
	// blocks, all together.
	maxCSSBytes = 256 << 10
	// maxCSSParsed caps the rules parsed from them, of any selector.
	maxCSSParsed = 16384
	// maxCSSRules caps the class selectors kept from them.
	maxCSSRules = 4096
	// maxCSSDeclBytes caps the text read from one declaration block.
	maxCSSDeclBytes = 4 << 10
	// maxCSSClasses caps the distinct classes read from one class
	// attribute.
	maxCSSClasses = 16
	// maxCSSWork caps the class rule sets applied over a whole page. Each
	// element applies at most two per distinct class (the class's and the
	// tag's), so a page past this has class rules on hundreds of
	// thousands of elements; heading inference is then not applied at all.
	maxCSSWork = 1 << 20
)

// cssSheet is the class rules of a page's style sheets that the inference
// reads: a rule whose selector is a single class, alone (`.c4`) or on a tag
// (`p.c4`). Any other selector (a descendant, a compound, an id, a
// pseudo-class, a bare tag) and anything inside an at-rule is ignored: a
// page's own heading formatting is what the class-styled exports write,
// and reading more would mean writing a browser.
//
// The rules of one class on one tag are merged as the sheet is read, a
// later rule's property over an earlier's, so a sheet that repeats a rule
// costs nothing more per element than one that states it once.
type cssSheet map[cssKey]cssDecls

// cssKey is a class, on a tag ("" for any).
type cssKey struct{ class, tag string }

// readStyleSheets collects the class rules of every <style> element under
// n, in document order, within maxCSSBytes of sheet text. budget bounds the
// nodes visited.
func readStyleSheets(n *html.Node) cssSheet {
	sheet := cssSheet{}
	var b strings.Builder
	budget := maxXMLNodes
	var walk func(*html.Node, int)
	walk = func(m *html.Node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			if budget--; budget < 0 || b.Len() >= maxCSSBytes {
				return
			}
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data == "style" {
				for t := c.FirstChild; t != nil && b.Len() < maxCSSBytes; t = t.NextSibling {
					if t.Type == html.TextNode {
						b.WriteString(t.Data[:min(len(t.Data), maxCSSBytes-b.Len())])
					}
				}
				// One sheet's last rule never runs into the next's.
				b.WriteString("\n}\n")
				continue
			}
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	sheet.parse(b.String())
	return sheet
}

// parse reads class rules from css: at most maxCSSParsed rules, of which
// at most maxCSSRules class selectors are kept.
func (s cssSheet) parse(css string) {
	css = stripCSSComments(css)
	kept := 0
	for parsed := 0; css != "" && parsed < maxCSSParsed && kept < maxCSSRules; parsed++ {
		open := strings.IndexByte(css, '{')
		if open < 0 {
			return
		}
		sel := strings.TrimSpace(css[:open])
		// An at-rule without a block (@import, @charset) ends at its ';'.
		for strings.HasPrefix(sel, "@") {
			semi := strings.IndexByte(sel, ';')
			if semi < 0 {
				break
			}
			sel = strings.TrimSpace(sel[semi+1:])
		}
		if strings.HasPrefix(sel, "@") {
			css = css[skipCSSBlock(css, open):]
			continue
		}
		end := strings.IndexByte(css[open:], '}')
		if end < 0 {
			return
		}
		block := css[open+1 : open+end]
		css = css[open+end+1:]
		// A stray '}' (an unbalanced sheet) leaves its text in front of
		// the next selector: only what follows the last one is a selector.
		if i := strings.LastIndexByte(sel, '}'); i >= 0 {
			sel = strings.TrimSpace(sel[i+1:])
		}
		var d cssDecls
		read := false
		for part := range strings.SplitSeq(sel, ",") {
			tag, class, ok := simpleClassSelector(strings.TrimSpace(part))
			if !ok {
				continue
			}
			if !read {
				// Read once per rule, whatever its selector list names.
				d, read = parseDecls(block, parsed), true
			}
			k := cssKey{class, tag}
			s[k] = s[k].over(d)
			if kept++; kept >= maxCSSRules {
				return
			}
		}
	}
}

// skipCSSBlock returns the offset just past the block opening at open,
// nested blocks included (an @media's rules).
func skipCSSBlock(css string, open int) int {
	depth := 0
	for i := open; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return len(css)
}

func stripCSSComments(css string) string {
	var b strings.Builder
	for {
		i := strings.Index(css, "/*")
		if i < 0 {
			b.WriteString(css)
			return b.String()
		}
		b.WriteString(css[:i])
		j := strings.Index(css[i+2:], "*/")
		if j < 0 {
			return b.String()
		}
		b.WriteByte(' ')
		css = css[i+2+j+2:]
	}
}

// simpleClassSelector splits `.c4` or `p.c4` into its tag ("" for any) and
// class; any other selector is not one.
func simpleClassSelector(sel string) (tag, class string, ok bool) {
	tag, class, ok = strings.Cut(sel, ".")
	if !ok || class == "" || !cssIdent(class) || (tag != "" && !cssTag(tag)) {
		return "", "", false
	}
	return strings.ToLower(tag), class, true
}

// cssIdent is a class name with nothing that would make its selector more
// than one class: letters, digits, '-' and '_'.
func cssIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func cssTag(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// match is what n's class rules declare, in cascade order: a tag.class rule
// over a .class rule (the more specific wins), a later rule over an earlier
// within each. Only the first maxCSSClasses distinct classes are read.
// applied counts the rule sets found, the work the cascade did.
func (s cssSheet) match(n *html.Node) (d cssDecls, applied int) {
	if len(s) == 0 {
		return d, 0
	}
	cls := attrOf(n, "class")
	if cls == "" {
		return d, 0
	}
	var plain, tagged cssDecls
	var seen [maxCSSClasses]string
	distinct := 0
	for c := range strings.FieldsSeq(cls) {
		if slices.Contains(seen[:distinct], c) {
			continue
		}
		if distinct == maxCSSClasses {
			break
		}
		seen[distinct] = c
		distinct++
		if r, ok := s[cssKey{c, ""}]; ok {
			plain = plain.over(r)
			applied++
		}
		if r, ok := s[cssKey{c, n.Data}]; ok {
			tagged = tagged.over(r)
			applied++
		}
	}
	return cssDecls{
		bold:  tagOver(plain.bold, tagged.bold),
		size:  tagOver(plain.size, tagged.size),
		color: tagOver(plain.color, tagged.color),
	}, applied
}

// tagOver is the tag.class rule's value where one set it, else the class
// rule's: specificity before order.
func tagOver(plain, tagged cssProp) cssProp {
	if tagged.set {
		return tagged
	}
	return plain
}

// cssWork reports whether applying s over the page under n stays within
// limit rule sets. It visits every element the walk could (and those it
// skips), so it bounds the walk's own cascade work from above.
func (s cssSheet) cssWork(n *html.Node, limit int) bool {
	if len(s) == 0 {
		return true
	}
	work, budget := 0, maxXMLNodes
	var walk func(*html.Node, int) bool
	walk = func(m *html.Node, depth int) bool {
		if depth > maxXMLDepth {
			return true
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			if budget--; budget < 0 {
				return true
			}
			if c.Type != html.ElementNode {
				continue
			}
			_, a := s.match(c)
			if work += a; work > limit {
				return false
			}
			if !walk(c, depth+1) {
				return false
			}
		}
		return true
	}
	_, a := s.match(n)
	return a <= limit && walk(n, 0)
}
