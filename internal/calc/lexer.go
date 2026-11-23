package calc

import (
	"math"
	"math/big"
	"strings"
)

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tOp  // operators and punctuation; the text is in tok.s
	tSep // a statement separator: ';' or a newline outside brackets
)

type token struct {
	kind tokKind
	s    string
	num  num // tNum only
	// imag marks an imaginary literal, 2i: the number times i. Only a
	// numeric suffix makes one; the name i stays an ordinary variable.
	imag bool
	pos  int
}

// ops is ordered longest first so the scanner takes the longest match.
var ops = [...]string{
	"->", "==", "!=", "<=", ">=", ".*", "./", ".^",
	"+", "-", "*", "/", "^", "%", "!", "<", ">", "=",
	"(", ")", "[", "]", ",", ":",
	// Member access, t.price: a column of a table or a cell of a row. After
	// the dotted operators above, so `.*` is still one token.
	".",
	// The postfix transpose, A'. Strings use double quotes, so a single
	// quote has no other meaning; a model's 'text' is refused by the parser
	// with a hint.
	"'",
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// lex splits src into tokens. Newlines inside ( ) or [ ] are whitespace so a
// long list or call may span lines; outside them a newline ends a statement.
func lex(src string) ([]token, *Error) {
	var toks []token
	depth := 0
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '\n' || c == ';':
			if c == ';' || depth == 0 {
				toks = append(toks, token{kind: tSep, s: string(c), pos: i})
			}
			i++
		case isDigit(c) || c == '.' && i+1 < len(src) && isDigit(src[i+1]):
			t, err := lexNumber(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i += len(t.s)
		case isLetter(c):
			j := i
			for j < len(src) && (isLetter(src[j]) || isDigit(src[j])) {
				j++
			}
			toks = append(toks, token{kind: tIdent, s: src[i:j], pos: i})
			i = j
		case c == '"':
			t, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i += n
		default:
			if strings.HasPrefix(src[i:], "**") {
				return nil, errf(codeSyntax, i, "`**` is not an operator").withHint("power is `^`, e.g. `2^10`")
			}
			op := ""
			for _, o := range ops {
				if strings.HasPrefix(src[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, errf(codeSyntax, i, "unexpected character %q", rune(c))
			}
			switch op {
			case "(", "[":
				depth++
			case ")", "]":
				if depth > 0 {
					depth--
				}
			}
			toks = append(toks, token{kind: tOp, s: op, pos: i})
			i += len(op)
		}
	}
	toks = append(toks, token{kind: tEOF, pos: len(src)})
	return toks, nil
}

func lexString(src string, start int) (token, int, *Error) {
	var b strings.Builder
	i := start + 1
	for i < len(src) {
		c := src[i]
		switch c {
		case '"':
			return token{kind: tStr, s: b.String(), pos: start}, i + 1 - start, nil
		case '\\':
			if i+1 >= len(src) {
				return token{}, 0, errf(codeSyntax, i, "unfinished escape in string")
			}
			switch src[i+1] {
			case '"', '\\':
				b.WriteByte(src[i+1])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return token{}, 0, errf(codeSyntax, i, "unknown escape \\%c in string", src[i+1]).withHint("escapes are \\\" \\\\ \\n \\t")
			}
			i += 2
		case '\n':
			return token{}, 0, errf(codeSyntax, start, "string is not closed before the end of the line")
		default:
			b.WriteByte(c)
			i++
		}
	}
	return token{}, 0, errf(codeSyntax, start, "string is not closed").withHint("strings use double quotes: \"text\"")
}

// lexNumber scans one numeric literal at src[start:]. A letter directly
// after a number is refused as implicit multiplication rather than read as
// two tokens: `2x` is the most common way a model writes a product, and
// silently accepting it would need a precedence rule nobody agrees on.
func lexNumber(src string, start int) (token, *Error) {
	i := start
	radix := false
	if src[i] == '0' && i+1 < len(src) && strings.ContainsRune("xXbBoO", rune(src[i+1])) {
		radix = true
		i += 2
		for i < len(src) && (isDigit(src[i]) || isLetter(src[i])) {
			i++
		}
	} else {
		for i < len(src) && (isDigit(src[i]) || src[i] == '_') {
			i++
		}
		// A '.' followed by '*', '/' or '^' is an element-wise operator, not
		// a decimal point: `2.*v`.
		if i < len(src) && src[i] == '.' && (i+1 >= len(src) || !strings.ContainsRune("*/^", rune(src[i+1]))) {
			i++
			for i < len(src) && (isDigit(src[i]) || src[i] == '_') {
				i++
			}
		}
		if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
			j := i + 1
			if j < len(src) && (src[j] == '+' || src[j] == '-') {
				j++
			}
			if j < len(src) && isDigit(src[j]) {
				i = j
				for i < len(src) && (isDigit(src[i]) || src[i] == '_') {
					i++
				}
			}
		}
	}
	text := src[start:i]
	if !radix && isImagSuffix(src, i) {
		n, err := parseNumberLiteral(text, false)
		if err != nil {
			err.Pos = start
			return token{}, err
		}
		if math.IsInf(n.float(), 0) {
			return token{}, errf(codeOverflow, start, "the imaginary part of %.20s overflows a 64-bit float", src[start:i+1])
		}
		return token{kind: tNum, s: src[start : i+1], num: n, imag: true, pos: start}, nil
	}
	if i < len(src) && (isLetter(src[i]) || src[i] == '"') {
		j := i
		for j < len(src) && (isLetter(src[j]) || isDigit(src[j])) {
			j++
		}
		return token{}, errf(codeSyntax, start, "`%s` is not a number: no implicit multiplication", src[start:j]).
			withHint("write `" + text + "*" + src[i:j] + "`")
	}
	n, err := parseNumberLiteral(text, radix)
	if err != nil {
		err.Pos = start
		return token{}, err
	}
	return token{kind: tNum, s: text, num: n, pos: start}, nil
}

// isImagSuffix reports a lone `i` at src[i] ending a decimal literal: 2i,
// 1.5i, 3e2i. A longer word stays the implicit-multiplication refusal, so
// 2in, 2if and 2pi read as before.
func isImagSuffix(src string, i int) bool {
	if i >= len(src) || src[i] != 'i' {
		return false
	}
	j := i + 1
	return j >= len(src) || !(isLetter(src[j]) || isDigit(src[j]) || src[j] == '"')
}

// parseNumberLiteral turns a literal into an exact number. The size is
// checked on the text before math/big sees it: SetString("1e999999999")
// would otherwise allocate a billion-digit integer.
func parseNumberLiteral(text string, radix bool) (num, *Error) {
	if radix {
		if len(text)-2 > MaxExactBits/4+1 {
			return num{}, errf(codeTooLarge, -1, "literal %.20s... is over %d bits", text, MaxExactBits)
		}
		v, ok := new(big.Int).SetString(text, 0)
		if !ok {
			return num{}, errf(codeSyntax, -1, "malformed number %q", text).withHint("hex is 0xff, binary 0b1010, octal 0o17")
		}
		return exactInt(v), nil
	}
	if strings.HasPrefix(text, "_") || strings.HasSuffix(text, "_") || strings.Contains(text, "__") ||
		strings.Contains(text, "_.") || strings.Contains(text, "._") {
		return num{}, errf(codeSyntax, -1, "malformed number %q", text).withHint("`_` may only separate digits, as in 1_000_000")
	}
	clean := strings.ReplaceAll(text, "_", "")
	mant, exp := clean, ""
	if k := strings.IndexAny(clean, "eE"); k >= 0 {
		mant, exp = clean[:k], clean[k+1:]
	}
	if exp != "" {
		e := strings.TrimLeft(exp, "+-")
		e = strings.TrimLeft(e, "0")
		// 10^e needs about 3.33*e bits; more than six exponent digits is
		// over the cap whatever the mantissa.
		if len(e) > 6 {
			return num{}, errf(codeTooLarge, -1, "exponent of %.20s is too large for an exact number", text)
		}
		ev := 0
		for _, c := range e {
			ev = ev*10 + int(c-'0')
		}
		if float64(ev+len(mant))*3.33 > MaxExactBits {
			return num{}, errf(codeTooLarge, -1, "literal %.20s is over %d bits", text, MaxExactBits)
		}
	} else if float64(len(mant))*3.33 > MaxExactBits {
		return num{}, errf(codeTooLarge, -1, "literal %.20s... is over %d bits", text, MaxExactBits)
	}
	r, ok := new(big.Rat).SetString(clean)
	if !ok {
		return num{}, errf(codeSyntax, -1, "malformed number %q", text)
	}
	return exactNum(r), nil
}
