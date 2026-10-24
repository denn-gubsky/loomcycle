package extract

import (
	"bytes"
	"strconv"
)

// A small reader for PDF content streams and CMaps. pdfcpu parses the file's
// object graph; the operators inside a page's content (and inside a font's
// ToUnicode CMap) are read here, from bytes already decoded under pdfcpu's
// limits, so every loop below is bounded by the length of its input.

type pdfKind uint8

const (
	tokNone pdfKind = iota
	tokNum
	tokStr  // literal or hex string: the raw bytes
	tokName // without the slash
	tokArr
	tokOp   // an operator (or a keyword such as true, null, begincmap)
	tokDict // a dictionary operand: its contents are not kept
)

type pdfVal struct {
	kind pdfKind
	num  float64
	str  string
	arr  []pdfVal
}

const (
	// pdfMaxNesting bounds nested arrays and dictionaries in one operand.
	pdfMaxNesting = 32
	// pdfMaxArray bounds the elements kept from one array (a TJ array of a
	// long line holds a few hundred).
	pdfMaxArray = 1 << 16
)

type pdfLexer struct {
	b []byte
	i int
}

func isPDFSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}

func isPDFDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func (l *pdfLexer) skipSpace() {
	for l.i < len(l.b) {
		c := l.b[l.i]
		if isPDFSpace(c) {
			l.i++
			continue
		}
		if c == '%' {
			for l.i < len(l.b) && l.b[l.i] != '\n' && l.b[l.i] != '\r' {
				l.i++
			}
			continue
		}
		return
	}
}

// next reads one token. ok is false at the end of the input. A closing
// bracket is returned as an operator "]" or ">>" for the caller to match.
func (l *pdfLexer) next(depth int) (pdfVal, bool) {
	l.skipSpace()
	if l.i >= len(l.b) {
		return pdfVal{}, false
	}
	c := l.b[l.i]
	switch {
	case c == '(':
		return pdfVal{kind: tokStr, str: l.literal()}, true
	case c == '<' && l.i+1 < len(l.b) && l.b[l.i+1] == '<':
		l.i += 2
		l.skipDict(depth + 1)
		return pdfVal{kind: tokDict}, true
	case c == '<':
		return pdfVal{kind: tokStr, str: l.hex()}, true
	case c == '>' && l.i+1 < len(l.b) && l.b[l.i+1] == '>':
		l.i += 2
		return pdfVal{kind: tokOp, str: ">>"}, true
	case c == '[':
		l.i++
		return pdfVal{kind: tokArr, arr: l.array(depth + 1)}, true
	case c == ']':
		l.i++
		return pdfVal{kind: tokOp, str: "]"}, true
	case c == '/':
		l.i++
		return pdfVal{kind: tokName, str: l.name()}, true
	case c == '+' || c == '-' || c == '.' || (c >= '0' && c <= '9'):
		start := l.i
		l.i++
		for l.i < len(l.b) && !isPDFSpace(l.b[l.i]) && !isPDFDelim(l.b[l.i]) {
			l.i++
		}
		if f, err := strconv.ParseFloat(string(l.b[start:l.i]), 64); err == nil {
			return pdfVal{kind: tokNum, num: f}, true
		}
		return pdfVal{kind: tokOp, str: string(l.b[start:l.i])}, true
	case isPDFDelim(c):
		// A stray ')', '>', '{' or '}': skip it.
		l.i++
		return pdfVal{kind: tokOp, str: string(c)}, true
	}
	start := l.i
	for l.i < len(l.b) && !isPDFSpace(l.b[l.i]) && !isPDFDelim(l.b[l.i]) {
		l.i++
	}
	return pdfVal{kind: tokOp, str: string(l.b[start:l.i])}, true
}

func (l *pdfLexer) array(depth int) []pdfVal {
	var out []pdfVal
	for {
		if depth > pdfMaxNesting {
			l.skipTo(']')
			return out
		}
		v, ok := l.next(depth)
		if !ok {
			return out
		}
		if v.kind == tokOp && v.str == "]" {
			return out
		}
		if len(out) < pdfMaxArray {
			out = append(out, v)
		}
	}
}

// skipDict skips a dictionary operand (the property list of a marked
// content operator, an inline image's parameters).
func (l *pdfLexer) skipDict(depth int) {
	for {
		if depth > pdfMaxNesting {
			l.skipTo('>')
			return
		}
		v, ok := l.next(depth)
		if !ok || (v.kind == tokOp && v.str == ">>") {
			return
		}
	}
}

// skipTo moves past the next c, for input nested too deep to parse.
func (l *pdfLexer) skipTo(c byte) {
	if j := bytes.IndexByte(l.b[l.i:], c); j >= 0 {
		l.i += j + 1
		return
	}
	l.i = len(l.b)
}

func (l *pdfLexer) name() string {
	start := l.i
	for l.i < len(l.b) && !isPDFSpace(l.b[l.i]) && !isPDFDelim(l.b[l.i]) {
		l.i++
	}
	raw := l.b[start:l.i]
	if bytes.IndexByte(raw, '#') < 0 {
		return string(raw)
	}
	var b []byte
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i+2 < len(raw) {
			if h, ok := hexByte(raw[i+1], raw[i+2]); ok {
				b = append(b, h)
				i += 2
				continue
			}
		}
		b = append(b, raw[i])
	}
	return string(b)
}

func (l *pdfLexer) literal() string {
	l.i++ // (
	var b []byte
	depth := 1
	for l.i < len(l.b) {
		c := l.b[l.i]
		l.i++
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(b)
			}
		case '\\':
			if l.i >= len(l.b) {
				return string(b)
			}
			e := l.b[l.i]
			l.i++
			switch e {
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case '\r':
				if l.i < len(l.b) && l.b[l.i] == '\n' {
					l.i++
				}
				continue
			case '\n':
				continue
			default:
				if e >= '0' && e <= '7' {
					n := int(e - '0')
					for k := 0; k < 2 && l.i < len(l.b) && l.b[l.i] >= '0' && l.b[l.i] <= '7'; k++ {
						n = n*8 + int(l.b[l.i]-'0')
						l.i++
					}
					c = byte(n)
				} else {
					c = e
				}
			}
		}
		b = append(b, c)
	}
	return string(b)
}

func (l *pdfLexer) hex() string {
	l.i++ // <
	var b []byte
	var hi byte
	half := false
	for l.i < len(l.b) {
		c := l.b[l.i]
		l.i++
		if c == '>' {
			break
		}
		v, ok := hexNibble(c)
		if !ok {
			continue
		}
		if half {
			b = append(b, hi<<4|v)
		} else {
			hi = v
		}
		half = !half
	}
	if half {
		b = append(b, hi<<4)
	}
	return string(b)
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func hexByte(a, b byte) (byte, bool) {
	x, ok1 := hexNibble(a)
	y, ok2 := hexNibble(b)
	return x<<4 | y, ok1 && ok2
}

// skipInlineImage moves past an inline image's data, just after its ID
// operator: to the EI that follows whitespace and precedes whitespace or the
// end. The data is binary and may hold anything, so this is a heuristic, as
// it is in every reader.
func (l *pdfLexer) skipInlineImage() {
	if l.i < len(l.b) && isPDFSpace(l.b[l.i]) {
		l.i++
	}
	for l.i+1 < len(l.b) {
		j := bytes.Index(l.b[l.i:], []byte("EI"))
		if j < 0 {
			break
		}
		at := l.i + j
		before := at == 0 || isPDFSpace(l.b[at-1])
		after := at+2 >= len(l.b) || isPDFSpace(l.b[at+2])
		l.i = at + 2
		if before && after {
			return
		}
	}
	l.i = len(l.b)
}

// pdfOperators reads content as a sequence of operators, calling do with
// each operator and its operands. maxOperands bounds the stack: a run of
// operands with no operator (garbage, or a stream that is not content) is
// dropped from the bottom rather than kept.
func pdfOperators(content []byte, do func(op string, args []pdfVal) bool) {
	const maxOperands = 64
	l := &pdfLexer{b: content}
	var args []pdfVal
	for {
		v, ok := l.next(0)
		if !ok {
			return
		}
		if v.kind != tokOp {
			if len(args) == maxOperands {
				copy(args, args[1:])
				args = args[:maxOperands-1]
			}
			args = append(args, v)
			continue
		}
		switch v.str {
		case "true", "false", "null":
			args = append(args, pdfVal{kind: tokNone})
			continue
		case "BI":
			// Inline image parameters run to ID; the data after ID is skipped.
			for {
				t, ok := l.next(0)
				if !ok {
					return
				}
				if t.kind == tokOp && t.str == "ID" {
					break
				}
			}
			l.skipInlineImage()
			if !do("BI", nil) {
				return
			}
			args = args[:0]
			continue
		}
		if !do(v.str, args) {
			return
		}
		args = args[:0]
	}
}
