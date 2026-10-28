package memory

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The directory half of the user-root Document: what OTHER people's agents in
// the tenant may read about its owner.
//
// WHY A SECTION, NOT THE DOCUMENT. The rest of the profile is written for the
// owner's own agents — standing preferences are instructions ("terse answers,
// never push") and locale is personal. Read into a colleague's agent, the
// first is an injection vector and the second a disclosure. One declared
// section is the smallest thing that is safe to hand out, and an empty one is
// the opt-out.

// Section headings of the user-root Document the runtime reads mechanically.
const (
	UserRootPublicHeading   = "Public"
	UserRootIdentityHeading = "Identity"
)

// PublicMaxBytes caps what PublicSection returns. A directory entry is a few
// lines; a profile that declares pages of "public" text is not one, and the
// reader of it is another person's model context.
const PublicMaxBytes = 4096

// displayNameMaxRunes bounds the name seeded into a new profile.
const displayNameMaxRunes = 120

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// UserRootSection returns the body of the level-2 section titled heading in a
// clean (include_metadata=false) export of the user-root Document, its own
// sub-sections included, or "" when there is none. The match is on the
// heading text, case-insensitively; a heading inside a fenced code block is
// not a heading.
func UserRootSection(md, heading string) string {
	var b strings.Builder
	in, fence := false, false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = !fence
		}
		if !fence {
			if level, title, ok := markdownHeading(line); ok && level <= 2 {
				if in {
					break
				}
				in = level == 2 && strings.EqualFold(title, heading)
				continue
			}
		}
		if in {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// PublicSection is what the owner of the user-root Document declared public:
// its Public section with the template's guidance (an HTML comment) and any
// other comment removed, trimmed, and cut at PublicMaxBytes on a rune
// boundary. truncated reports the cut. "" means the owner shares nothing
// beyond their name.
func PublicSection(md string) (text string, truncated bool) {
	text = strings.TrimSpace(htmlComment.ReplaceAllString(UserRootSection(md, UserRootPublicHeading), ""))
	if len(text) <= PublicMaxBytes {
		return text, false
	}
	cut := PublicMaxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]), true
}

// IdentityNames are the names the owner declared in the Identity section
// only — ParseSelfNames over that section, so a bullet written anywhere else
// in the document is never read out to another user.
func IdentityNames(md string) []string {
	return ParseSelfNames(UserRootSection(md, UserRootIdentityHeading))
}

// UserRootTemplateFor is UserRootTemplate with the Public section pre-filled
// with the user's display name, the one thing the deployment already knows
// and shows about them. The person adds the rest. An empty name, or one that
// is nothing but control characters, leaves the template as it is.
func UserRootTemplateFor(displayName string) string {
	name := singleLine(displayName, displayNameMaxRunes)
	if name == "" {
		return userRootTemplate
	}
	const marker = "\n## Public\n"
	i := strings.Index(userRootTemplate, marker)
	if i < 0 {
		return userRootTemplate
	}
	body := userRootTemplate[i+len(marker):]
	end := strings.Index(body, "\n## ")
	if end < 0 {
		end = len(body)
	}
	section := strings.TrimRight(body[:end], "\n") + "\n\nName: " + name + "\n"
	return userRootTemplate[:i+len(marker)] + section + body[end:]
}

// singleLine folds s onto one line (every run of whitespace or control
// characters becomes one space), trimmed and cut at max runes. A heading or
// list marker at its start is defused by the "Name: " prefix it is written
// after.
func singleLine(s string, max int) string {
	var b strings.Builder
	space, n := false, 0
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if n == max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// markdownHeading reads an ATX heading: its level, its title, and whether the
// line is one. Up to three leading spaces are allowed, as CommonMark allows.
func markdownHeading(line string) (int, string, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, "", false
	}
	level := 0
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(t) && t[level] != ' ' && t[level] != '\t') {
		return 0, "", false
	}
	title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(t[level:]), "#"))
	return level, title, true
}
