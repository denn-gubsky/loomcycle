package extract

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Heading recovery for HTML (Options.InferHeadings). Formatting is read from
// <b> and <strong> and from inline style attributes (font-weight,
// font-size, color), inherited down the tree as a browser does. Style
// sheets and classes are not read.

// htmlBodyPt is a browser's default font size: 16px.
const htmlBodyPt = 12

// walkStyled walks an element's children under the formatting it sets. A
// list item's text is never a heading.
func (h *htmlState) walkStyled(n *html.Node, depth int, li bool) error {
	if !h.infer {
		return h.walk(n, depth+1)
	}
	saved := h.cur
	h.cur = htmlStyled(n, saved)
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

// htmlStyled is the formatting text inside n has, given its parent's.
func htmlStyled(n *html.Node, parent runFmt) runFmt {
	f := parent
	switch n.Data {
	case "b", "strong":
		f.bold = true
	case "font":
		if c := attrOf(n, "color"); c != "" {
			f.color = cssColor(c, parent.color)
		}
	}
	style := attrOf(n, "style")
	if style == "" {
		return f
	}
	// A style attribute is someone else's text: only the first few
	// declarations are read.
	for i, decl := range strings.SplitN(style, ";", 64) {
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
			f.bold = cssBold(val, f.bold)
		case "font-size":
			if s, ok := cssSize(val, parent.size); ok {
				f.size = s
			}
		case "color":
			f.color = cssColor(val, parent.color)
		}
	}
	return f
}

func cssBold(v string, parent bool) bool {
	switch v {
	case "bold", "bolder":
		return true
	case "normal", "lighter":
		return false
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n >= 600
	}
	return parent
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
