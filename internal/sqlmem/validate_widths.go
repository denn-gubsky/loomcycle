package sqlmem

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// validateWidths refuses an agent statement that asks for a value longer than
// limit where the width is a constant it can fold (literals, parentheses,
// casts, + - * /): zeroblob(N) / randomblob(N) and a printf/format width or
// precision on sqlite, repeat/lpad/rpad on postgres.
//
// What holds the bound differs by tier, and this check is the hard part of it
// on neither:
//   - sqlite: the engine's own limit (SQLITE_LIMIT_LENGTH, set on every agent
//     statement, sqlite.go opConn) refuses any value over limit however it is
//     computed. This check only refuses the plain case early, with a message
//     the model can act on; a width it cannot fold is left to the engine.
//   - postgres: there is no per-value limit in the engine. A row over limit is
//     refused by the server before it is sent (wrapValueLimit), but the server
//     still builds the value. So a repeat/lpad/rpad width that is not a
//     constant (a column, a parameter, a subquery) is refused here outright,
//     and a constant over limit too.
func validateWidths(statement string, d dialect, limit int) error {
	if limit <= 0 {
		return nil
	}
	toks := widthTokens(stripComments(statement, d == dialectPostgres), d == dialectPostgres)
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind != tokIdent || toks[i+1].text != "(" {
			continue
		}
		name := strings.ToLower(toks[i].text)
		args := callArgs(toks[i+2:])
		switch {
		case d == dialectSQLite && (name == "zeroblob" || name == "randomblob"):
			if len(args) > 0 {
				if v, ok := foldConst(args[0]); ok && v > float64(limit) {
					return refuse("%s(%s) asks for a value over the %d-byte limit on one SQL Memory value", name, tokensText(args[0]), limit)
				}
			}
		case d == dialectSQLite && (name == "printf" || name == "format"):
			if len(args) == 0 || len(args[0]) != 1 || args[0][0].kind != tokString {
				continue
			}
			spec := args[0][0].text
			for _, m := range printfWidthRe.FindAllStringSubmatch(spec, -1) {
				for _, n := range m[1:] {
					if v, err := strconv.ParseInt(n, 10, 64); n != "" && (err != nil || v > int64(limit)) {
						return refuse("%s's format asks for a width over the %d-byte limit on one SQL Memory value", name, limit)
					}
				}
			}
			if strings.Contains(spec, "*") {
				for _, a := range args[1:] {
					if v, ok := foldConst(a); ok && v > float64(limit) {
						return refuse("%s asks for a width of %s, over the %d-byte limit on one SQL Memory value", name, tokensText(a), limit)
					}
				}
			}
		case d == dialectPostgres && (name == "repeat" || name == "lpad" || name == "rpad"):
			if len(args) < 2 {
				continue
			}
			v, ok := foldConst(args[1])
			if !ok {
				return refuse("%s's length must be a constant here (got %s): the value it builds is not bounded otherwise", name, tokensText(args[1]))
			}
			if v > float64(limit) {
				return refuse("%s(…, %s) asks for a value over the %d-byte limit on one SQL Memory value", name, tokensText(args[1]), limit)
			}
		}
	}
	return nil
}

// foldConst evaluates an argument that is a constant expression: numbers,
// parentheses, unary and binary + - * /, a "::type" cast and CAST(x AS type).
// ok is false for anything else (a column, a parameter, a call, a subquery).
func foldConst(toks []widthToken) (float64, bool) {
	p := constParser{toks: toks}
	v, ok := p.expr()
	return v, ok && p.i == len(toks)
}

type constParser struct {
	toks []widthToken
	i    int
}

func (p *constParser) punct(s string) bool {
	if p.i < len(p.toks) && p.toks[p.i].kind == tokPunct && p.toks[p.i].text == s {
		p.i++
		return true
	}
	return false
}

func (p *constParser) expr() (float64, bool) {
	v, ok := p.term()
	for ok {
		switch {
		case p.punct("+"):
			var w float64
			w, ok = p.term()
			v += w
		case p.punct("-"):
			var w float64
			w, ok = p.term()
			v -= w
		default:
			return v, true
		}
	}
	return 0, false
}

func (p *constParser) term() (float64, bool) {
	v, ok := p.factor()
	for ok {
		switch {
		case p.punct("*"):
			var w float64
			w, ok = p.factor()
			v *= w
		case p.punct("/"):
			var w float64
			if w, ok = p.factor(); ok && w != 0 {
				v /= w
			}
		default:
			return v, true
		}
	}
	return 0, false
}

func (p *constParser) factor() (float64, bool) {
	var v float64
	var ok bool
	switch {
	case p.punct("+"):
		return p.factor()
	case p.punct("-"):
		v, ok = p.factor()
		return -v, ok
	case p.punct("("):
		if v, ok = p.expr(); !ok || !p.punct(")") {
			return 0, false
		}
	case p.i < len(p.toks) && p.toks[p.i].kind == tokNumber:
		v, ok = numberValue(p.toks[p.i].text)
		p.i++
		if !ok {
			return 0, false
		}
	case p.i+1 < len(p.toks) && p.toks[p.i].kind == tokIdent && strings.EqualFold(p.toks[p.i].text, "cast") && p.toks[p.i+1].text == "(":
		p.i += 2
		if v, ok = p.expr(); !ok || p.i >= len(p.toks) || !strings.EqualFold(p.toks[p.i].text, "as") {
			return 0, false
		}
		p.i++
		if !p.typeName() || !p.punct(")") {
			return 0, false
		}
	default:
		return 0, false
	}
	// Postfix casts: ::int, ::numeric(12,0).
	for p.i+1 < len(p.toks) && p.toks[p.i].text == ":" && p.toks[p.i+1].text == ":" {
		p.i += 2
		if !p.typeName() {
			return 0, false
		}
	}
	return v, true
}

// typeName skips a type name: words (double precision) and an optional
// parenthesised list of numbers.
func (p *constParser) typeName() bool {
	n := 0
	for p.i < len(p.toks) && p.toks[p.i].kind == tokIdent && !strings.EqualFold(p.toks[p.i].text, "as") {
		p.i++
		n++
	}
	if n == 0 {
		return false
	}
	if p.punct("(") {
		for p.i < len(p.toks) && (p.toks[p.i].kind == tokNumber || p.toks[p.i].text == ",") {
			p.i++
		}
		return p.punct(")")
	}
	return true
}

// numberValue reads a numeric literal: an integer too large for int64 counts
// as +Inf, so it is over any limit.
func numberValue(text string) (float64, bool) {
	if v, err := strconv.ParseInt(text, 0, 64); err == nil {
		return float64(v), true
	} else if errors.Is(err, strconv.ErrRange) {
		return math.Inf(1), true
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return f, true
	}
	return 0, false
}

func tokensText(toks []widthToken) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
		if t.kind == tokString {
			parts[i] = "'" + t.text + "'"
		}
	}
	return strings.Join(parts, " ")
}

// printfWidthRe finds a conversion's width and precision in a printf format.
var printfWidthRe = regexp.MustCompile(`%[-+ 0#,!]*(\d*)(?:\.(\d*))?`)

type widthTokenKind int

const (
	tokPunct widthTokenKind = iota
	tokIdent
	tokNumber
	tokString
)

type widthToken struct {
	kind widthTokenKind
	text string // a string token's text is its unquoted content
}

// widthTokens splits a comment-stripped statement into identifiers, numbers,
// string literals and single punctuation characters: enough to find a call and
// its literal arguments.
func widthTokens(s string, dollarQuotes bool) []widthToken {
	var out []widthToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for j < len(s) {
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			out = append(out, widthToken{kind: tokString, text: b.String()})
			i = j + 1
		case dollarQuotes && c == '$':
			if tag, ok := dollarTagAt(s, i); ok {
				body := s[i+len(tag):]
				end := strings.Index(body, tag)
				if end < 0 {
					end = len(body)
				}
				out = append(out, widthToken{kind: tokString, text: body[:end]})
				i += len(tag) + end + len(tag)
				continue
			}
			out = append(out, widthToken{kind: tokPunct, text: "$"})
			i++
		case c == '"' || c == '[' || c == '`':
			closer := c
			if c == '[' {
				closer = ']'
			}
			j := strings.IndexByte(s[i+1:], closer)
			if j < 0 {
				j = len(s) - i - 1
			}
			out = append(out, widthToken{kind: tokIdent, text: s[i+1 : i+1+j]})
			i += j + 2
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && (isWordByte(s[j]) || s[j] == '.' ||
				(s[j] == '+' || s[j] == '-') && (s[j-1] == 'e' || s[j-1] == 'E')) {
				j++
			}
			out = append(out, widthToken{kind: tokNumber, text: s[i:j]})
			i = j
		case isWordByte(c):
			j := i + 1
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			out = append(out, widthToken{kind: tokIdent, text: s[i:j]})
			i = j
		default:
			out = append(out, widthToken{kind: tokPunct, text: s[i : i+1]})
			i++
		}
	}
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// callArgs splits the tokens after a call's "(" into its top-level arguments,
// up to the matching ")". A leading unary "+" is dropped from an argument.
func callArgs(toks []widthToken) [][]widthToken {
	var args [][]widthToken
	var cur []widthToken
	depth := 0
	for _, t := range toks {
		switch {
		case t.kind == tokPunct && t.text == "(":
			depth++
		case t.kind == tokPunct && t.text == ")":
			if depth == 0 {
				return append(args, trimPlus(cur))
			}
			depth--
		case t.kind == tokPunct && t.text == "," && depth == 0:
			args = append(args, trimPlus(cur))
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	return append(args, trimPlus(cur))
}

func trimPlus(arg []widthToken) []widthToken {
	if len(arg) > 1 && arg[0].kind == tokPunct && arg[0].text == "+" {
		return arg[1:]
	}
	return arg
}
