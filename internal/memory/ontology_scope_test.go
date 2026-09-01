package memory

import (
	"strings"
	"testing"
)

// Organisation knowledge is placed by DECLARATION on the entity type, not by a
// per-fact model judgement. These tests pin the properties that make that safe: the
// default routes nothing, an unusable value is never guessed into a real one, and the
// declaration never reaches the agent-facing prompt.

func TestOntologyScope_DirectiveParsesAndIgnoresTrailingProse(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"bare", "- `@memory_scope` tenant", "tenant"},
		{"em-dash note", "- `@memory_scope` tenant — everyone in the org depends on these", "tenant"},
		{"no space before the dash", "- `@memory_scope` user—private to one person", "user"},
		{"mixed case", "- `@memory_scope` Tenant", "tenant"},
		{"among fields", "- `name` — what people call it\n- `@memory_scope` tenant\n- `owner` — accountable", "tenant"},
		{"absent", "- `name` — what people call it", ""},
		{"last wins", "- `@memory_scope` user\n- `@memory_scope` tenant", "tenant"},
		// Separator spellings an operator would not think to doubt.
		{"colon", "- `@memory_scope`: tenant", "tenant"},
		{"colon inside prose", "- `@memory_scope`: tenant — shared", "tenant"},
		{"equals", "- `@memory_scope` = user", "user"},
		{"value backticked", "- `@memory_scope` `tenant`", "tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, issue := ParseOntologyScope(tc.body)
			if got != tc.want {
				t.Errorf("scope = %q, want %q (issue %q)", got, tc.want, issue)
			}
			if tc.want != "" && issue != "" {
				t.Errorf("a valid declaration must carry no advisory, got %q", issue)
			}
		})
	}
}

// TestOntologyScope_UnusableValueIsReportedNotGuessed is the safety property. The wrong
// guess here is "tenant", and its blast radius is every user in the tenant — so an
// unrecognised value must leave the type placing NOTHING and say why.
func TestOntologyScope_UnusableValueIsReportedNotGuessed(t *testing.T) {
	for _, body := range []string{
		"- `@memory_scope` global",
		"- `@memory_scope` agent",   // a real memory scope, but not one facts can be placed in
		"- `@memory_scope` run",     // ditto, and ephemeral
		"- `@memory_scope` TENANTS", // near-miss on a valid value
		"- `@memory_scope`",         // names nothing at all
	} {
		got, issue := ParseOntologyScope(body)
		if got != "" {
			t.Errorf("%q: declared scope %q — an unusable value must not resolve to a real scope", body, got)
		}
		if issue == "" {
			t.Errorf("%q: silently ignored; an operator who typo'd this needs to be told", body)
			continue
		}
		// The advisory is read by a person in a panel. Nested backticks from building it
		// out of markdown fragments render as garbage, so it must name the directive and
		// the values plainly.
		if strings.Contains(issue, "``") {
			t.Errorf("%q: advisory has collapsed backticks and will render wrong: %s", body, issue)
		}
		if !strings.Contains(issue, OntologyScopeDirective) {
			t.Errorf("%q: advisory does not name the directive: %s", body, issue)
		}
	}
}

// TestOntologyScope_DirectiveIsNotAField guards the reason the `@` sigil exists. Without
// the skip, every type declaring a scope grows a phantom field the prompt then tells a
// model to fill in.
func TestOntologyScope_DirectiveIsNotAField(t *testing.T) {
	body := "- `name` — what people call it\n- `@memory_scope` tenant\n- `owner` — accountable"
	for _, f := range ParseOntologyFields(body) {
		if strings.HasPrefix(f, "@") {
			t.Errorf("ParseOntologyFields returned the directive %q as a field", f)
		}
	}
	if got := ParseOntologyFields(body); len(got) != 2 {
		t.Errorf("want exactly the two real fields, got %v", got)
	}

	// Same guarantee on the whole-document parser, which is a separate code path.
	terms := ParseOntologyMarkdown("# Tenant Ontology\n\n## service\n" + body + "\n")
	if len(terms) != 1 {
		t.Fatalf("want 1 term, got %d", len(terms))
	}
	if terms[0].MemoryScope != "tenant" {
		t.Errorf("markdown parser did not read the directive: %+v", terms[0])
	}
	for _, f := range terms[0].Fields {
		if strings.HasPrefix(f, "@") {
			t.Errorf("markdown parser kept the directive as a field %q", f)
		}
	}
}

func TestOntologyScope_InheritsFromNearestDeclaringAncestor(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{
		{Name: "organization", MemoryScope: "tenant"},
		{Name: "team", Parent: "organization"},                         // inherits tenant
		{Name: "squad", Parent: "team"},                                // inherits through team
		{Name: "contact", Parent: "organization", MemoryScope: "user"}, // declared beats inherited
		{Name: "loose"}, // no ancestor, no declaration
	})
	want := map[string]string{
		"organization": "tenant",
		"team":         "tenant",
		"squad":        "tenant",
		"contact":      "user",
		"loose":        "",
	}
	for _, tm := range terms {
		if got := EffectiveMemoryScope(tm); got != want[tm.Name] {
			t.Errorf("%s: effective scope %q, want %q", tm.Name, got, want[tm.Name])
		}
	}
	// The declaring type must still be identifiable: a subclass showing an inherited
	// scope as its own would make the operator panel claim a declaration nobody wrote.
	for _, tm := range terms {
		if tm.Name == "team" && tm.MemoryScope != "" {
			t.Errorf("team declared nothing, but MemoryScope = %q", tm.MemoryScope)
		}
	}
}

// An invalid declaration must behave exactly like no declaration — falling through to
// the ancestor — rather than becoming a silent third state that routes nowhere.
func TestOntologyScope_InvalidDeclarationFallsThroughToTheAncestor(t *testing.T) {
	_, issue := ParseOntologyScope("- `@memory_scope` nonsense")
	if issue == "" {
		t.Fatal("precondition: the value must be reported as unusable")
	}
	terms := ResolveInheritance([]OntologyTerm{
		{Name: "organization", MemoryScope: "tenant"},
		{Name: "team", Parent: "organization", MemoryScopeIssue: issue},
	})
	for _, tm := range terms {
		if tm.Name == "team" && EffectiveMemoryScope(tm) != "tenant" {
			t.Errorf("team should inherit tenant despite its own broken directive, got %q",
				EffectiveMemoryScope(tm))
		}
	}
}

// TestOntologyScope_SeedDeclaresNothing keeps the default inert. A deployment that never
// edits its ontology must place facts exactly where it does today.
func TestOntologyScope_SeedDeclaresNothing(t *testing.T) {
	for _, tm := range BaseSeedOntology() {
		if tm.MemoryScope != "" {
			t.Errorf("seed type %q declares scope %q — the seed must route nothing", tm.Name, tm.MemoryScope)
		}
	}
	for _, tm := range EffectiveOntology(nil, false) {
		if EffectiveMemoryScope(tm) != "" {
			t.Errorf("unedited ontology places %q in %q", tm.Name, EffectiveMemoryScope(tm))
		}
	}
}

// A draft ontology steers nothing, and that has to include its scope declarations —
// otherwise a half-written document would start moving facts between partitions before
// the operator confirmed it.
func TestOntologyScope_DraftOntologyDeclarationsAreDiscarded(t *testing.T) {
	tenant := []OntologyTerm{{Name: "service", MemoryScope: "tenant"}}

	for _, tm := range EffectiveOntology(tenant, false) {
		if tm.Name == "service" {
			t.Errorf("a DRAFT ontology contributed the type %q at all", tm.Name)
		}
	}
	found := false
	for _, tm := range EffectiveOntology(tenant, true) {
		if tm.Name == "service" {
			found = true
			if EffectiveMemoryScope(tm) != "tenant" {
				t.Errorf("confirmed ontology lost the declaration: %+v", tm)
			}
		}
	}
	if !found {
		t.Error("a CONFIRMED ontology must contribute the tenant's type")
	}
}

// TestOntologyScope_NotRenderedIntoTheAgentPrompt is the one that matters most.
//
// The extractor is the weakest link in the memory pipeline — its own bundle calls the
// prompt "a mitigation, not a guarantee" and caps it in a test because "size is a
// feature". Placement is deliberately NOT its judgement to make: it emits a type, and an
// operator's declaration turns that into a partition. Rendering the scope here would hand
// it the decision this whole design exists to keep away from it.
func TestOntologyScope_NotRenderedIntoTheAgentPrompt(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{
		{Name: "organization", Fields: []string{"name"}, MemoryScope: "tenant"},
		{Name: "team", Parent: "organization", Fields: []string{"lead"}},
	})
	rendered := RenderOntology(terms, true)
	for _, leak := range []string{"memory_scope", OntologyScopeDirective, "tenant scope"} {
		if strings.Contains(strings.ToLower(rendered), strings.ToLower(leak)) {
			t.Errorf("the agent-facing ontology prompt mentions %q:\n%s", leak, rendered)
		}
	}
	// It must still render the types themselves — a prompt that lost them would pass
	// the check above for the wrong reason.
	if !strings.Contains(rendered, "organization") || !strings.Contains(rendered, "team") {
		t.Errorf("the prompt lost its types:\n%s", rendered)
	}
}
