package memory

// ontology.go — the layered entity-tier ontology (RFC BL P4c PR 3).
//
// The effective ontology is `base seed ⊕ tenant layer`, and the tenant layer only
// counts once the operator has CONFIRMED it. Until then a tenant runs on the seed
// alone, which means provisioning the document changes nothing on its own — the
// operator's flip is what activates it.
//
// That document-level gate replaced a per-term propose/approve queue during
// design, and the reason is worth keeping: a queue accumulates `pending` terms and
// the effective ontology silently stops growing, so the failure mode is invisible.
// A single draft→confirmed flip cannot rot that way — before it, nothing changed;
// after it, everything the operator wrote applies.

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

//go:embed templates/ontology.md
var ontologyTemplate string

// OntologyTemplate returns the import_md-shaped Markdown used to seed a tenant's
// ontology document. Compile-time constant, so provisioning is deterministic.
//
// It ships with THREE worked examples rather than a blank page: an operator
// editing `project`, `incident` and `constraint` into their own vocabulary starts
// from something that already reads correctly, and the difference between a
// feature people fill in and one they leave empty is usually whether the first
// screen shows them the shape.
func OntologyTemplate() string { return ontologyTemplate }

// OntologyPath is the canonical Path-tree location, in the TENANT scope, of the
// operator-editable ontology document.
const OntologyPath = "/memory/ontology"

// OntologyTitle MUST match the template's first heading — import_md makes the
// first heading the root chunk and the document title.
const OntologyTitle = "Tenant Ontology"

// OntologyConfirmedStatus is the root-chunk status that ACTIVATES the tenant
// layer. Any other value (including the provisioned default, `draft`) leaves the
// tenant running on the base seed alone.
const OntologyConfirmedStatus = "confirmed"

// OntologyDraftStatus is what a freshly provisioned document carries.
const OntologyDraftStatus = "draft"

// OntologyTerm is one type in the ontology: a name plus the fields an instance of
// it carries.
type OntologyTerm struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
	// Source is "base" for a seed term and "tenant" for one the operator added or
	// overrode. Reported so a reader can tell which half of the layering they are
	// looking at without diffing against the seed.
	Source string `json:"source"`
	// Inherited holds the field names this type gets from its ancestors, nearest
	// ancestor last, with anything the type redeclares removed (most specific wins,
	// the same rule as the tenant-over-seed layer).
	//
	// SEPARATE from Fields rather than merged into it, because two readers want
	// different things: the prompt wants the complete set, and the operator panel has
	// to show which fields a subclass actually declared. Merging would make the
	// panel's "this deployment defines" column claim fields nobody wrote there.
	Inherited []string `json:"inherited,omitempty"`
	// NameIssue is an operator-facing advisory about the name itself — spaces, capitals,
	// odd characters. Advisory only: the type is fully in force. Per-TERM rather than a
	// document-level note so the panel can point at the offender instead of asking the
	// operator to find it.
	NameIssue string `json:"name_issue,omitempty"`
	// Parent is the NAME of the entity this one subclasses, or "" for a root
	// (RFC BZ). A name rather than a chunk id because the ontology is consumed by
	// name everywhere downstream — the prompt, the stored fact's type, the retrieval
	// filter — and an id would have to be resolved back to a name at each of them.
	//
	// Seed terms are always roots: the seed lives in Go and has no chunks to nest
	// beneath, so subclassing a standard type means overriding it as a root chunk and
	// nesting under your own copy (RFC BZ §10.1).
	Parent string `json:"parent,omitempty"`
}

// BaseSeedOntology is the ontology every tenant starts with: POLE+O — person,
// object, location, event, organization — plus the two the memory tier itself
// needs, `preference` and `fact`.
//
// POLE+O is borrowed rather than invented. It is the schema intelligence analysts
// have used for decades because those five cover most of what one says about the
// world, and a tenant that never edits this file still gets something usable.
//
// Returned as a fresh slice each call: a shared package-level slice would let one
// caller's append or sort reorder every other caller's ontology.
func BaseSeedOntology() []OntologyTerm {
	return []OntologyTerm{
		{Name: "person", Fields: []string{"name", "role", "aliases"}, Source: "base"},
		{Name: "object", Fields: []string{"name", "kind", "identifier"}, Source: "base"},
		{Name: "location", Fields: []string{"name", "kind", "region"}, Source: "base"},
		{Name: "event", Fields: []string{"name", "occurred_at", "participants"}, Source: "base"},
		{Name: "organization", Fields: []string{"name", "kind", "domain"}, Source: "base"},
		// The memory tier's own two. `preference` carries a confidence because a
		// preference is inferred more often than stated; `fact` is the subject /
		// predicate / object triple the natural key for a fact is built from.
		{Name: "preference", Fields: []string{"category", "statement", "context", "confidence"}, Source: "base"},
		{Name: "fact", Fields: []string{"subject", "predicate", "object"}, Source: "base"},
	}
}

// EffectiveOntology layers a tenant's terms over the base seed.
//
// tenantConfirmed is the gate: when false the tenant's terms are DISCARDED and the
// result is the seed alone. That is the whole point of the draft state — a
// half-written ontology must not steer extraction just because it exists on disk.
//
// A tenant term with a seed term's name OVERRIDES it (same name, tenant fields),
// rather than merging field lists. Merging would produce a type the operator never
// wrote and could not remove a field from; overriding means what the document says
// is what applies.
//
// The result is sorted by name so materialization and prompt rendering are stable
// — an ontology that reordered between runs would churn the system prompt and cost
// a provider prompt-cache hit on every call.
func EffectiveOntology(tenantTerms []OntologyTerm, tenantConfirmed bool) []OntologyTerm {
	byName := make(map[string]OntologyTerm, len(tenantTerms)+8)
	for _, t := range BaseSeedOntology() {
		byName[t.Name] = t
	}
	if tenantConfirmed {
		for _, t := range tenantTerms {
			if t.Name == "" {
				continue
			}
			t.Source = "tenant"
			byName[t.Name] = t
		}
	}
	out := make([]OntologyTerm, 0, len(byName))
	for _, t := range byName {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	// Pinned roots are re-rooted BEFORE inheritance, so a wrongly-nested tier type does
	// not also inherit a domain type's fields on its way through.
	out, _ = EnforcePinnedRoots(out)
	// AFTER layering, so a subclass inherits what its parent effectively HAS — if the
	// parent overrode a standard type, the child gets the override, not the seed.
	return ResolveInheritance(out)
}

// FieldsJSON renders a term's fields for the chunk_types row. define_type stores
// fields as an opaque JSON string, so the shape here is what a later reader gets.
func (t OntologyTerm) FieldsJSON() string {
	if len(t.Fields) == 0 {
		return "[]"
	}
	b, err := json.Marshal(t.Fields)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// RenderOntology composes the {{memory:ontology}} body: the effective terms as a
// compact list an extractor can apply.
//
// UNFRAMED at the injection site (see trustedVariants) because this is a schema
// the model is meant to USE, not accumulated content it should distrust — the same
// reasoning as the consolidation bands. What makes that safe is that every term
// name and field reaching here has passed through the ontology document, which is
// tenant-operator-authored and confirmed by hand.
//
// HOUSE RULE: model-visible text — no internal RFC citations.
func RenderOntology(terms []OntologyTerm, tenantConfirmed bool) string {
	if len(terms) == 0 {
		return ""
	}
	b := "# Entity types\n\n" +
		"When you record an entity or a fact, use one of these types and fill the fields it lists.\n\n"
	// A HIERARCHY CHANGES THE PROMPT; a flat ontology does not. Rendered flat, the
	// output below is byte-identical to what it was before subclasses existed, so a
	// deployment that never nests sees no prompt drift, no cache invalidation, and no
	// need to re-baseline its extraction results.
	if !hasHierarchy(terms) {
		for _, t := range terms {
			b += "- **" + t.Name + "**"
			if len(t.Fields) > 0 {
				b += " — " + joinComma(t.Fields)
			}
			b += "\n"
		}
	} else {
		b += renderOntologyTree(terms)
		// Without this sentence the subclasses go unused. Handed a ladder, a model
		// tends to sit at the top of it and answer `event` where `incident` was
		// available — a capability with no caller, which is the failure this
		// subsystem has already shipped twice.
		b += "\nA type indented under another is a more specific kind of it and lists " +
			"every field it inherits. Use the MOST SPECIFIC type that fits what you are " +
			"recording; fall back to the general one only when no subtype applies.\n"
	}
	if !tenantConfirmed {
		// Said plainly because the difference is invisible otherwise: an operator
		// who edited the document and did not confirm it would see their terms
		// missing here with no explanation.
		b += "\nThese are the standard types. This deployment has not confirmed any " +
			"additions of its own, so use these as they are.\n"
	}
	return b
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// PinnedOntologyRoots are the memory tier's own structural types, which a tenant may
// not re-parent (RFC BZ §10.3, decided here rather than left to discovery).
//
// They may still be SUBCLASSED — `dietary-preference` under `preference` is a good idea
// and stays legal. What is refused is giving them a parent, because that inverts the
// tier: make `preference` a subclass of some domain type and, with subtype-expanded
// retrieval, a query for that domain type starts sweeping in every preference the user
// ever expressed. The tier stops meaning what every other surface assumes it means.
//
// Enforced by clearing the parent rather than by rejecting the document, so an operator
// who nests one by accident loses the nesting and keeps their ontology. The panel says
// what happened; silence would leave them believing it took.
func PinnedOntologyRoots() []string { return []string{"preference", "fact"} }

// EnforcePinnedRoots clears the parent of any pinned root and reports which were
// re-rooted, so the caller can tell the operator rather than let them discover it.
func EnforcePinnedRoots(terms []OntologyTerm) ([]OntologyTerm, []string) {
	pinned := map[string]bool{}
	for _, n := range PinnedOntologyRoots() {
		pinned[n] = true
	}
	var rerooted []string
	out := make([]OntologyTerm, 0, len(terms))
	for _, t := range terms {
		if t.Parent != "" && pinned[strings.ToLower(t.Name)] {
			rerooted = append(rerooted, t.Name)
			t.Parent = ""
		}
		out = append(out, t)
	}
	return out, rerooted
}

// ontologyNameRe is the identifier shape an entity name should take.
var ontologyNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$`)

// OntologyNameIssue returns an operator-facing warning when a name is awkward as a type,
// or "" when it is fine (RFC BZ §10.2).
//
// WARN, NEVER NORMALISE, and never reject. Any free-text heading is a legal type name
// today, so rewriting or refusing one would silently change — or break — an ontology that
// already works, and a name is a fact's natural key: normalising `Notes on naming` into
// `notes-on-naming` after facts were stored under the old spelling splits the type in
// half. The cost of a bad name is friction in a prompt, not corruption, so the right
// response is to point at it.
//
// Deliberately not applied to the seed: its names are already in this shape, and a
// warning on a name the operator cannot edit is noise.
func OntologyNameIssue(name string) string {
	n := strings.TrimSpace(name)
	if n == "" || ontologyNameRe.MatchString(n) {
		return ""
	}
	switch {
	case strings.ContainsAny(n, " \t"):
		return "contains spaces — a type name is used inside a prompt and as part of a " +
			"fact's key, where a phrase reads as prose rather than as a type. Prefer " +
			"lowercase words joined by - or _."
	case n != strings.ToLower(n):
		return "has capitals — type names are matched case-insensitively, so two spellings " +
			"of the same type are easy to create by accident. Prefer lowercase."
	default:
		return "has characters outside a-z, 0-9, - and _, which are awkward in a prompt " +
			"and in a fact's key."
	}
}

// ResolveInheritance fills each term's Inherited from its ancestors.
//
// A subclass inherits its parent's fields and adds its own: `incident` under `event`
// carries `occurred_at` without restating it. An inherited field cannot be REMOVED,
// and that is correct rather than a limitation — a subclass lacking its parent's field
// is not a subclass, and an operator who wants a type without those fields wants a
// sibling. (Removal is available on the other axis: a tenant term REPLACES a same-named
// seed term wholesale.)
//
// Bounded against a cycle even though the chunk tree cannot produce one. This runs on
// every prompt render, so an unbounded walk over caller-supplied terms would turn a
// malformed input into a hung request rather than a wrong answer.
func ResolveInheritance(terms []OntologyTerm) []OntologyTerm {
	byName := make(map[string]OntologyTerm, len(terms))
	for _, t := range terms {
		byName[t.Name] = t
	}
	memo := make(map[string][]string, len(terms))
	var resolve func(name string, seen map[string]bool) []string
	resolve = func(name string, seen map[string]bool) []string {
		if got, ok := memo[name]; ok {
			return got
		}
		t, ok := byName[name]
		// A dangling parent yields nothing rather than an error: the name came from a
		// chunk title, and a typo should cost the operator their inheritance, not the
		// whole ontology.
		if !ok || t.Parent == "" || seen[name] {
			return nil
		}
		seen[name] = true
		parent, ok := byName[t.Parent]
		if !ok {
			return nil
		}
		// Ancestor-first: the general fields (a name, a timestamp) read ahead of the
		// specific ones, which is also the order a model fills them in.
		chain := append(append([]string{}, resolve(t.Parent, seen)...), parent.Fields...)
		declared := make(map[string]bool, len(t.Fields))
		for _, f := range t.Fields {
			declared[f] = true
		}
		out := make([]string, 0, len(chain))
		emitted := make(map[string]bool, len(chain))
		for _, f := range chain {
			if declared[f] || emitted[f] {
				continue
			}
			emitted[f] = true
			out = append(out, f)
		}
		memo[name] = out
		return out
	}
	res := make([]OntologyTerm, 0, len(terms))
	for _, t := range terms {
		t.Inherited = resolve(t.Name, map[string]bool{})
		res = append(res, t)
	}
	return res
}

// AllFields is a term's complete field set as a consumer should see it: inherited
// first, then declared.
func AllFields(t OntologyTerm) []string {
	if len(t.Inherited) == 0 {
		return t.Fields
	}
	return append(append([]string{}, t.Inherited...), t.Fields...)
}

// hasHierarchy reports whether any term names a parent that is actually present.
//
// A DANGLING parent does not count. Otherwise a single typo'd chunk title would switch
// every deployment to the tree rendering and add the specificity instruction with no
// hierarchy to apply it to.
func hasHierarchy(terms []OntologyTerm) bool {
	present := make(map[string]bool, len(terms))
	for _, t := range terms {
		present[t.Name] = true
	}
	for _, t := range terms {
		if t.Parent != "" && present[t.Parent] {
			return true
		}
	}
	return false
}

// renderOntologyTree emits the terms as an indented tree, each line carrying the
// type's COMPLETE field set.
//
// Complete rather than declared-only, and the duplication is deliberate: a model that
// has to infer "incident also has occurred_at" from indentation gets it wrong often
// enough to matter, and the extra tokens are bounded by the depth cap. The indentation
// carries specificity; the field list carries what to fill in.
//
// Orphans — a term whose named parent is absent — are rendered at the root rather than
// dropped. A type missing from the prompt is a type the model cannot use, which is the
// silent-loss failure this whole RFC exists to end.
func renderOntologyTree(terms []OntologyTerm) string {
	present := make(map[string]bool, len(terms))
	for _, t := range terms {
		present[t.Name] = true
	}
	kids := make(map[string][]OntologyTerm, len(terms))
	for _, t := range terms {
		parent := t.Parent
		if parent != "" && !present[parent] {
			parent = ""
		}
		kids[parent] = append(kids[parent], t)
	}
	for k := range kids {
		sort.Slice(kids[k], func(i, j int) bool { return kids[k][i].Name < kids[k][j].Name })
	}
	var b strings.Builder
	// Bounded by the same cap the reader enforces, so a cycle reaching this far cannot
	// spin: the renderer is on the prompt path and a hang here is an outage.
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		if depth > OntologyMaxDepth {
			return
		}
		for _, t := range kids[parent] {
			b.WriteString(strings.Repeat("  ", depth))
			b.WriteString("- **" + t.Name + "**")
			if all := AllFields(t); len(all) > 0 {
				b.WriteString(" — " + joinComma(all))
			}
			b.WriteString("\n")
			walk(t.Name, depth+1)
		}
	}
	walk("", 0)
	return b.String()
}

// OntologyMaxDepth bounds how deep a subclass chain may go — ONE definition, shared by
// the chunk reader that builds the tree and the renderer that emits it. Two constants
// with the same value in two packages is how a cap starts disagreeing with itself.
//
// The rendered tree goes into every extraction prompt, so depth costs tokens on every
// call, and a taxonomy deeper than this is usually modelling confusion rather than
// precision. Nothing deeper is DISCARDED: the reader flattens it to the cap.
const OntologyMaxDepth = 4

// TrimOntologyName normalises a heading line or a chunk title into an entity name.
//
// ONE definition, shared by the chunk-tree reader (where the name comes from a title)
// and the markdown parser (where it comes from a heading), so the same entity cannot end
// up with two spellings depending on which path read it — the name is a fact's natural
// key and a retrieval filter, so a stray "#" would split a type in half.
func TrimOntologyName(s string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#"))
}

// FirstHeadingName returns the name from the first Markdown heading in a body, or "".
//
// The fallback for a chunk with no title: an operator who typed "## incident" into a body
// through update_chunk named their entity, and dropping it for want of a title column
// would be the same silent loss RFC BZ exists to remove. Any heading level is accepted
// because the level a chunk renders at depends on its depth, not on the author.
func FirstHeadingName(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		// Require the space: "#tag" is not a heading.
		if i := strings.IndexByte(t, ' '); i > 0 && strings.Trim(t[:i], "#") == "" {
			if n := TrimOntologyName(t); n != "" {
				return n
			}
		}
	}
	return ""
}

// ParseOntologyFields extracts the field names from ONE entity's body.
//
// Exported because RFC BZ reads the chunk tree while the legacy markdown path still
// exists as a narrow fallback (§2.3), and both must agree on what a field is. Two
// readers deciding that independently is how they drift — the same failure the
// embedding predicate had when the writer and the sweep disagreed.
//
// A field is the FIRST backticked token on a `- ` bullet. Prose after it is the
// operator's description and is deliberately ignored, so a field can be documented
// inline without the documentation becoming part of the name.
func ParseOntologyFields(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		if f := firstBackticked(t); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ParseOntologyMarkdown extracts terms from the ontology document's Markdown.
//
// It reads the format the TEMPLATE writes and an operator edits: a `## name`
// heading per term, and field names as the first backticked token on each bullet
// beneath it.
//
//	## incident
//	- `summary` — one sentence on what happened
//	- `cause` — what turned out to be responsible
//
// Parsing the Markdown rather than a structured `fields` map is deliberate: this is
// hand-edited operator content, and the template's own form is prose with
// backticks. Requiring JSON would mean the document an operator is invited to edit
// and the document the runtime can read are different documents.
//
// Deliberately lenient. A heading with no bullets is a term with no fields, a
// bullet with no backticks is skipped, and unparseable prose is ignored rather than
// failing the read — an ontology that dropped a term over punctuation would be
// worse than one that occasionally misses a field list. The `#` title heading and
// any `---` trailer are skipped.
func ParseOntologyMarkdown(md string) []OntologyTerm {
	var out []OntologyTerm
	var cur *OntologyTerm
	flush := func() {
		if cur != nil && cur.Name != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "## "):
			flush()
			name := TrimOntologyName(strings.TrimPrefix(t, "## "))
			cur = &OntologyTerm{Name: name, Source: "tenant"}
		case strings.HasPrefix(t, "# "):
			// The document title, not a term.
			flush()
		case cur != nil && strings.HasPrefix(t, "- "):
			if f := firstBackticked(t); f != "" {
				cur.Fields = append(cur.Fields, f)
			}
		}
	}
	flush()
	return out
}

// firstBackticked returns the first `backticked` token in s, or "".
func firstBackticked(s string) string {
	i := strings.Index(s, "`")
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	j := strings.Index(rest, "`")
	if j <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}
