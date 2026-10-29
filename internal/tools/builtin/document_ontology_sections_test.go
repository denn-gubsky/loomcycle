package builtin

import (
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// sectionedOntologyMD is the RFC FO §3.1 example, trimmed to what the classes reader
// has to get right: a classes section with a document-class tree, a grammar, a chunk
// class, and the three other section kinds the reader must keep out of the class list.
const sectionedOntologyMD = "# Tenant Ontology\n@version 4\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text @required\n- `identifier` text @identity\n- `issued` date\n\n" +
	"#### NormativeAct\n@identifier_pattern (?P<number>\\d+-[IVX]+)\n@citation_grammar law-ua\n\n" +
	"##### grammar:law-ua\npattern: Закон України № ?(?P<number>\\d+-[IVX]+)\nresolve: work_key=law:{number}\n\n" +
	"##### Law\n\n" +
	"### Provision\n@anchor_grammar akn-ua\n@identiy\n- `anchor` anchor @identity\n\n" +
	"## incident\n- `summary`\n\n" +
	"## Relations\n@section relations\n\n### cites\n@inverse cited_by\n\n" +
	"## Schemes\n@section schemes\n\n### topics\n\n" +
	"## Instances\n@section instances\n\n### Acme Bank\n@a organization\n"

// TestOntologyTree_ClassesSectionReadsClassesFromTheSection (RFC FO §3.1).
//
// The section chunk is not a class, its children are ROOT classes (depth counts from
// the section), a class outside any section still reads as a class, and the other
// section kinds contribute nothing to the class list.
func TestOntologyTree_ClassesSectionReadsClassesFromTheSection(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, sectionedOntologyMD)
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	by := map[string]memrank.OntologyTerm{}
	for _, tm := range read.Terms {
		by[tm.Name] = tm
	}
	for _, absent := range []string{"Classes", "Relations", "Schemes", "Instances", "cites", "topics", "Acme Bank", "grammar:law-ua"} {
		if _, bad := by[absent]; bad {
			t.Errorf("%q was read as a class", absent)
		}
	}
	want := map[string]string{"Document": "", "NormativeAct": "Document", "Law": "NormativeAct", "Provision": "", "incident": ""}
	if len(by) != len(want) {
		t.Errorf("classes = %v, want exactly %v", termNames(by), want)
	}
	for name, parent := range want {
		tm, ok := by[name]
		if !ok {
			t.Errorf("class %q missing", name)
			continue
		}
		if tm.Parent != parent {
			t.Errorf("%s.Parent = %q, want %q", name, tm.Parent, parent)
		}
	}
	for name, isDoc := range map[string]bool{"Document": true, "NormativeAct": true, "Law": true, "Provision": false, "incident": false} {
		if by[name].IsDocumentClass != isDoc {
			t.Errorf("%s.IsDocumentClass = %v, want %v", name, by[name].IsDocumentClass, isDoc)
		}
	}
	if by["NormativeAct"].IdentifierPattern == "" || by["NormativeAct"].CitationGrammar != "law-ua" {
		t.Errorf("NormativeAct directives not read: %#v", by["NormativeAct"])
	}
	if specs := by["Document"].FieldSpecs; len(specs) != 3 || !specs[0].Required || !specs[1].Identity || specs[2].Type != memrank.FieldDate {
		t.Errorf("Document field specs = %#v", specs)
	}
	if !strings.Contains(strings.Join(by["Provision"].Issues, " "), "@identiy") {
		t.Errorf("the typo'd directive was not reported: %v", by["Provision"].Issues)
	}
	if strings.Join(read.Sections, ",") != "classes,relations,schemes,instances" {
		t.Errorf("sections = %v", read.Sections)
	}
	if len(read.Grammars) != 1 || read.Grammars[0].Name != "law-ua" || read.Grammars[0].Class != "NormativeAct" || read.Grammars[0].Issue != "" {
		t.Errorf("grammars = %#v", read.Grammars)
	}
	if read.RootRevision < 1 {
		t.Errorf("root revision = %d, want ≥ 1", read.RootRevision)
	}
	// Document classes are in the read and out of the prompt.
	rendered := memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true)
	for _, docClass := range []string{"Document", "NormativeAct", "**Law**"} {
		if strings.Contains(rendered, docClass) {
			t.Errorf("document class %s reached the prompt:\n%s", docClass, rendered)
		}
	}
	if !strings.Contains(rendered, "**Provision**") || !strings.Contains(rendered, "**incident**") {
		t.Errorf("chunk classes missing from the prompt:\n%s", rendered)
	}
}

// TestOntologyTree_SectionlessDocumentReadsAsBefore.
//
// The compatibility guarantee: no directive, no sections. A root child titled like a
// section, and a chunk titled like a grammar, keep the reading they always had.
func TestOntologyTree_SectionlessDocumentReadsAsBefore(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, "# Tenant Ontology\n\n"+
		"## Classes\n- `name`\n\n### grammar:x\n- `y`\n\n"+
		"## document\n- `title`\n")
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]memrank.OntologyTerm{}
	for _, tm := range read.Terms {
		by[tm.Name] = tm
	}
	if len(read.Sections) != 0 || len(read.Grammars) != 0 || len(read.Issues) != 0 {
		t.Errorf("a section-less document grew sections/grammars/issues: %#v", read)
	}
	if by["grammar:x"].Parent != "Classes" {
		t.Errorf("a grammar-titled chunk in a flat document must stay a subclass: %v", termNames(by))
	}
	// An entity type named "document" in a flat ontology is NOT a document class: it
	// stays in the prompt, where it has always been.
	if by["document"].IsDocumentClass {
		t.Error("a flat document's entity type was turned into a document class")
	}
	if !strings.Contains(memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true), "**document**") {
		t.Error("the flat entity type `document` left the prompt")
	}
}

// TestOntologyTree_DuplicateAndUnknownSectionsFailOpen.
//
// A second section of a kind, and a section of an unknown kind, both read the way the
// chunk read before sections existed — as a class — and are reported. Declaring
// sections must never take a type out of force.
func TestOntologyTree_DuplicateAndUnknownSectionsFailOpen(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, "# Tenant Ontology\n\n"+
		"## Classes\n@section classes\n\n### project\n\n"+
		"## More\n@section classes\n\n### incident\n\n"+
		"## Rules\n@section rules\n\n### approval\n")
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]memrank.OntologyTerm{}
	for _, tm := range read.Terms {
		by[tm.Name] = tm
	}
	if by["project"].Parent != "" {
		t.Errorf("project should be a root class of the first section: %#v", by["project"])
	}
	if by["More"].Name == "" || by["incident"].Parent != "More" {
		t.Errorf("the duplicate section should read as a class with its subclasses: %v", termNames(by))
	}
	if by["Rules"].Name == "" || by["approval"].Parent != "Rules" {
		t.Errorf("the unknown section kind should read as a class: %v", termNames(by))
	}
	joined := strings.Join(read.Issues, "\n")
	if !strings.Contains(joined, "second @section classes") || !strings.Contains(joined, `"rules"`) {
		t.Errorf("issues = %q", joined)
	}
}

// TestOntologyTree_DepthCapCountsFromTheSection.
//
// Four levels below the section are allowed. Markdown stops at H6, so the deepest
// class import_md can build here is d at H6 — level 4 below the section but level 5
// below the root. Counted from the root it would be flattened onto b; counted from the
// section (RFC FO §3.1) it keeps its written parent c.
func TestOntologyTree_DepthCapCountsFromTheSection(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, "# Tenant Ontology\n\n## Classes\n@section classes\n\n"+
		"### a\n\n#### b\n\n##### c\n\n###### d\n")
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]memrank.OntologyTerm{}
	for _, tm := range read.Terms {
		by[tm.Name] = tm
	}
	if by["d"].Parent != "c" || read.DepthCapped {
		t.Errorf("level 4 under the section should keep parent c uncapped, got parent %q capped=%v", by["d"].Parent, read.DepthCapped)
	}
}

func termNames(m map[string]memrank.OntologyTerm) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
