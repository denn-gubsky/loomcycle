package memory

// consolidator_key.go — the memory consolidator's subject key, in Go (RFC FO P1c-2).
//
// The consolidator (a code-js agent in bundles/memory/loomcycle.yaml) keys every subject
// node `type + ":" + slug(normalizeSubject(name))`. An ontology instance adopted into the
// registry must carry EXACTLY that key: the consolidator finds an existing subject by
// its slug, so a different key would make it mint a second node for the same thing the
// first time it learned a fact about it (RFC FO decision 10f.3).
//
// This is a PORT, held to the JavaScript by TestConsolidatorEntityKey_MatchesTheBundle,
// which runs the bundle's own functions in goja on the same inputs. Change the bundle
// and that test says so; never change one side alone.

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// consolidatorStopwords is the bundle's STOPWORDS.
var consolidatorStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "to": true, "of": true, "in": true, "on": true, "at": true,
	"for": true, "and": true, "or": true, "that": true, "this": true, "it": true,
	"their": true, "his": true, "her": true, "its": true, "has": true, "have": true,
	"with": true, "as": true, "by": true, "from": true, "they": true, "them": true,
}

// ConsolidatorEntityKey is the bundle's entityKey(type, subject).
func ConsolidatorEntityKey(typ, subject string) string {
	return typ + ":" + consolidatorSlug(consolidatorNormalizeSubject(subject))
}

// ConsolidatorSlug is the bundle's slug(normalizeSubject(subject)) — the path segment a
// subject document is filed under (/facts/<slug>).
func ConsolidatorSlug(subject string) string {
	return consolidatorSlug(consolidatorNormalizeSubject(subject))
}

// consolidatorNormalizeSubject: lowercase, collapse whitespace runs to one space, trim,
// drop one leading article.
func consolidatorNormalizeSubject(s string) string {
	s = jsCollapseSpace(jsToLower(s))
	for _, art := range []string{"the", "a", "an"} {
		if strings.HasPrefix(s, art) && len(s) > len(art) && jsIsSpace(rune(s[len(art)])) {
			return strings.TrimLeftFunc(s[len(art):], jsIsSpace)
		}
	}
	return s
}

// jsToLower is String.prototype.toLowerCase AS THE CONSOLIDATOR'S ENGINE RUNS IT: goja
// lowercases an ASCII string with strings.ToLower and anything else with x/text's full
// Unicode case mapping (a final Σ becomes ς, İ becomes i + U+0307), then applies one
// Greek workaround. Copied from goja's string_unicode.go toLower — the engine, not the
// ECMAScript spec, is what the consolidator's keys are computed by.
func jsToLower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	r := []rune(cases.Lower(language.Und).String(s))
	for i := 0; i < len(r)-1; i++ {
		if (i == 0 || r[i-1] != 0x3b1) && r[i] == 0x345 && r[i+1] == 0x3c2 {
			i++
			r[i] = 0x3c3
		}
	}
	return string(r)
}

// jsCollapseSpace is `.replace(/\s+/g, " ").trim()` with JavaScript's \s — which, unlike
// RE2's ASCII-only \s, includes the Unicode space separators and U+FEFF.
func jsCollapseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if jsIsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return strings.TrimFunc(b.String(), jsIsSpace)
}

// jsIsSpace is JavaScript's WhiteSpace ∪ LineTerminator (what \s and trim() match).
func jsIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// consolidatorRawWords is the bundle's rawWords: ASCII letters and digits only, every
// other run a separator.
func consolidatorRawWords(text string) []string {
	var b strings.Builder
	dash := false
	for _, r := range jsToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Split(strings.Trim(b.String(), "-"), "-")
}

// consolidatorSlug is the bundle's slug: up to six content words, at most 60
// characters, and a fingerprint fallback for text that tokenises to nothing (any
// non-Latin name).
func consolidatorSlug(text string) string {
	words := consolidatorRawWords(text)
	var keep []string
	for i := 0; i < len(words) && len(keep) < 6; i++ {
		if words[i] != "" && !consolidatorStopwords[words[i]] {
			keep = append(keep, words[i])
		}
	}
	if len(keep) == 0 {
		if len(words) > 6 {
			words = words[:6]
		}
		keep = words
	}
	out := strings.Join(keep, "-")
	if len(out) > 60 {
		out = strings.TrimRight(out[:60], "-")
	}
	if out == "" {
		return "unnamed-" + consolidatorTextFingerprint(text)
	}
	return out
}

// consolidatorTextFingerprint is the bundle's textFingerprint: two djb2 variants over
// UTF-16 code units (JavaScript's charCodeAt), int32 arithmetic, base36.
func consolidatorTextFingerprint(text string) string {
	var a, b int32 = 5381, 52711
	for _, c := range utf16Units(text) {
		a = a<<5 + a + int32(c)
		b = b<<5 + b + int32(c^0x5f)
	}
	return strconv.FormatUint(uint64(uint32(a)), 36) + strconv.FormatUint(uint64(uint32(b)), 36)
}

// utf16Units is a string's UTF-16 code units, as JavaScript indexes them: a rune above
// U+FFFF is a surrogate pair.
func utf16Units(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		if r > 0xffff {
			r -= 0x10000
			out = append(out, uint16(0xd800+(r>>10)), uint16(0xdc00+(r&0x3ff)))
			continue
		}
		out = append(out, uint16(r))
	}
	return out
}
