package extract

import (
	"errors"
	"strings"
)

// DjVuLibre's programs answer in S-expressions (miniexp): djvutxt
// --detail=page writes one per page, `(page x0 y0 x1 y1 "text")` or `()` for
// a page with no text layer; djvused print-outline writes `(bookmarks
// ("title" "#page" children...) ...)`; print-meta writes `key "value"` pairs.
// The answer is the helper's reading of an untrusted file, so it is parsed
// under bounds like the file itself: the caller caps its bytes, this caps
// its nesting and its nodes.

const (
	maxSexprDepth = 64
	maxSexprNodes = 200_000
)

var errSexpr = errors.New("extract: the DjVu helper's answer could not be read")

// sexpr is one parsed expression: a list, a string, or an atom (a symbol
// or a number, kept as written).
type sexpr struct {
	list   []sexpr
	isList bool
	str    string
	isStr  bool
	atom   string
}

type sexprParser struct {
	b     []byte
	i     int
	nodes int
}

// parseSexprs reads every top-level expression of b.
func parseSexprs(b []byte) ([]sexpr, error) {
	p := &sexprParser{b: b}
	var out []sexpr
	for {
		p.space()
		if p.i >= len(p.b) {
			return out, nil
		}
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
}

func (p *sexprParser) space() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			p.i++
		case ';':
			// A comment, to the end of the line.
			for p.i < len(p.b) && p.b[p.i] != '\n' {
				p.i++
			}
		default:
			return
		}
	}
}

func (p *sexprParser) expr(depth int) (sexpr, error) {
	if depth > maxSexprDepth {
		return sexpr{}, errSexpr
	}
	p.nodes++
	if p.nodes > maxSexprNodes {
		return sexpr{}, errSexpr
	}
	switch p.b[p.i] {
	case '(':
		p.i++
		e := sexpr{isList: true}
		for {
			p.space()
			if p.i >= len(p.b) {
				return sexpr{}, errSexpr
			}
			if p.b[p.i] == ')' {
				p.i++
				return e, nil
			}
			c, err := p.expr(depth + 1)
			if err != nil {
				return sexpr{}, err
			}
			e.list = append(e.list, c)
		}
	case ')':
		return sexpr{}, errSexpr
	case '"':
		s, err := p.str()
		return sexpr{str: s, isStr: true}, err
	}
	start := p.i
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r', '\f', '\v', '(', ')', '"', ';':
			return sexpr{atom: string(p.b[start:p.i])}, nil
		}
		p.i++
	}
	return sexpr{atom: string(p.b[start:p.i])}, nil
}

// str reads a quoted string with C escapes, as miniexp writes them: \n, \t
// and the like, \ooo octal and \xHH hex bytes, \\ and \". The bytes are not
// checked as UTF-8 here; the caller cleans the text.
func (p *sexprParser) str() (string, error) {
	p.i++ // the opening quote
	var b strings.Builder
	for p.i < len(p.b) {
		c := p.b[p.i]
		p.i++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.i >= len(p.b) {
				return "", errSexpr
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case '0', '1', '2', '3', '4', '5', '6', '7':
				v := int(e - '0')
				for k := 0; k < 2 && p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '7'; k++ {
					v = v*8 + int(p.b[p.i]-'0')
					p.i++
				}
				b.WriteByte(byte(v))
			case 'x':
				v, k := 0, 0
				for ; k < 2 && p.i < len(p.b) && hexDigit(p.b[p.i]) >= 0; k++ {
					v = v*16 + hexDigit(p.b[p.i])
					p.i++
				}
				if k == 0 {
					b.WriteByte('x')
				} else {
					b.WriteByte(byte(v))
				}
			case '\n':
				// A line continuation.
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errSexpr
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// djvuPageText is the text of one djvutxt page expression, `(page x0 y0 x1
// y1 ...)` or `()`. Its content is a string, or zones (column, region, para,
// line, word, char) holding strings; a word ends with a space, a line with
// a line break and a larger zone with a blank line, so a finer --detail
// reads the same as page detail. ok is false for an expression that is not
// a page at all.
func djvuPageText(e sexpr) (text string, ok bool) {
	if !e.isList {
		return "", false
	}
	if len(e.list) == 0 {
		return "", true
	}
	if e.list[0].atom != "page" {
		return "", false
	}
	var b strings.Builder
	djvuZoneText(e, &b)
	return b.String(), true
}

func djvuZoneText(e sexpr, b *strings.Builder) {
	for _, c := range e.list {
		switch {
		case c.isStr:
			b.WriteString(c.str)
		case c.isList:
			djvuZoneText(c, b)
			if len(c.list) > 0 {
				switch c.list[0].atom {
				case "word":
					b.WriteByte(' ')
				case "char":
				case "line":
					b.WriteByte('\n')
				default:
					b.WriteString("\n\n")
				}
			}
		}
	}
}
