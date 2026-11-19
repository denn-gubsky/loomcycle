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
	Render bool `json:"render,omitempty"`
	// The header (RFC GM D14) says what the taxonomy is for and where it came from.
	// Domain is the knowledge domain it covers, one line of free text.
	Domain string `json:"domain,omitempty"`
	// Projects names the ontology classes whose things the taxonomy arranges (document,
	// entity or fact classes), subclasses included. Empty means the default: every
	// document class.
	Projects []string `json:"projects,omitempty"`
	// Origin is adopted, declared or generated; "" when the scheme does not say.
	Origin string `json:"origin,omitempty"`
	// Source and Version are an adopted scheme's provenance: where it was published
	// (text or a URL) and which edition of it this is.
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	// Period is the length of the periods the taxonomy's series are kept in (RFC GM
	// 6A.1, D32): day, week or month; "" means the default, month.
	Period   string            `json:"period,omitempty"`
	Meaning  string            `json:"meaning,omitempty"`
	Concepts []OntologyConcept `json:"concepts,omitempty"`
	ChunkID  string            `json:"chunk_id,omitempty"`
	Issues   []string          `json:"issues,omitempty"`
	// EntryStatus is "retired" for a retired scheme ("" when live).
	EntryStatus string `json:"entry_status,omitempty"`
	// Locked: the scheme's header carries @locked (RFC FO 3.1a). It locks the scheme
	// entry; each concept carries its own.
	Locked bool `json:"locked,omitempty"`
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
	// Focus names the class or instance the concept is about (RFC GM D15). Confirming
	// the ontology writes it as a `focus` statement from the concept's chunk.
	Focus string `json:"focus,omitempty"`
	// Retention and ACL are a file-plan concept's policy, carried as data. Not enforced
	// yet (RFC FO decision 10f.1): enforcement is a later phase of its own.
	Retention string `json:"retention,omitempty"`
	ACL       string `json:"acl,omitempty"`
	// Origin is its scheme's @origin, so a reader of one concept knows whether it is
	// adopted — a concept of a published scheme is never renamed or moved by a proposal
	// pass (RFC GM 2B). "" when the scheme declares none.
	Origin string `json:"origin,omitempty"`
	// Tag is the concept's mirror tag, `<scheme>/<path>`.
	Tag     string   `json:"tag"`
	Meaning string   `json:"meaning,omitempty"`
	ChunkID string   `json:"chunk_id,omitempty"`
	Issues  []string `json:"issues,omitempty"`
	// EntryStatus is "retired" for a retired concept: the concept filter, memberships
	// and its policy keep working for what is already placed under it; nothing new is
	// placed there.
	EntryStatus string `json:"entry_status,omitempty"`
	// Locked: the concept's body carries @locked (RFC FO 3.1a).
	Locked bool `json:"locked,omitempty"`
}

// SchemeMaxDepth bounds how many levels of concepts a scheme may hold below itself
// (RFC GM). Deeper than OntologyMaxDepth on purpose: a concept carries no fields and
// inherits nothing, and real classification schemes (subject headings, file plans) run
// deeper than a class hierarchy should. One definition, shared by the reader that
// flattens past it, the prompt renderer and the classify walk down a scheme.
const SchemeMaxDepth = 8

// SchemeDepthIssue is the advisory on a scheme whose concepts nest deeper than
// SchemeMaxDepth. Nothing is discarded: the deeper concepts are kept at the cap.
func SchemeDepthIssue() string {
	n := strconv.Itoa(SchemeMaxDepth)
	return "concepts nest deeper than " + n + " levels below the scheme; the deeper ones were flattened onto level " + n
}

// Scheme origins a scheme header's @origin may name (RFC GM).
const (
	SchemeOriginAdopted   = "adopted"
	SchemeOriginDeclared  = "declared"
	SchemeOriginGenerated = "generated"
)

// Series periods a scheme's @period may name (RFC GM 6A.1, D32). Month is the
// default: P0 found month the only length that holds enough members per node on
// the knowledge bases measured.
const (
	SchemePeriodDay   = "day"
	SchemePeriodWeek  = "week"
	SchemePeriodMonth = "month"
)

// ParseSchemeBody reads a scheme entry's body: @ordered, @render, the header (@domain,
// @projects, @origin, @source, @version, @period) and its prose.
//
// Whether a @projects class exists is not known here — the classes are another
// section — so CheckSchemeReferences reports that once the whole document is read.
func ParseSchemeBody(name, body string) OntologyScheme {
	s := OntologyScheme{Name: name, Meaning: firstProseLine(body)}
	for _, d := range ParseDirectives(body) {
		switch d.Name {
		case "ordered":
			s.Ordered = true
		case "render":
			s.Render = true
		case "domain":
			s.Domain = singleValue(&s.Issues, d, s.Domain, strings.TrimSpace(d.Value))
		case "projects":
			names := splitNameList(d.Value)
			if len(names) == 0 {
				s.Issues = append(s.Issues, "@projects names no class; ignored")
				continue
			}
			s.Projects = append(s.Projects, names...)
		case "origin":
			v := strings.ToLower(firstWord(d.Value))
			switch v {
			case SchemeOriginAdopted, SchemeOriginDeclared, SchemeOriginGenerated:
				s.Origin = singleValue(&s.Issues, d, s.Origin, v)
			default:
				s.Issues = append(s.Issues, "@origin "+strconv.Quote(d.Value)+" is not an origin (want "+
					SchemeOriginAdopted+", "+SchemeOriginDeclared+" or "+SchemeOriginGenerated+"); ignored")
			}
		case "source":
			s.Source = singleValue(&s.Issues, d, s.Source, strings.TrimSpace(d.Value))
		case "version":
			s.Version = singleValue(&s.Issues, d, s.Version, strings.TrimSpace(d.Value))
		case "period":
			v := strings.ToLower(firstWord(d.Value))
			switch v {
			case SchemePeriodDay, SchemePeriodWeek, SchemePeriodMonth:
				s.Period = singleValue(&s.Issues, d, s.Period, v)
			default:
				s.Issues = append(s.Issues, "@period "+strconv.Quote(d.Value)+" is not a period (want "+
					SchemePeriodDay+", "+SchemePeriodWeek+" or "+SchemePeriodMonth+"); the default, month, applies")
			}
		case "section":
		case LockedDirective:
			s.Locked = lockedFlag(d, &s.Issues) || s.Locked
		default:
			s.Issues = append(s.Issues, directiveIssue(d.Name, "scheme"))
		}
	}
	if s.Origin != "" && s.Origin != SchemeOriginAdopted {
		// Kept, not dropped: the operator may be about to change the origin. Said so the
		// header does not claim a provenance the scheme does not have.
		for _, f := range []struct{ name, v string }{{"source", s.Source}, {"version", s.Version}} {
			if f.v != "" {
				s.Issues = append(s.Issues, "@"+f.name+" describes an adopted scheme, and this one is @origin "+s.Origin)
			}
		}
	}
	return s
}

// singleValue is a one-valued directive's value after d: v, unless v is empty (reported,
// the old value kept). A second, different value replaces the first and is reported, so
// an operator who wrote two does not have to guess which one is read.
func singleValue(issues *[]string, d Directive, old, v string) string {
	if v == "" {
		*issues = append(*issues, "@"+d.Name+" needs a value; ignored")
		return old
	}
	if old != "" && old != v {
		*issues = append(*issues, "@"+d.Name+" given more than once; only the last ("+strconv.Quote(v)+") applies")
	}
	return v
}

// splitNameList reads "A, B, C" as class names. Class names may hold spaces, so only
// the comma separates them.
func splitNameList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
		case "focus":
			// The whole value: an instance's name may hold spaces ("Acme Bank").
			c.Focus = singleValue(&c.Issues, d, c.Focus, strings.TrimSpace(d.Value))
		case "retention":
			v := firstWord(d.Value)
			if _, ok := ParseRetention(v); !ok {
				c.Issues = append(c.Issues, "@retention "+strconv.Quote(d.Value)+
					" is not a duration (want e.g. 90d, 6w, 18mo, 10y); ignored")
				continue
			}
			c.Retention = v
		case "acl":
			// One group per concept. What cannot be used is reported rather than dropped
			// silently: a group the author meant to require that is never counted is a
			// policy nobody can see is missing.
			v := firstWord(d.Value)
			if v == "" {
				c.Issues = append(c.Issues, "@acl needs a group name; ignored")
				continue
			}
			if c.ACL != "" && c.ACL != v {
				c.Issues = append(c.Issues, "@acl given more than once; only the last ("+strconv.Quote(v)+") applies")
			}
			c.ACL = v
		case LockedDirective:
			c.Locked = lockedFlag(d, &c.Issues) || c.Locked
		default:
			c.Issues = append(c.Issues, directiveIssue(d.Name, "concept"))
		}
	}
	if len(ParseOntologyFields(body)) > 0 {
		c.Issues = append(c.Issues, "a concept carries no fields; the field bullets here are ignored")
	}
	return c
}

// Focus target kinds.
const (
	FocusClass    = "class"
	FocusInstance = "instance"
)

// FocusTarget is what a concept's @focus names: a class, or an instance of the
// Instances section (whose Class is its @a class).
type FocusTarget struct {
	Kind  string
	Name  string
	Class string
	// Retired: the class or instance named is retired, so no new focus statement may
	// point at it.
	Retired bool
}

// ResolveFocus finds the class or instance a @focus names, case-insensitively — the way
// adoption matches an instance's class and its relation targets. ok is false when the
// name is neither. both reports a name that is a class AND an instance; the instance is
// returned then, as the more specific reading (a concept for one product is about that
// product, not about every product).
//
// classes are the classes in force once the document is confirmed
// (EffectiveClasses(terms, true)), so a seed class can be a focus.
func ResolveFocus(name string, classes []OntologyTerm, instances []OntologyInstance) (t FocusTarget, ok, both bool) {
	n := strings.TrimSpace(name)
	var class *OntologyTerm
	for i := range classes {
		if strings.EqualFold(classes[i].Name, n) {
			class = &classes[i]
			break
		}
	}
	for _, in := range instances {
		if strings.EqualFold(in.Name, n) {
			return FocusTarget{Kind: FocusInstance, Name: in.Name, Class: in.Class, Retired: in.EntryStatus != ""}, true, class != nil
		}
	}
	if class != nil {
		return FocusTarget{Kind: FocusClass, Name: class.Name, Retired: class.EntryStatus != ""}, true, false
	}
	return FocusTarget{}, false, false
}

// liveConcepts is concepts without the retired ones, as a new slice.
func liveConcepts(concepts []OntologyConcept) []OntologyConcept {
	out := make([]OntologyConcept, 0, len(concepts))
	for _, c := range concepts {
		if c.EntryStatus == "" {
			out = append(out, c)
		}
	}
	return out
}

// CheckSchemeReferences adds the advisories only the whole document can give: a
// @projects class that does not exist, and a @focus that names no class or instance.
// It edits the schemes in place and acts on nothing — a name that resolves to nothing
// is reported and skipped, as every other directive is (fail-open).
func CheckSchemeReferences(schemes []OntologyScheme, classes []OntologyTerm, instances []OntologyInstance) {
	known := make(map[string]bool, len(classes))
	retired := map[string]bool{}
	for _, c := range classes {
		known[strings.ToLower(c.Name)] = true
		if c.EntryStatus != "" {
			retired[strings.ToLower(c.Name)] = true
		}
	}
	for i := range schemes {
		s := &schemes[i]
		for _, p := range s.Projects {
			switch {
			case !known[strings.ToLower(p)]:
				s.Issues = append(s.Issues, "@projects "+strconv.Quote(p)+" is not a class of this ontology")
			case retired[strings.ToLower(p)]:
				s.Issues = append(s.Issues, "@projects "+strconv.Quote(p)+
					" names a retired class; what is already placed stays, nothing new of it is")
			}
		}
		for j := range s.Concepts {
			c := &s.Concepts[j]
			if c.Focus == "" {
				continue
			}
			t, ok, both := ResolveFocus(c.Focus, classes, instances)
			switch {
			case !ok:
				c.Issues = append(c.Issues, "@focus "+strconv.Quote(c.Focus)+
					" is not a class or an instance of this ontology; no focus is written")
			case both:
				c.Issues = append(c.Issues, "@focus "+strconv.Quote(c.Focus)+
					" names both a class and an instance; the instance is used")
			}
			if ok && t.Retired {
				c.Issues = append(c.Issues, "@focus "+strconv.Quote(c.Focus)+
					" names a retired entry; no new focus statement is written")
			}
		}
	}
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
	segs = append(segs, MirrorSegment(scheme))
	for _, p := range path {
		segs = append(segs, MirrorSegment(p))
	}
	return strings.Join(segs, "/")
}

// MirrorSegment lowercases a title and joins its words with '-'. A '/' inside a title
// becomes '-' too: it is the tag hierarchy separator, and a title containing one would
// otherwise fabricate a level of nesting nobody declared.
func MirrorSegment(s string) string {
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
		if !s.Render || s.EntryStatus != "" {
			continue
		}
		// A retired concept is closed to new placements, so the prompt does not offer it.
		// A scheme with none retired is appended as it is (byte-identical render).
		if live := liveConcepts(s.Concepts); len(live) != len(s.Concepts) {
			s.Concepts = live
		}
		if len(s.Concepts) > 0 {
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
			if depth > SchemeMaxDepth {
				depth = SchemeMaxDepth
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
