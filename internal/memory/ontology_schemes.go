package memory

// ontology_schemes.go — the Schemes section of the ontology document (RFC FO P1c).
//
// Each entry under `@section schemes` is a SKOS concept scheme — topics, a normative
// rank, a file plan — and its children are CONCEPTS; a child concept is NARROWER than
// its parent. Concepts are individuals, not classes: they carry no fields and inherit
// nothing, which is why they live in their own section rather than under Classes.
//
// A concept's identity across scopes is its MIRROR TAG, `<scheme>/<path>` (RFC FO
// decision 10f.2). A statement cannot cross scopes, so a document in a user's scope can
// never link to a concept chunk in the tenant ontology; a tag can be carried anywhere,
// and the existing tag_prefix filters already give "this concept and everything
// narrower". Pure parsing — no I/O.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OntologyScheme is one concept scheme.
type OntologyScheme struct {
	Name string `json:"name"`
	// Ordered makes a concept's position its rank (what a precedence relation compares).
	Ordered bool `json:"ordered,omitempty"`
	// Render lists the scheme in the extraction prompt.
	Render   bool              `json:"render,omitempty"`
	Meaning  string            `json:"meaning,omitempty"`
	Concepts []OntologyConcept `json:"concepts,omitempty"`
	ChunkID  string            `json:"chunk_id,omitempty"`
	Issues   []string          `json:"issues,omitempty"`
}

// OntologyConcept is one concept of a scheme.
type OntologyConcept struct {
	Scheme string `json:"scheme"`
	Name   string `json:"name"`
	// Path is the concept's names from the scheme's top level down to itself.
	Path []string `json:"path"`
	// Parent is the broader concept's name, "" at the scheme's top level.
	Parent string `json:"parent,omitempty"`
	// Rank is the 1-based position in document order when the scheme is @ordered.
	Rank       int      `json:"rank,omitempty"`
	Notation   string   `json:"notation,omitempty"`
	Alt        []string `json:"alt,omitempty"`
	ExactMatch string   `json:"exact_match,omitempty"`
	// Retention and ACL are a file-plan concept's policy, carried as data. Not enforced
	// yet (RFC FO decision 10f.1): enforcement is a later phase of its own.
	Retention string `json:"retention,omitempty"`
	ACL       string `json:"acl,omitempty"`
	// Tag is the concept's mirror tag, `<scheme>/<path>`.
	Tag     string   `json:"tag"`
	Meaning string   `json:"meaning,omitempty"`
	ChunkID string   `json:"chunk_id,omitempty"`
	Issues  []string `json:"issues,omitempty"`
}

// ParseSchemeBody reads a scheme entry's body: @ordered, @render and its prose.
func ParseSchemeBody(name, body string) OntologyScheme {
	s := OntologyScheme{Name: name, Meaning: firstProseLine(body)}
	for _, d := range ParseDirectives(body) {
		switch d.Name {
		case "ordered":
			s.Ordered = true
		case "render":
			s.Render = true
		case "section":
		default:
			s.Issues = append(s.Issues, directiveIssue(d.Name, "scheme"))
		}
	}
	return s
}

// ParseConceptBody reads one concept entry's body. A concept carries no fields, so a
// field bullet is reported rather than silently kept.
func ParseConceptBody(scheme, name string, path []string, body string) OntologyConcept {
	c := OntologyConcept{Scheme: scheme, Name: name, Path: append([]string{}, path...),
		Tag: MirrorTag(scheme, path), Meaning: firstProseLine(body)}
	if len(path) > 1 {
		c.Parent = path[len(path)-2]
	}
	for _, d := range ParseDirectives(body) {
		switch d.Name {
		case "notation":
			c.Notation = firstWord(d.Value)
		case "alt":
			if v := strings.TrimSpace(d.Value); v != "" {
				c.Alt = append(c.Alt, v)
			}
		case "exact_match":
			c.ExactMatch = firstWord(d.Value)
		case "retention":
			v := firstWord(d.Value)
			if _, ok := ParseRetention(v); !ok {
				c.Issues = append(c.Issues, "@retention "+strconv.Quote(d.Value)+
					" is not a duration (want e.g. 90d, 6w, 18mo, 10y); ignored")
				continue
			}
			c.Retention = v
		case "acl":
			c.ACL = firstWord(d.Value)
		default:
			c.Issues = append(c.Issues, directiveIssue(d.Name, "concept"))
		}
	}
	if len(ParseOntologyFields(body)) > 0 {
		c.Issues = append(c.Issues, "a concept carries no fields; the field bullets here are ignored")
	}
	return c
}

func directiveIssue(name, what string) string {
	if knownDirectives[name] {
		return "@" + name + " is not a " + what + " directive; ignored"
	}
	return "unknown directive @" + name + "; ignored"
}

// MirrorTag is a concept's tag: the scheme and each path segment, slash-joined. One
// definition shared by the mirror and every reader, so the tag a concept is written with
// and the tag a document is classified with can never differ.
func MirrorTag(scheme string, path []string) string {
	segs := make([]string, 0, len(path)+1)
	segs = append(segs, mirrorSegment(scheme))
	for _, p := range path {
		segs = append(segs, mirrorSegment(p))
	}
	return strings.Join(segs, "/")
}

// mirrorSegment lowercases a title and joins its words with '-'. A '/' inside a title
// becomes '-' too: it is the tag hierarchy separator, and a title containing one would
// otherwise fabricate a level of nesting nobody declared.
func mirrorSegment(s string) string {
	s = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "/", " ")
	return strings.Join(strings.Fields(s), "-")
}

var retentionRe = regexp.MustCompile(`^(\d+)(d|w|mo|y)$`)

// ParseRetention reads a retention period: Nd, Nw, Nmo (months), Ny, or a Go duration
// (72h). Months and years are calendar-approximate (30 and 365 days); a retention
// period measured in years does not need leap-day precision. Zero or negative is not a
// period.
func ParseRetention(s string) (time.Duration, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if m := retentionRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			return 0, false
		}
		day := 24 * time.Hour
		unit := map[string]time.Duration{"d": day, "w": 7 * day, "mo": 30 * day, "y": 365 * day}[m[2]]
		return time.Duration(n) * unit, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// RenderSchemes composes the extraction prompt's scheme listing: only schemes marked
// @render, each concept with its notation and alternative labels, indented under its
// broader concept. "" when no scheme asks to be rendered, so a deployment that never
// marks one sees no prompt change.
//
// HOUSE RULE: model-visible text — no internal RFC citations.
func RenderSchemes(schemes []OntologyScheme) string {
	var rendered []OntologyScheme
	for _, s := range schemes {
		if s.Render && len(s.Concepts) > 0 {
			rendered = append(rendered, s)
		}
	}
	if len(rendered) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Concept schemes\n\n" +
		"When you say what something is about or how it is classified, use these concepts; " +
		"an indented concept is narrower than the one above it.\n")
	for _, s := range rendered {
		b.WriteString("\n**" + s.Name + "**")
		if s.Ordered {
			b.WriteString(" (in order, highest first)")
		}
		b.WriteString("\n")
		for _, c := range s.Concepts {
			depth := len(c.Path) - 1
			if depth > OntologyMaxDepth {
				depth = OntologyMaxDepth
			}
			b.WriteString(strings.Repeat("  ", depth) + "- " + c.Name)
			if c.Notation != "" {
				b.WriteString(" (" + c.Notation + ")")
			}
			if len(c.Alt) > 0 {
				b.WriteString(" — also: " + strings.Join(c.Alt, ", "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
