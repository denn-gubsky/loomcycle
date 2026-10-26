package helper

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A helper template is tokenized into argv here, never by a shell. The rules:
//
//   - Words are separated by unquoted spaces, tabs and newlines.
//   - '…' quotes everything up to the next ', literally: no escapes, no
//     placeholders.
//   - "…" quotes up to the next unescaped "; inside it \" and \\ are a quote
//     and a backslash, any other backslash is literal, and {name} is a
//     placeholder.
//   - Outside quotes a backslash makes the next character literal.
//   - {name} (name = letter or _, then letters, digits, _) is a placeholder.
//     A '{' that does not open one is an error; write \{ or '{' for a brace.
//   - The shell characters | ; & < > ` $ ( ) are refused outside quotes:
//     there is no shell to give them meaning, and an operator who writes
//     "ffmpeg … | other" should learn that at validation, not from a failed
//     conversion. Quoted, they are ordinary characters.
//   - Quotes join: pre'{x}'post is one word. '' and "" make an empty word.
//
// A placeholder fills exactly the part of the word it stands in, with its value
// byte for byte: a value can never add, remove or split a word. The first word
// (the program) may hold no placeholder, so a value never chooses what runs.

// segment is a run of literal text or one placeholder.
type segment struct {
	text        string
	placeholder bool
}

type word []segment

// shellChars are refused outside quotes.
const shellChars = "|;&<>`$()"

func parseTemplate(tmpl string) ([]word, error) {
	var (
		words  []word
		cur    word
		lit    strings.Builder
		inWord bool
	)
	flush := func() {
		if lit.Len() > 0 {
			cur = append(cur, segment{text: lit.String()})
			lit.Reset()
		}
	}
	endWord := func() {
		if inWord {
			flush()
			if cur == nil {
				cur = word{} // an explicit empty word ('' or "")
			}
			words = append(words, cur)
		}
		cur, inWord = nil, false
	}
	// placeholder reads {name} starting at tmpl[i] == '{' and returns the
	// index after the closing brace.
	placeholder := func(i int) (int, error) {
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			return 0, fmt.Errorf("unclosed placeholder at %q", tmpl[i:])
		}
		name := tmpl[i+1 : i+end]
		if !validName(name) {
			return 0, fmt.Errorf("%q is not a placeholder (a name is a letter or _, then letters, digits or _; write \\{ for a literal brace)", tmpl[i:i+end+1])
		}
		flush()
		cur = append(cur, segment{text: name, placeholder: true})
		return i + end + 1, nil
	}

	for i := 0; i < len(tmpl); {
		c := tmpl[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			endWord()
			i++
		case c == '\'':
			inWord = true
			end := strings.IndexByte(tmpl[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			lit.WriteString(tmpl[i+1 : i+1+end])
			i += end + 2
		case c == '"':
			inWord = true
			i++
			for {
				if i >= len(tmpl) {
					return nil, errors.New("unterminated double quote")
				}
				d := tmpl[i]
				if d == '"' {
					i++
					break
				}
				if d == '\\' && i+1 < len(tmpl) && (tmpl[i+1] == '"' || tmpl[i+1] == '\\') {
					lit.WriteByte(tmpl[i+1])
					i += 2
					continue
				}
				if d == '{' {
					next, err := placeholder(i)
					if err != nil {
						return nil, err
					}
					i = next
					continue
				}
				lit.WriteByte(d)
				i++
			}
		case c == '\\':
			if i+1 >= len(tmpl) {
				return nil, errors.New("trailing backslash")
			}
			inWord = true
			lit.WriteByte(tmpl[i+1])
			i += 2
		case c == '{':
			inWord = true
			next, err := placeholder(i)
			if err != nil {
				return nil, err
			}
			i = next
		case strings.IndexByte(shellChars, c) >= 0:
			return nil, fmt.Errorf("%q is shell syntax, and helper commands run without a shell; quote it to pass it literally", string(c))
		default:
			inWord = true
			lit.WriteByte(c)
			i++
		}
	}
	endWord()
	if len(words) == 0 {
		return nil, errors.New("the template names no program")
	}
	for _, s := range words[0] {
		if s.placeholder {
			return nil, fmt.Errorf("the program may not come from a placeholder ({%s})", s.text)
		}
	}
	return words, nil
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// expand fills each placeholder from vars. A placeholder with no value is an
// error, as is a value holding NUL (it cannot cross exec) or one that starts a
// word with '-', where the program would read it as an option.
func expand(words []word, vars map[string]string) ([]string, error) {
	argv := make([]string, 0, len(words))
	for _, w := range words {
		var b strings.Builder
		for j, s := range w {
			if !s.placeholder {
				b.WriteString(s.text)
				continue
			}
			v, ok := vars[s.text]
			if !ok {
				return nil, fmt.Errorf("unknown placeholder {%s}", s.text)
			}
			if strings.IndexByte(v, 0) >= 0 {
				return nil, fmt.Errorf("the value of {%s} holds a NUL byte", s.text)
			}
			if j == 0 && strings.HasPrefix(v, "-") {
				return nil, fmt.Errorf("the value of {%s} starts with '-' where it begins an argument, and would be read as an option", s.text)
			}
			b.WriteString(v)
		}
		argv = append(argv, b.String())
	}
	return argv, nil
}

// Validate checks a helper template the way Run will: its syntax, and that it
// uses no placeholder outside allowed. Config validation calls it at boot so a
// broken helper is refused before the first file needs it.
func Validate(template string, allowed []string) error {
	words, err := parseTemplate(template)
	if err != nil {
		return err
	}
	for _, w := range words {
		for _, s := range w {
			if s.placeholder && !slices.Contains(allowed, s.text) {
				return fmt.Errorf("unknown placeholder {%s} (this helper takes %s)", s.text, placeholderList(allowed))
			}
		}
	}
	return nil
}

func placeholderList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "{" + n + "}"
	}
	return strings.Join(out, ", ")
}
