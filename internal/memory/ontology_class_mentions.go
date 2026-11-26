package memory

// ontology_class_mentions.go — the places the ontology itself names a class, and their
// rewrite when the class is renamed (RFC GQ 9b C12; owner ruling 15.1 of 2026-11-25:
// the rename job rewrites them in the same change).
//
// Four places name a class by name:
//
//	@a <class>                       an instance's class
//	@projects <class>, <class>       a scheme's projected classes
//	@domain / @range <class> | …     a relation's ends
//	- `field` ref(<class>)           a class's reference field
//
// The rewrite is textual and follows the readers exactly (ParseDirectives,
// ParseInstanceBody, ParseSchemeBody, ParseRelationBody, ParseOntologyFieldSpecs): a
// token is replaced only where the reader takes it as a class name, so prose, an
// example in a fence or an indented block, and every other directive keep their text.
// A concept field's range names a scheme, not a class, and is left alone.

import "strings"

// Entry kinds whose bodies can name a class (the plan's kinds).
const (
	MentionKindClass    = "class"
	MentionKindRelation = "relation"
	MentionKindScheme   = "scheme"
	MentionKindInstance = "instance"
)

// RewriteClassMentions replaces, in one entry's body, every mention of a class named in
// from (matched under Unicode case folding) by to. kind is the entry's kind; a kind that
// names no class returns the body unchanged. n is the number of mentions replaced.
func RewriteClassMentions(kind, body string, from []string, to string) (string, int) {
	if len(from) == 0 || to == "" {
		return body, 0
	}
	lines := strings.Split(body, "\n")
	inFence, n := false, 0
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		lead := len(line) - len(strings.TrimLeft(line, " \t\r\n\v\f"))
		var nt string
		var k int
		if strings.HasPrefix(t, "- ") {
			// A bullet: a directive in its bullet form, or a field (ParseDirectives reads
			// bullets inside a fence too, so this comes before the fence check).
			nt, k = rewriteBulletMention(kind, t, from, to)
		} else if !inFence && !strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "\t") {
			nt, k = rewriteBareMention(kind, t, from, to)
		}
		if k > 0 {
			lines[i] = line[:lead] + nt + line[lead+len(t):]
			n += k
		}
	}
	return strings.Join(lines, "\n"), n
}

// rewriteBulletMention handles `- `@directive` value` and `- `field` ref(Class)`.
func rewriteBulletMention(kind, t string, from []string, to string) (string, int) {
	open := strings.Index(t, "`")
	if open < 0 {
		return t, 0
	}
	closeRel := strings.Index(t[open+1:], "`")
	if closeRel <= 0 {
		return t, 0
	}
	closeAt := open + 1 + closeRel
	name := strings.TrimSpace(t[open+1 : closeAt])
	tailStart := closeAt + 1
	if strings.HasPrefix(name, "@") {
		if len(name) < 2 {
			return t, 0
		}
		// bulletDirectiveValue: the tail, trimmed, less a leading ":" or "=", cut at " — ".
		s := tailStart
		for s < len(t) && isSpaceByte(t[s]) {
			s++
		}
		for s < len(t) && (t[s] == ':' || t[s] == '=') {
			s++
		}
		e := len(t)
		if k := strings.Index(t[s:], " — "); k >= 0 {
			e = s + k
		}
		return rewriteDirectiveValue(kind, strings.ToLower(name[1:]), t, s, e, from, to)
	}
	if kind != MentionKindClass || name == "" {
		return t, 0
	}
	// parseFieldTail: a ref field is `ref(` as the first word of the tail; its class runs
	// from the first "(" to the first ")".
	s := tailStart
	for s < len(t) && isSpaceByte(t[s]) {
		s++
	}
	if !strings.HasPrefix(strings.ToLower(t[s:]), "ref(") {
		return t, 0
	}
	po := strings.Index(t[s:], "(")
	pc := strings.Index(t[s:], ")")
	if pc < po {
		return t, 0
	}
	return replaceTrimmed(t, s+po+1, s+pc, from, to)
}

// rewriteBareMention handles `@directive value` on a line of its own.
func rewriteBareMention(kind, t string, from []string, to string) (string, int) {
	m := bareDirectiveRe.FindStringSubmatchIndex(t)
	if m == nil || m[4] < 0 {
		return t, 0
	}
	return rewriteDirectiveValue(kind, strings.ToLower(t[m[2]:m[3]]), t, m[4], m[5], from, to)
}

// rewriteDirectiveValue rewrites the class names in t[s:e], the value of directive name
// on an entry of kind, the way that entry's reader splits the value.
func rewriteDirectiveValue(kind, name, t string, s, e int, from []string, to string) (string, int) {
	switch {
	case kind == MentionKindInstance && name == "a":
		return replaceTrimmed(t, s, e, from, to)
	case kind == MentionKindScheme && name == "projects":
		// splitNameList: comma-separated, each part trimmed.
		return replaceParts(t, s, e, ",", from, to, replaceTrimmed)
	case kind == MentionKindRelation && (name == "domain" || name == "range"):
		// splitClassList: "|" or "," separated, the first word of each part.
		return replaceParts(t, s, e, "|,", from, to, replaceFirstWord)
	}
	return t, 0
}

// replaceParts applies one to each part of t[s:e] between separator bytes, right to
// left so earlier offsets stay valid.
func replaceParts(t string, s, e int, seps string, from []string, to string,
	one func(t string, s, e int, from []string, to string) (string, int)) (string, int) {
	var bounds [][2]int
	start := s
	for i := s; i < e; i++ {
		if strings.IndexByte(seps, t[i]) >= 0 {
			bounds = append(bounds, [2]int{start, i})
			start = i + 1
		}
	}
	bounds = append(bounds, [2]int{start, e})
	n := 0
	for i := len(bounds) - 1; i >= 0; i-- {
		var k int
		t, k = one(t, bounds[i][0], bounds[i][1], from, to)
		n += k
	}
	return t, n
}

// replaceTrimmed replaces t[s:e], less its surrounding spaces, when it is a name in from.
func replaceTrimmed(t string, s, e int, from []string, to string) (string, int) {
	for s < e && isSpaceByte(t[s]) {
		s++
	}
	for e > s && isSpaceByte(t[e-1]) {
		e--
	}
	if s == e || !foldIn(t[s:e], from) {
		return t, 0
	}
	return t[:s] + to + t[e:], 1
}

// replaceFirstWord replaces the first word of t[s:e] when it is a name in from.
func replaceFirstWord(t string, s, e int, from []string, to string) (string, int) {
	for s < e && isSpaceByte(t[s]) {
		s++
	}
	w := s
	for w < e && !isSpaceByte(t[w]) {
		w++
	}
	if s == w || !foldIn(t[s:w], from) {
		return t, 0
	}
	return t[:s] + to + t[w:], 1
}

func foldIn(v string, names []string) bool {
	for _, n := range names {
		if strings.EqualFold(v, n) {
			return true
		}
	}
	return false
}

// isSpaceByte matches the ASCII whitespace strings.Fields and TrimSpace split on; the
// rewritten tokens are names, so a multi-byte space inside one is left as text.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
