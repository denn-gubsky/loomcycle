// Package fence escapes untrusted text that is about to be wrapped in a
// <kind>…</kind> block for a model, so that nothing inside the block can read
// as the tag that closes it.
package fence

import (
	"regexp"
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
