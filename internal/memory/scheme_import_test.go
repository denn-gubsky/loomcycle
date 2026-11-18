package memory

import (
	"strings"
	"testing"
)

func outlineNames(cs []ProposedConcept) string {
	var parts []string
	for _, n := range OutlineOf(cs) {
		parts = append(parts, strings.Repeat(">", n.Level)+n.Name)
	}
	return strings.Join(parts, " ")
}

// TestParseSchemeOutline_HeadingsAndBulletsNest: ## headings are top concepts, bullets
// nest by indent below the heading above them, and lines under a concept set its data.
func TestParseSchemeOutline_HeadingsAndBulletsNest(t *testing.T) {
	p, err := ParseSchemeOutline("# S\nWhat it is.\n## A\nnotation: 1\n- `1.1` B\n    alt: Bee\n    - C\n  plain words\n- D\n### E\n```\n- not a concept\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "S" || p.Meaning != "What it is." {
		t.Errorf("name/meaning = %q / %q", p.Name, p.Meaning)
	}
	if got := outlineNames(p.Concepts); got != ">A >>B >>>C >>D >>E" {
		t.Errorf("outline = %s", got)
	}
	a := p.Concepts[0]
	b := a.Children[0]
	if a.Notation != "1" || b.Notation != "1.1" || strings.Join(b.Alt, ",") != "Bee" {
		t.Errorf("a = %#v", a)
	}
	if c := b.Children[0]; c.Meaning != "plain words" {
		t.Errorf("c meaning = %q", c.Meaning)
	}
}

// TestParseSchemeOutline_SkippedLevelIsRefused: a heading two levels below the one above
// it has no parent to hang under.
func TestParseSchemeOutline_SkippedLevelIsRefused(t *testing.T) {
	for _, md := range []string{"## A\n#### B\n", "- A\n# S\n", "```\n- A\n"} {
		if _, err := ParseSchemeOutline(md); err == nil {
			t.Errorf("%q was accepted", md)
		}
	}
}

// TestSchemeProposalCheck_RefusesWhatCannotBeWritten: a duplicate tag, an unknown
// origin and an empty name are refused; a notation with a space and newlines in values
// are made safe with a warning rather than written as a second directive line.
func TestSchemeProposalCheck_RefusesWhatCannotBeWritten(t *testing.T) {
	for name, p := range map[string]SchemeProposal{
		"duplicate tag": {Name: "S", Concepts: []ProposedConcept{{Name: "A b"}, {Name: "a  B"}}},
		"origin":        {Name: "S", Header: SchemeHeader{Origin: "stolen"}, Concepts: []ProposedConcept{{Name: "A"}}},
		"empty name":    {Name: "S", Concepts: []ProposedConcept{{Name: " "}}},
		"no concepts":   {Name: "S"},
		"hash name":     {Name: "# S", Concepts: []ProposedConcept{{Name: "A"}}},
	} {
		if _, err := p.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p := SchemeProposal{Name: "S", Header: SchemeHeader{Domain: "a\n@acl x", Projects: []string{"A, B"}},
		Concepts: []ProposedConcept{{Name: "A", Notation: "Q A", Alt: []string{"x\n@retention 1d"}}}}
	warnings, err := p.Check()
	if err != nil {
		t.Fatal(err)
	}
	if p.Concepts[0].Notation != "" || len(warnings) != 1 {
		t.Errorf("notation = %q, warnings = %v", p.Concepts[0].Notation, warnings)
	}
	lines := append(p.Header.Directives(), p.Concepts[0].Directives()...)
	body := strings.Join(lines, "\n")
	ds := ParseDirectives(body)
	if len(ds) != len(lines) {
		t.Errorf("a value made a directive line of its own:\n%s", body)
	}
	if strings.Join(p.Header.Projects, "|") != "A B" {
		t.Errorf("projects = %v", p.Header.Projects)
	}
}

const twoSchemes = `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:skos="http://www.w3.org/2004/02/skos/core#">
<skos:ConceptScheme rdf:about="s1"><skos:prefLabel>One</skos:prefLabel></skos:ConceptScheme>
<skos:ConceptScheme rdf:about="s2"><skos:prefLabel>Two</skos:prefLabel></skos:ConceptScheme>
<skos:Concept rdf:about="a"><skos:prefLabel>A</skos:prefLabel><skos:topConceptOf rdf:resource="s1"/></skos:Concept>
<skos:Concept rdf:about="b"><skos:prefLabel>B</skos:prefLabel><skos:broader rdf:resource="a"/></skos:Concept>
<skos:Concept rdf:about="c"><skos:prefLabel>C</skos:prefLabel><skos:inScheme rdf:resource="s2"/></skos:Concept>
<skos:Concept rdf:about="d"><skos:prefLabel>D</skos:prefLabel><skos:broader rdf:resource="a"/><skos:broader rdf:resource="b"/></skos:Concept>
<skos:Concept rdf:about="x"><skos:prefLabel>X</skos:prefLabel><skos:broader rdf:resource="y"/></skos:Concept>
<skos:Concept rdf:about="y"><skos:prefLabel>Y</skos:prefLabel><skos:broader rdf:resource="x"/><skos:inScheme rdf:resource="s1"/></skos:Concept>
</rdf:RDF>`

// TestParseSKOS_PicksOneSchemeAndOneParent: a file of two schemes needs one named; the
// named scheme takes its own concepts and those narrower than them; a concept with two
// broader concepts sits under the first, and a loop is broken, each with a warning.
func TestParseSKOS_PicksOneSchemeAndOneParent(t *testing.T) {
	if _, _, err := ParseSKOS([]byte(twoSchemes), "en", ""); err == nil || !strings.Contains(err.Error(), "pass scheme") {
		t.Fatalf("two schemes without a choice: %v", err)
	}
	p, warnings, err := ParseSKOS([]byte(twoSchemes), "en", "One")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "One" {
		t.Errorf("name = %q", p.Name)
	}
	if got := outlineNames(p.Concepts); got != ">A >>B >>D >X >>Y" && got != ">A >>B >>D >Y >>X" {
		t.Errorf("outline = %s", got)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "more than one broader") || !strings.Contains(joined, "loop") {
		t.Errorf("warnings = %v", warnings)
	}
}
