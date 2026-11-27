package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Page text (RFC HB D3): a header or footer is one line or three slots, and
// a line may hold four placeholders. {title} and {date} become text; {page}
// and {pages} become the paged formats' own page counters, which each
// writer writes itself (Typst's counter, Word's PAGE and NUMPAGES fields,
// ODT's page-number and page-count), never from text. HTML and EPUB have no
// pages, so there a counter is dropped with the words that lead up to it.

// PageLine is a header or footer: one line (Left), or three slots.
type PageLine struct {
	Left, Center, Right string
}

// errPageLine is a header or footer that is neither a string nor the slots.
var errPageLine = errors.New("render: page line")

// pageLineSlots is a PageLine's object form, the keys a style may use.
type pageLineSlots struct {
	Left   string `json:"left,omitempty"`
	Center string `json:"center,omitempty"`
	Right  string `json:"right,omitempty"`
}

// UnmarshalJSON reads a line, or {left, center, right}; a line is the left
// slot, which is how a style before RFC HB reads.
func (l *PageLine) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*l = PageLine{}
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return errPageLine
		}
		*l = PageLine{Left: s}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var o pageLineSlots
	if err := dec.Decode(&o); err != nil {
		return errPageLine
	}
	*l = PageLine(o)
	return nil
}

// MarshalJSON writes a left-only line as a string, so a style without slots
// encodes as it did before RFC HB (the render log's hashes stay the same).
func (l PageLine) MarshalJSON() ([]byte, error) {
	if l.Center == "" && l.Right == "" {
		return json.Marshal(l.Left)
	}
	return json.Marshal(pageLineSlots(l))
}

// IsZero reports a line with no slot set (for omitzero).
func (l PageLine) IsZero() bool { return l == PageLine{} }

func (l PageLine) slots() [3]string { return [3]string{l.Left, l.Center, l.Right} }

// check holds each slot to a style line, and its placeholders to the four.
func (l PageLine) check() error {
	for _, s := range l.slots() {
		if !styleLineOK(s) {
			return fmt.Errorf("one line (or each of left, center and right) of at most %d characters, without control characters", maxStyleLine)
		}
		if _, err := parsePageText(s, "", ""); err != nil {
			return err
		}
	}
	return nil
}

// plain reports a line the writers print as before RFC HB: no slot but the
// left, and no brace in it (so no placeholder and no escaped brace).
func (l PageLine) plain() bool {
	return l.Center == "" && l.Right == "" && !strings.ContainsAny(l.Left, "{}")
}

// A pageField is what a part of a page line is: text, or a page counter.
type pageField int

const (
	fieldText pageField = iota
	fieldPage
	fieldPages
)

// pageSeg is one part of a slot: Text, or a counter the writer draws.
type pageSeg struct {
	Text  string
	Field pageField
}

// parsePageText splits a slot into text and counters, putting title and
// date where {title} and {date} stand. {{ and }} are braces; a lone } is
// a brace; any other { must open one of the four placeholders.
func parsePageText(s, title, date string) ([]pageSeg, error) {
	var segs []pageSeg
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			segs = append(segs, pageSeg{Text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '{' && strings.HasPrefix(s[i:], "{{"):
			text.WriteByte('{')
			i++
		case c == '}' && strings.HasPrefix(s[i:], "}}"):
			text.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("a placeholder is not closed (write {{ for a brace)")
			}
			switch name := s[i+1 : i+end]; name {
			case "title":
				text.WriteString(oneLine(title))
			case "date":
				text.WriteString(date)
			case "page", "pages":
				flush()
				f := fieldPage
				if name == "pages" {
					f = fieldPages
				}
				segs = append(segs, pageSeg{Field: f})
			default:
				return nil, fmt.Errorf("unknown placeholder {%s}: the placeholders are {page}, {pages}, {title} and {date} (write {{ for a brace)", name)
			}
			i += end
		default:
			text.WriteByte(c)
		}
	}
	flush()
	return segs, nil
}

// pageSlots is a header or footer resolved for a writer: left, center and
// right, each a run of text and counters.
type pageSlots [3][]pageSeg

func (p pageSlots) empty() bool { return len(p[0]) == 0 && len(p[1]) == 0 && len(p[2]) == 0 }

func (p pageSlots) has(f pageField) bool {
	for _, slot := range p {
		for _, seg := range slot {
			if seg.Field == f {
				return true
			}
		}
	}
	return false
}

// pageTextPlain reports a style whose header and footer print as before
// RFC HB; a writer then keeps its earlier output byte for byte (G3).
func (s *Style) pageTextPlain() bool {
	return s == nil || (s.Header.plain() && s.Footer.plain())
}

// UsesDate reports a style that prints {date} somewhere.
func (s *Style) UsesDate() bool {
	if s == nil {
		return false
	}
	for _, l := range []PageLine{s.Header, s.Footer} {
		for _, slot := range l.slots() {
			if strings.Contains(strings.ReplaceAll(slot, "{{", ""), "{date}") {
				return true
			}
		}
	}
	return false
}

// pageText resolves the style's header and footer for a paged writer. When
// no line shows {page} and page numbers are on, the page number goes at the
// end of the footer's right slot, where it stood before RFC HB.
func (d *Doc) pageText() (header, footer pageSlots) {
	st := d.Style
	if st == nil {
		return
	}
	resolve := func(l PageLine) pageSlots {
		var p pageSlots
		for i, slot := range l.slots() {
			// check() held every slot to the placeholders, so this
			// cannot fail on a Doc that passed it.
			p[i], _ = parsePageText(slot, d.Title, st.Date)
		}
		return p
	}
	header, footer = resolve(st.Header), resolve(st.Footer)
	if st.pageNumbers() && !header.has(fieldPage) && !footer.has(fieldPage) {
		if len(footer[2]) > 0 {
			footer[2] = append(footer[2], pageSeg{Text: " "})
		}
		footer[2] = append(footer[2], pageSeg{Field: fieldPage})
	}
	return header, footer
}

// webSeparators are the separators a dropped counter takes with it.
var webSeparators = []string{" · ", " | ", " — ", " – ", " - ", ", "}

// webPageText is a header or footer for HTML and EPUB, which have no pages:
// a slot with a counter keeps only the text before the separator that leads
// up to the first counter ("Ann Lee · Page {page} of {pages}" is "Ann
// Lee"), or nothing when there is no such separator.
func (d *Doc) webPageText(l PageLine) [3]string {
	var out [3]string
	for i, slot := range l.slots() {
		var title, date string
		if d.Style != nil {
			date = d.Style.Date
		}
		title = d.Title
		segs, _ := parsePageText(slot, title, date)
		var b strings.Builder
		counter := false
		for _, seg := range segs {
			if seg.Field != fieldText {
				counter = true
				break
			}
			b.WriteString(seg.Text)
		}
		text := b.String()
		if counter {
			cut := -1
			for _, sep := range webSeparators {
				if k := strings.LastIndex(text, sep); k > cut {
					cut = k
				}
			}
			if cut < 0 {
				text = ""
			} else {
				text = strings.TrimRight(text[:cut], " ")
			}
		}
		out[i] = text
	}
	return out
}
