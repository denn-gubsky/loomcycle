package memory

// ontology_scheme_proposal.go — a WHOLE concept scheme proposed at once (RFC GM P3.1,
// P1.4): its header and its concept tree as structured data. Pure — no I/O.
//
// THE DIRECTIVES ARE DATA UNTIL ACCEPTED (RFC GQ D12). A proposal's text can come from
// an untrusted document or a model, and the ontology reader acts on any `@name` line in a
// chunk body. So the header (@domain, @projects, @origin, @source, @version) and each
// concept's @notation, @alt and @exact_match travel as the fields below, never as body
// lines; the runtime writes them as directives only when an operator accepts, from these
// fields, after Check has passed again. Nothing else can become a directive that way:
// @acl, @retention, @render, @focus and @ordered are the operator's to add by editing.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SchemeProposalMaxConcepts bounds one whole-scheme proposal. A scheme larger than this
// belongs in a document of its own (RFC GM D16 (c), not built): every concept of a scheme
// in the ontology document is a chunk the reader walks on each uncached read.
const SchemeProposalMaxConcepts = 500

// ErrSchemeTooLarge is wrapped by the refusal of a scheme over SchemeProposalMaxConcepts.
var ErrSchemeTooLarge = errors.New("too many concepts")

// Limits on one proposal's text. A name is a chunk title and a mirror-tag segment; the
// others are single directive lines or a one-line meaning.
const (
	schemeNameMax     = 200
	schemeMeaningMax  = 1000
	schemeNotationMax = 64
	schemeValueMax    = 512
	schemeAltMax      = 20
	schemeProjectsMax = 20
)

// SchemeHeader is a proposed scheme's header (RFC GM D14).
type SchemeHeader struct {
	Domain   string   `json:"domain,omitempty"`
	Projects []string `json:"projects,omitempty"`
	Origin   string   `json:"origin,omitempty"`
	Source   string   `json:"source,omitempty"`
	Version  string   `json:"version,omitempty"`
}

// ProposedConcept is one concept of a proposed scheme, with the narrower concepts
// beneath it.
type ProposedConcept struct {
	Name       string            `json:"name"`
	Meaning    string            `json:"meaning,omitempty"`
	Notation   string            `json:"notation,omitempty"`
	Alt        []string          `json:"alt,omitempty"`
	ExactMatch string            `json:"exact_match,omitempty"`
	Children   []ProposedConcept `json:"children,omitempty"`
}

// SchemeProposal is a whole scheme: its name, its meaning, its header and its concepts.
type SchemeProposal struct {
	Name     string            `json:"name"`
	Meaning  string            `json:"meaning,omitempty"`
	Header   SchemeHeader      `json:"header"`
	Concepts []ProposedConcept `json:"concepts"`
}

// ProposedSchemeView is what a reviewer is shown of a scheme proposal beside its name:
// the header it would put in force, and the concept tree by name (its meaning is the
// proposal's body). Built from the stored proposal, so it is what accepting would write.
type ProposedSchemeView struct {
	Header   SchemeHeader  `json:"header"`
	Concepts int           `json:"concepts"`
	Outline  []OutlineNode `json:"outline,omitempty"`
}

// OutlineNode is one concept of a proposed scheme's outline: its name, its level below
// the scheme (1 = top), and the directives accepting writes for it, so a reviewer sees
// them before they are in force.
type OutlineNode struct {
	Name       string   `json:"name"`
	Level      int      `json:"level"`
	Notation   string   `json:"notation,omitempty"`
	Alt        []string `json:"alt,omitempty"`
	ExactMatch string   `json:"exact_match,omitempty"`
}

// Check normalises a proposal in place and reports what makes it unusable (err) and
// what it changed (warnings). It is run when a proposal is filed and again when it is
// accepted, so a stored proposal written some other way is held to the same rules.
//
// Normalising: every value is trimmed and folded onto one line (a newline in a value
// would start a second directive line); a notation or exact match holding a space is
// dropped with a warning, since the reader keeps only its first word; concepts nested
// deeper than SchemeMaxDepth are lifted onto the deepest level, as the reader flattens
// them (P1.2). Refused: an empty or over-long name, an @origin that is not one, two
// concepts with the same tag, more than SchemeProposalMaxConcepts concepts.
func (p *SchemeProposal) Check() (warnings []string, err error) {
	p.Name = oneLine(p.Name)
	if err := checkName("the scheme", p.Name); err != nil {
		return nil, err
	}
	p.Meaning = clip(oneLine(p.Meaning), schemeMeaningMax, "the scheme's meaning", &warnings)
	if err := p.Header.check(&warnings); err != nil {
		return nil, err
	}
	if len(p.Concepts) == 0 {
		return nil, fmt.Errorf("a scheme needs at least one concept")
	}
	capped := false
	p.Concepts = flattenConcepts(p.Concepts, 1, &capped)
	if capped {
		warnings = append(warnings, SchemeDepthIssue())
	}
	n := countConcepts(p.Concepts)
	if n > SchemeProposalMaxConcepts {
		return nil, fmt.Errorf("%w: the scheme has %d concepts, over the limit of %d per proposal",
			ErrSchemeTooLarge, n, SchemeProposalMaxConcepts)
	}
	seen := map[string]bool{}
	var walk func(cs []ProposedConcept, path []string) error
	walk = func(cs []ProposedConcept, path []string) error {
		for i := range cs {
			c := &cs[i]
			if err := c.check(&warnings); err != nil {
				return err
			}
			full := append(append([]string{}, path...), c.Name)
			tag := MirrorTag(p.Name, full)
			if seen[tag] {
				return fmt.Errorf("two concepts would share the tag %s; rename one", tag)
			}
			seen[tag] = true
			if err := walk(c.Children, full); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(p.Concepts, nil); err != nil {
		return nil, err
	}
	return warnings, nil
}

func (h *SchemeHeader) check(warnings *[]string) error {
	h.Domain = clip(oneLine(h.Domain), schemeValueMax, "@domain", warnings)
	h.Source = clip(oneLine(h.Source), schemeValueMax, "@source", warnings)
	h.Version = clip(oneLine(h.Version), schemeNotationMax, "@version", warnings)
	h.Origin = strings.ToLower(oneLine(h.Origin))
	switch h.Origin {
	case "", SchemeOriginAdopted, SchemeOriginDeclared, SchemeOriginGenerated:
	default:
		return fmt.Errorf("@origin %s is not an origin (want %s, %s or %s)", strconv.Quote(h.Origin),
			SchemeOriginAdopted, SchemeOriginDeclared, SchemeOriginGenerated)
	}
	var projects []string
	for _, p := range h.Projects {
		// A comma separates class names in the directive, so one inside a name would
		// split it in two.
		p = oneLine(strings.ReplaceAll(p, ",", " "))
		if p != "" {
			projects = append(projects, p)
		}
	}
	if len(projects) > schemeProjectsMax {
		return fmt.Errorf("@projects names %d classes, over the limit of %d", len(projects), schemeProjectsMax)
	}
	h.Projects = projects
	return nil
}

func (c *ProposedConcept) check(warnings *[]string) error {
	c.Name = oneLine(c.Name)
	if err := checkName("a concept", c.Name); err != nil {
		return err
	}
	c.Meaning = clip(oneLine(c.Meaning), schemeMeaningMax, c.Name+": meaning", warnings)
	c.Notation = singleToken(oneLine(c.Notation), schemeNotationMax, c.Name+": notation", warnings)
	c.ExactMatch = singleToken(oneLine(c.ExactMatch), schemeValueMax, c.Name+": exact match", warnings)
	var alt []string
	seen := map[string]bool{strings.ToLower(c.Name): true}
	for _, a := range c.Alt {
		a = clip(oneLine(a), schemeNameMax, c.Name+": alternative label", warnings)
		if a == "" || seen[strings.ToLower(a)] {
			continue
		}
		seen[strings.ToLower(a)] = true
		alt = append(alt, a)
	}
	if len(alt) > schemeAltMax {
		*warnings = append(*warnings, fmt.Sprintf("%s: %d alternative labels, only the first %d are kept",
			c.Name, len(alt), schemeAltMax))
		alt = alt[:schemeAltMax]
	}
	c.Alt = alt
	return nil
}

// Directives are the header lines accepting writes into the scheme's body, in a fixed
// order. Only these five: what a proposal can put in force is exactly this list.
func (h SchemeHeader) Directives() []string {
	var out []string
	add := func(name, v string) {
		if v != "" {
			out = append(out, "@"+name+" "+v)
		}
	}
	add("domain", h.Domain)
	if len(h.Projects) > 0 {
		out = append(out, "@projects "+strings.Join(h.Projects, ", "))
	}
	add("origin", h.Origin)
	add("source", h.Source)
	add("version", h.Version)
	return out
}

// Directives are the lines accepting writes into a concept's body.
func (c ProposedConcept) Directives() []string {
	var out []string
	if c.Notation != "" {
		out = append(out, "@notation "+c.Notation)
	}
	for _, a := range c.Alt {
		out = append(out, "@alt "+a)
	}
	if c.ExactMatch != "" {
		out = append(out, "@exact_match "+c.ExactMatch)
	}
	return out
}

// OutlineOf lists a concept tree by name and level, in document order.
func OutlineOf(cs []ProposedConcept) []OutlineNode {
	var out []OutlineNode
	var walk func(cs []ProposedConcept, level int)
	walk = func(cs []ProposedConcept, level int) {
		for _, c := range cs {
			out = append(out, OutlineNode{Name: c.Name, Level: level, Notation: c.Notation, Alt: c.Alt, ExactMatch: c.ExactMatch})
			walk(c.Children, level+1)
		}
	}
	walk(cs, 1)
	return out
}

// flattenConcepts lifts the children of a concept at SchemeMaxDepth onto its own level,
// beside it, as the reader does with a document nested too deep: nothing is dropped.
func flattenConcepts(cs []ProposedConcept, depth int, capped *bool) []ProposedConcept {
	out := make([]ProposedConcept, 0, len(cs))
	for _, c := range cs {
		if depth < SchemeMaxDepth {
			c.Children = flattenConcepts(c.Children, depth+1, capped)
			out = append(out, c)
			continue
		}
		kids := c.Children
		c.Children = nil
		out = append(out, c)
		if len(kids) > 0 {
			*capped = true
			out = append(out, flattenConcepts(kids, depth, capped)...)
		}
	}
	return out
}

func countConcepts(cs []ProposedConcept) int {
	n := len(cs)
	for _, c := range cs {
		n += countConcepts(c.Children)
	}
	return n
}

func checkName(what, name string) error {
	if TrimOntologyName(name) != name || name == "" {
		if name == "" {
			return fmt.Errorf("%s needs a name", what)
		}
		return fmt.Errorf("%s name %s may not start with #", what, strconv.Quote(name))
	}
	if utf8.RuneCountInString(name) > schemeNameMax {
		return fmt.Errorf("%s name is over %d characters: %s…", what, schemeNameMax, string([]rune(name)[:40]))
	}
	if MirrorSegment(name) == "" {
		return fmt.Errorf("%s name %s has no letters or digits for its tag", what, strconv.Quote(name))
	}
	return nil
}

// oneLine trims s and folds every run of white space (newlines included) into one space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, max int, what string, warnings *[]string) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	*warnings = append(*warnings, fmt.Sprintf("%s is over %d characters; cut", what, max))
	return string([]rune(s)[:max])
}

// singleToken is s when it is one word within max characters, else "" with a warning:
// the reader keeps a notation's or exact match's first word only, so a value with a
// space would be stored as something it is not.
func singleToken(s string, max int, what string, warnings *[]string) string {
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, " \t") || utf8.RuneCountInString(s) > max {
		*warnings = append(*warnings, fmt.Sprintf("%s %s is not one word of at most %d characters; dropped",
			what, strconv.Quote(s), max))
		return ""
	}
	return s
}
