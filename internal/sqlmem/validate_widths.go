package sqlmem

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// validateWidths refuses an agent statement that asks, in a literal it can
// see, for a value longer than limit: zeroblob(N) / randomblob(N) and a
// printf/format width or precision on sqlite, repeat/lpad/rpad on postgres.
// The engine's own limit (SQLITE_LIMIT_LENGTH, set on every agent statement)
// is what holds on sqlite whatever the statement computes; this check gives
// the model a clear refusal for the plain case, and on postgres, which has no
// such limit, it is the bound for a literal width (the result cap still
// refuses the row, after the driver has read it).
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
		over := func(arg []widthToken) bool { return len(arg) == 1 && overLimit(arg[0], limit) }
		switch {
		case d == dialectSQLite && (name == "zeroblob" || name == "randomblob"):
			if len(args) > 0 && over(args[0]) {
				return refuse("%s(%s) asks for a value over the %d-byte limit on one SQL Memory value", name, args[0][0].text, limit)
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
					if over(a) {
						return refuse("%s asks for a width of %s, over the %d-byte limit on one SQL Memory value", name, a[0].text, limit)
					}
				}
			}
		case d == dialectPostgres && (name == "repeat" || name == "lpad" || name == "rpad"):
			if len(args) > 1 && over(args[1]) {
				return refuse("%s(…, %s) asks for a value over the %d-byte limit on one SQL Memory value", name, args[1][0].text, limit)
			}
		}
	}
	return nil
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

// overLimit reports whether a number token is over limit. An integer too
// large to parse is over it; a real (4e8) counts by its value, as the
// functions take it.
func overLimit(t widthToken, limit int) bool {
	if t.kind != tokNumber {
		return false
	}
	if v, err := strconv.ParseInt(t.text, 0, 64); err == nil {
		return v > int64(limit)
	} else if errors.Is(err, strconv.ErrRange) {
		return true
	}
	if f, err := strconv.ParseFloat(t.text, 64); err == nil {
		return f > float64(limit)
	}
	return false
}
