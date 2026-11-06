package extract

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// Heading recovery (Options.InferHeadings, RFC GB §6). An editor that marks
// headings with formatting rather than heading styles (a bold line, a larger
// font, a colour) exports a document with a flat outline: one section, so
// nothing downstream (classification, anchors, links) has a part to hold
// on to. The rules below are deterministic: the same document always gets
// the same outline, and a document whose headings are real heading styles
// keeps its own.

const (
	// inferMaxRunes is the longest line that may be a heading.
	inferMaxRunes = 120
	// inferSizeRatio is how much larger than the body a line must be to be
	// a heading by its size alone.
	inferSizeRatio = 1.15
	// decorativeRepeats is how often one picture must appear in a document
	// to be taken as decoration (a divider, a logo on every section).
	decorativeRepeats = 3
)

// runFmt is the formatting of a piece of text: bold, its size in points
// (0 when unknown) and its colour ("" for the default, black).
type runFmt struct {
	bold  bool
	size  float64
	color string
}

// fmtRun is text with one formatting.
type fmtRun struct {
	text string
	fmt  runFmt
}

// inferPara is one paragraph of a document as the inference sees it.
type inferPara struct {
	runs []fmtRun
	// eligible is false for a paragraph in a list or a table cell: it is
	// body text whatever it looks like.
	eligible bool
	// real is the level of a real heading (an <hN>, a Heading N style),
	// 0 for any other paragraph. A real heading is not body text.
	real int
}

// paraLook is what a paragraph's text looks like as a whole.
type paraLook struct {
	text  string // the cleaned text
	chars int    // its non-space characters
	bold  bool   // every non-space character is bold
	size  float64
	color string
	mixed bool // its characters are not all one colour
}

// look summarises a paragraph: bold when all of its text is, its size the
// smallest of its text's (a line is only as large as its smallest part), its
// colour when all of its text has one.
func (p inferPara) look() paraLook {
	var b strings.Builder
	l := paraLook{bold: true}
	first := true
	for _, r := range p.runs {
		b.WriteString(r.text)
		n := nonSpaceRunes(r.text)
		if n == 0 {
			continue
		}
		l.chars += n
		l.bold = l.bold && r.fmt.bold
		if first || (r.fmt.size > 0 && (l.size == 0 || r.fmt.size < l.size)) {
			l.size = r.fmt.size
		}
		if first {
			l.color = r.fmt.color
		} else if r.fmt.color != l.color {
			l.mixed = true
		}
		first = false
	}
	if l.chars == 0 {
		l.bold = false
	}
	l.text = cleanSpace(b.String())
	return l
}

// inferLevels returns the heading level each paragraph gets (0: none),
// counting from 1 in the document's own heading space (an inferred heading
// at level 2 sits where a Heading 2 would), and how many were inferred.
func inferLevels(paras []inferPara) ([]int, int) {
	looks := make([]paraLook, len(paras))
	// The body: every paragraph that is not a real heading, by the
	// characters each holds, so a long paragraph outweighs a short label.
	var sizes []sizeWeight
	colors := map[string]int{}
	shallowest := 0
	for i, p := range paras {
		if p.real > 0 {
			if shallowest == 0 || p.real < shallowest {
				shallowest = p.real
			}
			continue
		}
		l := p.look()
		looks[i] = l
		if l.chars == 0 {
			continue
		}
		if l.size > 0 {
			sizes = append(sizes, sizeWeight{l.size, l.chars})
		}
		if !l.mixed {
			colors[l.color] += l.chars
		}
	}
	median := weightedMedian(sizes)
	dominant := dominantColor(colors)

	levels := make([]int, len(paras))
	larger := map[float64]bool{}
	type pick struct {
		i    int
		size float64 // rounded; 0 for a line at body size
	}
	var picks []pick
	for i, p := range paras {
		if p.real > 0 || !p.eligible {
			continue
		}
		l := looks[i]
		if !headingShaped(l.text) || l.chars == 0 {
			continue
		}
		big := median > 0 && l.size > median
		byColor := !l.mixed && l.color != dominant && (l.bold || big)
		if !l.bold && !(median > 0 && l.size >= inferSizeRatio*median) && !byColor {
			continue
		}
		s := 0.0
		if big {
			// Sizes a half point apart are one size: 14pt is 18.67px.
			s = math.Round(l.size*2) / 2
			larger[s] = true
		}
		picks = append(picks, pick{i, s})
	}
	order := make([]float64, 0, len(larger))
	for s := range larger {
		order = append(order, s)
	}
	slices.Sort(order)
	slices.Reverse(order)
	for _, p := range picks {
		lv := shallowest + 1 + len(order) // bold at body size: the deepest
		if p.size > 0 {
			lv = shallowest + 1 + slices.Index(order, p.size)
		}
		levels[p.i] = min(lv, 9)
	}
	return levels, len(picks)
}

// headingShaped reports whether text could be a heading: one line, at most
// inferMaxRunes characters, not ending as a sentence or a clause does.
func headingShaped(text string) bool {
	if text == "" || strings.ContainsRune(text, '\n') || utf8.RuneCountInString(text) > inferMaxRunes {
		return false
	}
	switch text[len(text)-1] {
	case '.', ',', ';':
		return false
	}
	return true
}

type sizeWeight struct {
	size  float64
	chars int
}

// weightedMedian is the size at which half the body's characters are
// smaller and half larger; 0 when no size is known.
func weightedMedian(ws []sizeWeight) float64 {
	if len(ws) == 0 {
		return 0
	}
	slices.SortFunc(ws, func(a, b sizeWeight) int {
		switch {
		case a.size < b.size:
			return -1
		case a.size > b.size:
			return 1
		}
		return 0
	})
	total := 0
	for _, w := range ws {
		total += w.chars
	}
	acc := 0
	for _, w := range ws {
		acc += w.chars
		if 2*acc >= total {
			return w.size
		}
	}
	return ws[len(ws)-1].size
}

// dominantColor is the colour most of the body's characters have; a tie
// goes to the default colour, then to the colour that sorts first, so the
// answer never depends on map order.
func dominantColor(colors map[string]int) string {
	best, n := "", colors[""]
	keys := make([]string, 0, len(colors))
	for c := range colors {
		keys = append(keys, c)
	}
	slices.Sort(keys)
	for _, c := range keys {
		if colors[c] > n {
			best, n = c, colors[c]
		}
	}
	return best
}

func nonSpaceRunes(s string) int {
	n := 0
	for _, r := range s {
		if !isSpace(r) {
			n++
		}
	}
	return n
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == ' ' || r == ' ' || r == ' '
}

// cleanSpace is a paragraph's text as the option writes it: a tab or a
// non-breaking space is a space, a run of spaces is one, and each line is
// trimmed. A paragraph of nothing but those is empty, and dropped.
func cleanSpace(s string) string {
	lines := strings.Split(normalizeNewlines(s), "\n")
	out := lines[:0]
	for _, l := range lines {
		var b strings.Builder
		space := false
		for _, r := range l {
			if isSpace(r) {
				space = true
				continue
			}
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
		out = append(out, b.String())
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// normalizeColor is a colour as the inference compares it: lower-case
// six-digit hex where it can be read as one, and black as the default
// ("" — an editor writes black and "automatic" alike for body text).
func normalizeColor(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch c {
	case "", "auto", "black", "#000", "#000000", "000000", "windowtext":
		return ""
	}
	if !strings.HasPrefix(c, "#") && len(c) == 6 && isHex(c) {
		c = "#" + c
	}
	if len(c) == 4 && c[0] == '#' && isHex(c[1:]) {
		c = "#" + string([]byte{c[1], c[1], c[2], c[2], c[3], c[3]})
	}
	if c == "#000000" {
		return ""
	}
	return c
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// decorative counts the occurrences of each picture, by a key that names it,
// and answers whether one is decoration.
type decorative map[string]int

func (d decorative) is(key string) bool {
	return key != "" && d[key] >= decorativeRepeats
}
