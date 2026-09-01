package memory

import "strings"

// The identity half of placement: which names mean "the person whose run this is".
//
// WHY IT HAS TO BE DECLARED. Placement can route a fact by the entity type its subject
// carries, but it cannot tell whose fact it is. "Ada prefers Go" and "Maria owns the
// release process" are the same shape, and only one of them belongs to the person running
// the agent. Without knowing the user's own names the guard catches literal self-reference
// ("the user", "me") and nothing else — so a fact recorded about the user under their own
// name was placed like a fact about a colleague.
//
// A DOCUMENT rather than a config field, because the user is who knows the answer: the
// names people actually use for them in conversation are not something an operator can
// enumerate from a directory, and they change. The user-root Document already exists, is
// already per-user, and is already the place a person writes durable context about
// themselves.

// Directives read out of the user-root Document's Identity section.
const (
	SelfNameDirective  = "@name"
	SelfAliasDirective = "@alias"
)

// selfNamesMax bounds what one document can declare. A name list is a handful of spellings;
// a hundred of them is a mistake or an attempt to make the guard refuse everything.
const selfNamesMax = 24

// selfNameMinLen rejects a one-character "name". A single letter matches too much to be
// used as identity, and a stray bullet is more likely than a person called "A".
const selfNameMinLen = 2

// ParseSelfNames extracts the user's declared names from the user-root Document's Markdown.
//
// ⚠️ A BULLET MUST START AT COLUMN 0. That is the whole defence against the shipped
// template arming itself: the template shows the form as an INDENTED code sample, so an
// unedited profile declares nobody. Trimming leading whitespace first — which every other
// bullet parser in this package does — would read "Ada Lovelace" out of the example and
// silently claim the user is called that.
// TestSelfNames_TheShippedTemplateNamesNobody is the test that matters here.
//
// Deliberately lenient about everything else: an unknown directive is skipped, a bullet
// with no value is skipped, and duplicates and case variants collapse. The result is a
// set of names to compare against, so a bad entry costs a wasted comparison rather than a
// failed read.
func ParseSelfNames(md string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(md, "\n") {
		// NO TrimSpace: see the warning above.
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		switch firstBackticked(line) {
		case SelfNameDirective, SelfAliasDirective:
		default:
			continue
		}
		name := selfNameValue(line)
		if len(name) < selfNameMinLen {
			continue
		}
		k := strings.ToLower(name)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, name)
		if len(out) >= selfNamesMax {
			break
		}
	}
	return out
}

// selfNameValue takes the whole remainder after the directive, not the first word — a name
// is "Ada Lovelace", and cutting at the first space would claim the user is called "Ada".
//
// A trailing note is still allowed, set off by a spaced dash the way the rest of these
// templates write one, so "- `@name` Ada Lovelace — the boss" declares "Ada Lovelace".
func selfNameValue(line string) string {
	i := strings.Index(line, "`")
	if i < 0 {
		return ""
	}
	rest := line[i+1:]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	v := strings.TrimSpace(rest[j+1:])
	v = strings.TrimLeft(v, ":=—- \t")
	for _, cut := range []string{" — ", " -- ", " – "} {
		if k := strings.Index(v, cut); k >= 0 {
			v = v[:k]
		}
	}
	return strings.Trim(v, "`\"'.,;: \t")
}
