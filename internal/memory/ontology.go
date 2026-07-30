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
	return out
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
	for _, t := range terms {
		b += "- **" + t.Name + "**"
		if len(t.Fields) > 0 {
			b += " — " + joinComma(t.Fields)
		}
		b += "\n"
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
			name := strings.TrimSpace(strings.TrimPrefix(t, "## "))
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
