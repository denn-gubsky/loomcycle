// Package fence escapes untrusted text that is about to be wrapped in a
// <kind>…</kind> block for a model, so that nothing inside the block can read
// as the tag that closes it.
package fence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// lt is the replacement for every spelling of "<". It is a look-alike itself,
// which is the point: every spelling collapses to one character that is never
// a tag, and code such as "a < b" stays readable as "a ‹ b". It is a fixed
// character, not a per-run delimiter, because the escaped text is replayed
// into the prompt on every turn and has to stay byte-stable for the
// provider's prompt cache.
const lt = "‹"

// ltSpellings matches every way of writing "<" that a model may read as one:
//
//   - the character and its look-alikes: fullwidth (FF1C), small (FE64), the
//     angle quotation marks and ornaments (2039, 276C, 276E, 2770), the angle
//     brackets (2329, 27E8, 29FC, 3008), the modifier arrowhead (02C2), the
//     syllabic and runic shapes (1438, 16B2), and not-less-than (226E), which
//     decomposes to "<" plus a combining stroke;
//   - the HTML entities, named or numeric, any case, leading zeros, with or
//     without the ";", behind "&" or one of its own look-alikes (FF06, FE60);
//   - the escapes of code and URLs: \u003c, \u{3c}, \x3c, %3c.
//
// A list of spellings cannot be proved complete. What it does is remove the
// ones a model demonstrably decodes; the allowlisted tag name and the fixed
// block layout are the rest of the defence.
var ltSpellings = regexp.MustCompile(`(?i)[<\x{FF1C}\x{FE64}\x{2039}\x{276C}\x{276E}\x{2770}\x{2329}\x{27E8}\x{29FC}\x{3008}\x{02C2}\x{1438}\x{16B2}\x{226E}]` +
	`|[&\x{FF06}\x{FE60}](?:lt|#0*60|#x0*3c);?` +
	`|\\u\{?0*3c\}?|\\x0*3c|%3c`)

// EscapeUntrusted returns s with no way left to write a tag:
//
//   - invisible characters are dropped: the format characters (Unicode
//     category Cf: zero-width spaces and joiners, bidi controls, tag
//     characters), the variation selectors and the other default-ignorable
//     code points. One placed inside a tag or an entity hides it from a
//     pattern while a model reads straight through it;
//   - every spelling of "<" becomes "‹".
//
// The invisible characters go first, so removing one cannot assemble a
// spelling the second step has already passed over.
func EscapeUntrusted(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
			return -1
		}
		return r
	}, s)
	return ltSpellings.ReplaceAllLiteralString(s, lt)
}

// CodeKind is the untrusted kind whose text reaches the model exactly as
// written, for code under review: escaping turns `Vec<T>` into `Vec‹T>`,
// hides the difference between `&lt;` and `<`, and drops the bidi controls a
// Trojan Source change is made of, so a reviewer could neither read the code
// as it is nor quote it back.
const CodeKind = "code"

// codeTag is the tag a code block is fenced in. It is not CodeKind: a
// caller-chosen kind must never name a tag whose closing form it can predict.
const codeTag = "untrusted-code"

// codeBoundaryHexLen is the length of a code block's boundary in hex
// characters: 128 bits. A text that holds its own boundary is a fixed point of
// the hash; at 128 bits nobody finds one, and CodeBlock checks for it anyway.
const codeBoundaryHexLen = 32

// codeBoundaryTries is how many salted boundaries CodeBlock tries before it
// gives up on keeping the text intact and escapes it instead.
const codeBoundaryTries = 8

// CodeBlock fences untrusted code without changing a character of it:
//
//	<untrusted-code b="H">
//	…the code…
//	</untrusted-code b="H">
//
// H is derived from the text, so nothing inside needs escaping: the text
// cannot hold the closing tag of its own boundary, and a closing tag with any
// other boundary is just more text inside the block. The boundary depends only
// on the text, so the block is byte-identical every time it is replayed and
// the prompt cache holds.
//
// Invisible characters are shown rather than dropped, as ⟨U+202E⟩: a reviewer
// has to see a bidi control to flag it, and none can hide inside the boundary.
func CodeBlock(text string) string {
	return codeBlock(text, sha256.Sum256)
}

// codeBlock is CodeBlock with the hash passed in, so a test can force the
// case where the text holds its boundary.
func codeBlock(text string, sum func([]byte) [sha256.Size]byte) string {
	shown := ShowInvisible(text)
	folded := strings.ToLower(shown)
	for i := 0; i < codeBoundaryTries; i++ {
		in := shown
		if i > 0 {
			in = strconv.Itoa(i) + "\n" + shown
		}
		h := sum([]byte(in))
		b := hex.EncodeToString(h[:])[:codeBoundaryHexLen]
		// The boundary must not appear in the text at all, in either case,
		// not only inside a closing tag: then no spelling of the closing tag
		// a model might accept can be written without knowing it.
		if !strings.Contains(folded, b) {
			return fmt.Sprintf("<%s b=%q>\n%s\n</%s b=%q>", codeTag, b, shown, codeTag, b)
		}
	}
	return fmt.Sprintf("<untrusted>\n%s\n</untrusted>", EscapeUntrusted(text))
}

// ShowInvisible writes every character a reader cannot see as ⟨U+XXXX⟩: the
// ones EscapeUntrusted drops (format characters such as bidi controls and
// zero-width joiners, variation selectors, other default-ignorables), and
// the control characters other than tab, line feed and carriage return.
func ShowInvisible(s string) string {
	if strings.IndexFunc(s, hidden) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		if hidden(r) {
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hidden(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	return unicode.IsControl(r) ||
		unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
}
