package memory

// ontology_relations.go — the Relations section of the ontology document (RFC FO P1b).
//
// Each entry under `@section relations` is a relation KIND; a child entry is a
// SUB-KIND (amends ⊂ changes), so a filter or walk for the parent expands to its
// children the way a type filter expands to its subclasses. Directives say what a kind
// means: its inverse, the classes at either end, its characteristics, its qualifiers.
//
// A seed vocabulary ships here, borrowed from Dublin Core, ELI, PROV-O, W3C ORG, SKOS
// and LegalRuleML. It is INERT until a tenant's confirmed ontology declares a Relations
// section (even an empty one): a tenant that never does keeps free-text kinds and the
// behaviour it has today. Pure parsing and layering — no I/O.

import (
	"regexp"
	"sort"
	"strings"
)

// Relation sources: where an effective kind came from.
const (
	RelationSourceBase     = "base"     // the seed vocabulary
	RelationSourceTenant   = "tenant"   // declared or adopted in the tenant's document
	RelationSourceReserved = "reserved" // written only by the runtime
)

// RelationQualifier is one attribute a statement of a kind may carry, with its allowed
// values (empty = any value).
type RelationQualifier struct {
	Name   string   `json:"name"`
	Values []string `json:"values,omitempty"`
}

// OntologyRelation is one relation kind.
type OntologyRelation struct {
	Name string `json:"name"`
	// Parent is the kind this one is a sub-kind of, or "" for a root.
	Parent string `json:"parent,omitempty"`
	// Inverse is the name seen from the target end — derived at read, never stored. A
	// symmetric kind is its own inverse.
	Inverse string `json:"inverse,omitempty"`
	// Domain and Range are class names (or `entity` / `concept`) the two ends must be.
	// Empty means unconstrained. Checked closed-world under the subclass closure.
	Domain           []string            `json:"domain,omitempty"`
	Range            []string            `json:"range,omitempty"`
	Symmetric        bool                `json:"symmetric,omitempty"`
	Asymmetric       bool                `json:"asymmetric,omitempty"`
	Irreflexive      bool                `json:"irreflexive,omitempty"`
	Transitive       bool                `json:"transitive,omitempty"`
	TransitiveWithin string              `json:"transitive_within,omitempty"`
	Functional       bool                `json:"functional,omitempty"`
	Qualifiers       []RelationQualifier `json:"qualifiers,omitempty"`
	// Discriminator is the qualifier whose value makes two statements of one triple
	// distinct (two amendments of one provision are two statements).
	Discriminator string `json:"discriminator,omitempty"`
	Dated         bool   `json:"dated,omitempty"`
	Walk          bool   `json:"walk,omitempty"`
	AutoAssert    bool   `json:"auto_assert,omitempty"`
	// Meaning is one line of what the kind says — the first prose line of its entry.
	Meaning     string   `json:"meaning,omitempty"`
	RenamedFrom string   `json:"renamed_from,omitempty"`
	Source      string   `json:"source"`
	Issues      []string `json:"issues,omitempty"`
	NameIssue   string   `json:"name_issue,omitempty"`
	// ChunkID is the entry's chunk in the ontology document; "" for a seed or reserved
	// kind, which has none. Lets a proposal hang under a declared kind by name.
	ChunkID string `json:"chunk_id,omitempty"`
}

// relationDirectives are the directives a relation entry may carry. A class directive in
// a relation entry is reported, not silently accepted, because it would do nothing.
var relationDirectives = map[string]bool{
	"inverse": true, "domain": true, "range": true, "symmetric": true, "asymmetric": true,
	"irreflexive": true, "transitive": true, "transitive_within": true, "functional": true,
	"qualifiers": true, "discriminator": true, "dated": true, "walk": true,
	"auto_assert": true, "renamed_from": true,
}

// ParseRelationBody reads one relation entry's body. Unusable declarations become
// Issues and are dropped; the kind itself stays in force (fail-open, as for classes).
func ParseRelationBody(name, body string) OntologyRelation {
	r := OntologyRelation{Name: name, Source: RelationSourceTenant, NameIssue: RelationNameIssue(name)}
	for _, d := range ParseDirectives(body) {
		switch d.Name {
		case "inverse":
			r.Inverse = firstWord(d.Value)
		case "domain":
			r.Domain = splitClassList(d.Value)
		case "range":
			r.Range = splitClassList(d.Value)
		case "symmetric":
			r.Symmetric = true
		case "asymmetric":
			r.Asymmetric = true
		case "irreflexive":
			r.Irreflexive = true
		case "transitive":
			r.Transitive = true
		case "transitive_within":
			r.TransitiveWithin = firstWord(d.Value)
		case "functional":
			r.Functional = true
		case "qualifiers":
			r.Qualifiers = append(r.Qualifiers, parseQualifiers(d.Value)...)
		case "discriminator":
			r.Discriminator = firstWord(d.Value)
		case "dated":
			r.Dated = true
		case "walk":
			r.Walk = true
		case "auto_assert":
			r.AutoAssert = true
		case "renamed_from":
			r.RenamedFrom = firstWord(d.Value)
		default:
			if knownDirectives[d.Name] {
				r.Issues = append(r.Issues, "@"+d.Name+" is not a relation directive; ignored")
			} else {
				r.Issues = append(r.Issues, "unknown directive @"+d.Name+"; ignored")
			}
		}
	}
	r.Meaning = firstProseLine(body)
	r.Issues = append(r.Issues, normaliseRelation(&r)...)
	return r
}

// normaliseRelation drops declarations that contradict each other, reporting each.
func normaliseRelation(r *OntologyRelation) []string {
	var issues []string
	if r.Symmetric && r.Asymmetric {
		issues = append(issues, "both @symmetric and @asymmetric; neither is applied")
		r.Symmetric, r.Asymmetric = false, false
	}
	if r.Symmetric && r.Inverse != "" && r.Inverse != r.Name {
		issues = append(issues, "a @symmetric kind is its own inverse; @inverse "+r.Inverse+" is ignored")
		r.Inverse = ""
	}
	if r.Discriminator != "" && !hasQualifier(*r, r.Discriminator) {
		issues = append(issues, "@discriminator "+r.Discriminator+" names no declared qualifier; ignored")
		r.Discriminator = ""
	}
	if r.TransitiveWithin != "" && !hasQualifier(*r, r.TransitiveWithin) {
		issues = append(issues, "@transitive_within "+r.TransitiveWithin+" names no declared qualifier; ignored")
		r.TransitiveWithin = ""
	}
	return issues
}

func hasQualifier(r OntologyRelation, name string) bool {
	for _, q := range r.Qualifiers {
		if q.Name == name {
			return true
		}
	}
	return false
}

// QualifierNamed returns the kind's qualifier of that name.
func (r OntologyRelation) QualifierNamed(name string) (RelationQualifier, bool) {
	for _, q := range r.Qualifiers {
		if q.Name == name {
			return q, true
		}
	}
	return RelationQualifier{}, false
}

// splitClassList reads `Provision|Document` (commas accepted too).
func splitClassList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '|' || r == ',' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, firstWord(p))
		}
	}
	return out
}

// parseQualifiers reads `mod_type(substitution|insertion), effective_from`: commas
// separate qualifiers except inside parentheses, `|` (or `,`) separates values.
func parseQualifiers(s string) []RelationQualifier {
	var out []RelationQualifier
	depth, start := 0, 0
	emit := func(part string) {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		q := RelationQualifier{Name: part}
		if i := strings.Index(part, "("); i > 0 {
			q.Name = strings.TrimSpace(part[:i])
			inner := strings.TrimSuffix(strings.TrimSpace(part[i+1:]), ")")
			for _, v := range strings.FieldsFunc(inner, func(r rune) bool { return r == '|' || r == ',' }) {
				if v = strings.TrimSpace(v); v != "" {
					q.Values = append(q.Values, v)
				}
			}
		}
		out = append(out, q)
	}
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				emit(s[start:i])
				start = i + 1
			}
		}
	}
	emit(s[start:])
	return out
}

// firstProseLine is the first line of a body that is neither a directive, a bullet nor
// a heading — the kind's one-line meaning.
func firstProseLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "-") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "```") {
			continue
		}
		return t
	}
	return ""
}

var relationNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// RelationNameIssue is an advisory when a kind name is not lower_snake_case. Advisory
// only: a kind name is what statements are stored under, so it is never rewritten.
func RelationNameIssue(name string) string {
	n := strings.TrimSpace(name)
	if n == "" || relationNameRe.MatchString(n) {
		return ""
	}
	return "relation kinds read best as lower_snake_case (cites, based_on); statements are stored under the name exactly as written"
}

// reservedRelations are the kinds only the runtime writes, as fixed nodes. Their names
// must match the runtime's reserved-kind list (asserted by a test in tools/builtin).
// A tenant entry with one of these names only anchors sub-kinds beneath it.
func reservedRelations() []OntologyRelation {
	return []OntologyRelation{
		{Name: "about", Inverse: "described_by", Meaning: "a fact is about this entity"},
		{Name: "supersedes", Inverse: "superseded_by", Asymmetric: true, Walk: true, Meaning: "a correction retires the earlier record"},
		{Name: "references", Inverse: "referenced_by", Meaning: "a link written in the text"},
		{Name: "mentions", Inverse: "mentioned_in", Meaning: "the text names this thing"},
		{Name: "derived_from", Inverse: "source_of", Meaning: "a fact or summary was distilled from this"},
		{Name: "supported_by", Inverse: "supports", Meaning: "a claim's evidence"},
	}
}

// IsReservedRelation reports whether a kind is one only the runtime writes.
func IsReservedRelation(name string) bool {
	for _, r := range reservedRelations() {
		if r.Name == name {
			return true
		}
	}
	return false
}

// ReservedRelationNames lists the reserved kinds, sorted.
func ReservedRelationNames() []string {
	var out []string
	for _, r := range reservedRelations() {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// SeedRelations is the base relation vocabulary.
//
// NO SEED KIND DECLARES A DOMAIN OR RANGE. The vocabulary describes what each kind
// joins, but as constraints against classes a tenant never declared (an "act", a
// "post") it would refuse nearly every write. Constraints come only from kinds the
// tenant declares itself. Characteristics and qualifiers are kept: they are about the
// relation, not about the tenant's classes.
//
// Returned fresh each call so no caller can reorder another's vocabulary.
func SeedRelations() []OntologyRelation {
	s := func(name, parent, inverse, meaning string) OntologyRelation {
		return OntologyRelation{Name: name, Parent: parent, Inverse: inverse, Meaning: meaning, Source: RelationSourceBase}
	}
	sym := func(r OntologyRelation) OntologyRelation { r.Symmetric, r.Inverse = true, ""; return r }
	walk := func(r OntologyRelation) OntologyRelation { r.Walk = true; return r }
	q := func(r OntologyRelation, qs ...RelationQualifier) OntologyRelation { r.Qualifiers = qs; return r }

	amends := q(s("amends", "changes", "amended_by", "changes the text of an act or provision"),
		RelationQualifier{Name: "mod_type", Values: []string{"substitution", "insertion", "repeal", "renumbering", "split", "join"}},
		RelationQualifier{Name: "effective_from"})
	amends.Discriminator, amends.Dated = "mod_type", true
	stricter := q(s("stricter_than", "", "looser_than", "sets a stricter version of the same requirement"),
		RelationQualifier{Name: "aspect"})
	stricter.Asymmetric, stricter.TransitiveWithin = true, "aspect"
	overrides := walk(q(s("overrides", "", "overridden_by", "takes precedence over another provision"),
		RelationQualifier{Name: "principle", Values: []string{"superior", "specialis", "posterior"}}))
	overrides.Asymmetric = true
	conflicts := sym(s("conflicts_with", "", "", "cannot both be followed"))
	conflicts.Irreflexive = true
	exact := sym(s("exact_match", "", "", "the same concept in another scheme"))
	exact.Transitive = true
	subclass := s("subclass_of", "", "has_subclass", "a class is a kind of another class")
	subclass.Transitive = true
	partOf := s("part_of", "", "has_part", "a part of a whole (the document tree itself is not written as edges)")
	partOf.Asymmetric = true
	versionOf := s("version_of", "", "has_version", "an edition of a work")
	versionOf.Functional = true
	primaryTopic := s("primary_topic", "subject", "primary_topic_of", "the one concept this is chiefly about")
	primaryTopic.Functional = true
	classified := s("classified_as", "", "classifies", "the file-plan class this record is filed under")
	classified.Functional = true
	holds := s("holds", "", "held_by", "a person holds a post")
	holds.Dated = true

	return []OntologyRelation{
		// structural
		partOf,
		s("member_of", "", "has_member", "an item of a collection"),
		// citation
		walk(s("cites", "references", "cited_by", "refers to another act or provision")),
		walk(s("based_on", "references", "basis_for", "is issued on the authority of another act")),
		walk(s("defines", "", "defined_by", "gives the definition of a term")),
		s("requires", "references", "required_by", "needs a form or document to be carried out"),
		// lifecycle
		versionOf,
		s("revision_of", "derived_from", "revised_by", "a later edition of a prior edition"),
		s("changes", "", "changed_by", "alters another act or provision"),
		amends,
		s("repeals", "changes", "repealed_by", "ends the force of another act or provision"),
		s("commences", "changes", "commenced_by", "brings another act or provision into force"),
		s("replaces", "", "replaced_by", "supersedes a non-legislative document"),
		s("corrects", "", "corrected_by", "fixes an error in another document"),
		s("consolidates", "", "consolidated_by", "combines an act with its amendments"),
		// normative
		s("applies", "references", "applied_by", "puts another act or provision into practice"),
		walk(q(s("implements", "applies", "implemented_by", "carries out a higher norm"),
			RelationQualifier{Name: "coverage", Values: []string{"full", "partial"}})),
		s("transposes", "implements", "transposed_by", "brings a directive or standard into national law"),
		s("conforms_to", "", "conformed_to_by", "meets a standard"),
		walk(q(s("derogates_from", "", "derogated_by", "makes an exception to another provision"),
			RelationQualifier{Name: "scope"})),
		overrides,
		stricter,
		conflicts,
		// semantic
		sym(s("related", "", "", "is related, without saying how")),
		s("elaborates", "related", "elaborated_by", "gives detail to a general statement"),
		sym(s("alternate_of", "", "", "the same content in another form or language")),
		exact,
		sym(s("close_match", "", "", "nearly the same concept in another scheme")),
		// classification
		s("instance_of", "", "has_instance", "a thing is an instance of a class"),
		subclass,
		s("broader", "", "narrower", "a concept is narrower than another (the scheme tree)"),
		s("subject", "", "subject_of", "what a document or fact is about"),
		primaryTopic,
		classified,
		s("focus", "", "focus_of", "the entity a concept is about"),
		// organisational
		s("sub_organization_of", "", "has_sub_organization", "a unit within an organisation"),
		holds,
		s("post_in", "", "has_post", "a post exists in a unit"),
		s("reports_to", "", "manages", "a post reports to another post"),
		s("responsible_for", "", "responsibility_of", "a post or unit is responsible for a document or function"),
		s("issued_by", "", "issued", "the organisation that issued a document"),
		// provenance
		s("quoted_from", "derived_from", "quoted_in", "takes words verbatim from a source"),
		s("primary_source", "derived_from", "primary_source_of", "the original source of a claim"),
		s("attributed_to", "", "attributed", "the agent a claim or document is attributed to"),
		s("generated_by", "", "generated", "the run or event that produced this"),
		s("invalidated_by", "", "invalidated", "the event that ended this"),
	}
}

// EffectiveRelations is the relation vocabulary in force: nil unless the tenant's
// CONFIRMED ontology declares a Relations section, otherwise reserved ⊕ seed ⊕ tenant.
//
// A tenant kind replaces a same-named seed kind wholesale (as classes do), except that a
// root entry keeps the seed kind's parent (see below). A tenant
// entry with a RESERVED name keeps the reserved definition — the runtime writes those
// edges and its meaning is fixed — and only anchors the sub-kinds nested beneath it.
// Sorted by name so rendering and expansion are stable.
func EffectiveRelations(tenant []OntologyRelation, sectionDeclared, confirmed bool) []OntologyRelation {
	if !sectionDeclared || !confirmed {
		return nil
	}
	byName := map[string]OntologyRelation{}
	for _, r := range reservedRelations() {
		r.Source = RelationSourceReserved
		byName[r.Name] = r
	}
	for _, r := range SeedRelations() {
		byName[r.Name] = r
	}
	for _, r := range tenant {
		if r.Name == "" || IsReservedRelation(r.Name) {
			continue
		}
		// A ROOT entry that overrides a seed kind keeps the seed kind's parent. Adopting
		// files a copy at the section root when the tenant has no copy of its parent, and
		// a wholesale override would drop the parent with it — `changes` would stop
		// expanding to `amends`. Nesting the entry under another kind still sets a
		// different parent; the position is the operator's, the default is the seed's.
		if seed, ok := byName[r.Name]; ok && r.Parent == "" && seed.Source == RelationSourceBase {
			r.Parent = seed.Parent
		}
		r.Source = RelationSourceTenant
		byName[r.Name] = r
	}
	out := make([]OntologyRelation, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MarkReservedEntries returns the tenant layer with an issue on every entry that uses a
// reserved name, so the panel can say its directives were ignored. The entry itself is
// kept: it anchors the sub-kinds beneath it.
func MarkReservedEntries(tenant []OntologyRelation) []OntologyRelation {
	out := make([]OntologyRelation, 0, len(tenant))
	for _, r := range tenant {
		if IsReservedRelation(r.Name) {
			r.Issues = append(r.Issues, r.Name+" is written only by the runtime; its meaning is fixed and the "+
				"directives here are ignored — it can still group the kinds nested under it")
		}
		out = append(out, r)
	}
	return out
}

// ExpandDescendants returns names plus every descendant through parentOf (child →
// parent), breadth-first and bounded by maxDepth, and which names were ADDED. The
// requested names come first in their given order; added ones follow, sorted, so output
// is stable. One helper for type and kind expansion — two closure walks over two trees
// is how they start disagreeing.
func ExpandDescendants(names []string, parentOf map[string]string, maxDepth int) (expanded, added []string) {
	kids := map[string][]string{}
	for child, parent := range parentOf {
		if parent != "" {
			kids[parent] = append(kids[parent], child)
		}
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			expanded = append(expanded, n)
		}
	}
	frontier := append([]string{}, expanded...)
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, n := range frontier {
			for _, k := range kids[n] {
				if seen[k] {
					continue
				}
				seen[k] = true
				added = append(added, k)
				next = append(next, k)
			}
		}
		frontier = next
	}
	sort.Strings(added)
	return append(expanded, added...), added
}

// RenderRelations composes the extraction prompt's relation block: the TENANT's own
// kinds only (declared or adopted), never the seed or the reserved kinds — the prompt
// grows only by what the operator chose (RFC FO decision 10d.2). "" when the tenant
// declares none, so a deployment without its own kinds sees no prompt change.
//
// HOUSE RULE: model-visible text — no internal RFC citations.
func RenderRelations(effective []OntologyRelation) string {
	var tenant []OntologyRelation
	for _, r := range effective {
		if r.Source == RelationSourceTenant {
			tenant = append(tenant, r)
		}
	}
	if len(tenant) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Relation kinds\n\n" +
		"When you record how two things are related, name the relation with one of these kinds. " +
		"If none of them fits, describe the relation in the sentence instead of inventing a kind.\n\n")
	for _, r := range tenant {
		b.WriteString("- **" + r.Name + "**")
		if r.Symmetric {
			b.WriteString(" (either direction)")
		} else if r.Inverse != "" {
			b.WriteString(" (seen from the other end: " + r.Inverse + ")")
		}
		if len(r.Domain) > 0 || len(r.Range) > 0 {
			b.WriteString(" — " + orAny(r.Domain) + " → " + orAny(r.Range))
		}
		if r.Parent != "" {
			b.WriteString(" — a kind of " + r.Parent)
		}
		if r.Meaning != "" {
			b.WriteString(" — " + r.Meaning)
		}
		if len(r.Qualifiers) > 0 {
			var qs []string
			for _, q := range r.Qualifiers {
				if len(q.Values) > 0 {
					qs = append(qs, q.Name+" ("+strings.Join(q.Values, ", ")+")")
				} else {
					qs = append(qs, q.Name)
				}
			}
			b.WriteString(" — record " + strings.Join(qs, "; "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func orAny(classes []string) string {
	if len(classes) == 0 {
		return "anything"
	}
	return strings.Join(classes, " or ")
}
