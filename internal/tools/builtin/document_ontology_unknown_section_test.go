package builtin

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// unknownSectionOntologyMD carries a section of a kind this build does not know
// (`questions`, which RFC GQ adds later) between known sections, with nested entries,
// a field and directives that would each mean something if read as a class.
const unknownSectionOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### project\n- `name`\n\n" +
	"## Questions\n@section questions\n\n" +
	"### why-late\n- `answer` text\n@memory_scope tenant\n\n#### why-later\n\n" +
	"## Relations\n@section relations\n\n### cites\n\n" +
	"## incident\n- `summary`\n"

// TestOntology_UnknownSectionKindIsNotInForce: a heading whose first directive is
// `@section <kind>` with a kind this build does not know contributes nothing — not its
// heading, not its entries — to the classes, the relations, the sections list, the
// prompt or describe_ontology. A later build's section must not be read as classes by
// an older one.
func TestOntology_UnknownSectionKindIsNotInForce(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, unknownSectionOntologyMD, memrank.OntologyConfirmedStatus)
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("tenant ontology read failed")
	}
	outOfForce := []string{"Questions", "why-late", "why-later"}
	names := map[string]bool{}
	for _, tm := range read.Terms {
		names[tm.Name] = true
	}
	for _, n := range outOfForce {
		if names[n] {
			t.Errorf("%q under an unknown section was read as a class: %v", n, read.Terms)
		}
	}
	if !names["project"] || !names["incident"] || len(names) != 2 {
		t.Errorf("classes = %v, want exactly project and incident", names)
	}
	if strings.Join(read.Sections, ",") != "classes,relations" {
		t.Errorf("sections = %v, want the known ones only", read.Sections)
	}
	if len(read.Relations) != 1 || read.Relations[0].Name != "cites" {
		t.Errorf("relations = %#v", read.Relations)
	}
	for _, p := range read.Proposals {
		t.Errorf("an out-of-force entry was offered as a proposal: %#v", p)
	}
	if len(read.Issues) != 1 {
		t.Errorf("issues = %q, want exactly one", read.Issues)
	}

	rendered := memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true)
	for _, n := range outOfForce {
		if strings.Contains(rendered, n) {
			t.Errorf("%q reached the prompt:\n%s", n, rendered)
		}
	}

	desc := describeOntologyVia(t, d, ctx)
	for _, n := range outOfForce {
		if _, bad := describedClassByName(desc)[n]; bad {
			t.Errorf("describe_ontology lists %q as a class", n)
		}
		for _, r := range desc.Relations {
			if r.Name == n {
				t.Errorf("describe_ontology lists %q as a relation", n)
			}
		}
	}
	if strings.Join(desc.Sections, ",") != "classes,relations" || len(desc.Issues) != 1 {
		t.Errorf("describe_ontology sections = %v issues = %q", desc.Sections, desc.Issues)
	}
}

// TestOntology_UnknownSectionIssueNamesTheKind: the one issue names the heading and the
// unknown kind, so an operator can tell which section this version does not read.
func TestOntology_UnknownSectionIssueNamesTheKind(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, unknownSectionOntologyMD)
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatal(err)
	}
	want := `"Questions": unknown section kind "questions": its entries are not in force`
	if len(read.Issues) != 1 || read.Issues[0] != want {
		t.Errorf("issues = %q, want [%q]", read.Issues, want)
	}
}

// TestOntology_HeadingWithoutSectionIsStillAClass: the pre-sections format is unchanged
// — a root heading with no `@section` line is a class with its subclasses, also when
// its first directive is another one and also beside an unknown section.
func TestOntology_HeadingWithoutSectionIsStillAClass(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, "# Tenant Ontology\n\n"+
		"## event\n@memory_scope tenant\n@section classes\n\n### incident\n\n"+
		"## Questions\n@section questions\n\n### why\n\n"+
		"## project\n- `name`\n")
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	for _, tm := range read.Terms {
		parents[tm.Name] = tm.Parent
	}
	want := map[string]string{"event": "", "incident": "event", "project": ""}
	if fmt.Sprint(parents) != fmt.Sprint(want) {
		t.Errorf("classes = %v, want %v", parents, want)
	}
	if len(read.Sections) != 0 {
		t.Errorf("sections = %v, want none", read.Sections)
	}
}

// knownSectionsGolden is what sectionedOntologyMD reads as — recorded on the build
// before unknown section kinds were taken out of force, so any drift in how a document
// of known sections reads fails here.
const knownSectionsGolden = `sections: classes,relations,schemes,instances
issues: []
term Document < "" doc=true fields=[title identifier issued] specs=3
term NormativeAct < "Document" doc=true fields=[] specs=0
term Law < "NormativeAct" doc=true fields=[] specs=0
term Provision < "" doc=false fields=[anchor] specs=1
term incident < "" doc=false fields=[summary] specs=1
grammar law-ua class=NormativeAct
relation cites < "" inverse=cited_by
scheme topics concepts=0
instance Acme Bank a=organization
proposals: 0
prompt:
# Entity types

When you record an entity or a fact, use one of these types and fill the fields it lists.

- **Provision** — anchor
- **event** — name, occurred_at, participants
- **fact** — subject, predicate, object
- **incident** — summary
- **location** — name, kind, region
- **object** — name, kind, identifier
- **organization** — name, kind, domain
- **person** — name, role, aliases
- **preference** — category, statement, context, confidence
`

// TestOntology_KnownSectionsReadAsBefore: a document of known sections only reads
// byte-for-byte as it did before unknown kinds were taken out of force.
func TestOntology_KnownSectionsReadAsBefore(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, sectionedOntologyMD)
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sections: %s\nissues: %q\n", strings.Join(read.Sections, ","), read.Issues)
	for _, tm := range read.Terms {
		fmt.Fprintf(&b, "term %s < %q doc=%v fields=%v specs=%d\n", tm.Name, tm.Parent, tm.IsDocumentClass, tm.Fields, len(tm.FieldSpecs))
	}
	for _, g := range read.Grammars {
		fmt.Fprintf(&b, "grammar %s class=%s\n", g.Name, g.Class)
	}
	for _, r := range read.Relations {
		fmt.Fprintf(&b, "relation %s < %q inverse=%s\n", r.Name, r.Parent, r.Inverse)
	}
	for _, s := range read.Schemes {
		fmt.Fprintf(&b, "scheme %s concepts=%d\n", s.Name, len(s.Concepts))
	}
	for _, in := range read.Instances {
		fmt.Fprintf(&b, "instance %s a=%s\n", in.Name, in.Class)
	}
	fmt.Fprintf(&b, "proposals: %d\n", len(read.Proposals))
	keys := make([]string, 0, len(read.SectionChunks))
	for k := range read.SectionChunks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "classes,instances,relations,schemes" {
		t.Errorf("section chunks = %v", keys)
	}
	b.WriteString("prompt:\n" + memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true))
	if got := b.String(); got != knownSectionsGolden {
		t.Errorf("a document of known sections reads differently:\n--- got\n%s\n--- want\n%s", got, knownSectionsGolden)
	}
}
