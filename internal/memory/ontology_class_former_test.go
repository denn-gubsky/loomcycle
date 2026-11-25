package memory

import (
	"reflect"
	"strings"
	"testing"
)

// TestFormerClassNames_ResolveTheClassCaseInsensitively: a former name resolves to its
// class in any case; a class in force, an unknown name and "" come back unchanged.
func TestFormerClassNames_ResolveTheClassCaseInsensitively(t *testing.T) {
	a := FormerClassNames(EffectiveClasses([]OntologyTerm{
		{Name: "Rule", RenamedFrom: "Policy"},
		{Name: "Memo"},
	}, true))
	for in, want := range map[string]string{
		"Policy": "Rule", "policy": "Rule", " POLICY ": "Rule",
		"Rule": "Rule", "Memo": "Memo", "Polic": "Polic", "": "", "person": "person",
	} {
		if got := a.Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
	if got := a.Former("rule"); got != "Policy" {
		t.Errorf("Former(rule) = %q, want the directive's spelling", got)
	}
	if got := a.Former("Memo"); got != "" {
		t.Errorf("Former(Memo) = %q, want none", got)
	}
	if !a.Any() {
		t.Error("Any() = false with a former name honoured")
	}
}

// TestFormerClassNames_NoneDeclaredIsTheZeroValue: an ontology with no @renamed_from
// yields the zero value and no issue, so every caller's old path is taken.
func TestFormerClassNames_NoneDeclaredIsTheZeroValue(t *testing.T) {
	classes := EffectiveClasses([]OntologyTerm{{Name: "Rule"}, {Name: "Memo"}}, true)
	a := FormerClassNames(classes)
	if a.Any() || !reflect.DeepEqual(a, ClassAliases{}) {
		t.Errorf("aliases = %+v, want the zero value", a)
	}
	if issues := FormerClassNameIssues(classes); issues != nil {
		t.Errorf("issues = %v, want none", issues)
	}
	if got := a.Resolve("Policy"); got != "Policy" {
		t.Errorf("Resolve(Policy) = %q", got)
	}
}

// TestFormerClassNames_ContestedOrInForceNameIsNotHonoured: a name two classes claim
// reads as neither, a name that is a class in force (a seed class included, and the
// class's own name) stays that class's, and each is reported; a name one class claims
// beside them is still honoured.
func TestFormerClassNames_ContestedOrInForceNameIsNotHonoured(t *testing.T) {
	classes := EffectiveClasses([]OntologyTerm{
		{Name: "Rule", RenamedFrom: "Policy"},
		{Name: "Memo", RenamedFrom: "policy"},  // contests Policy, in another case
		{Name: "Note", RenamedFrom: "Memo"},    // a class in force
		{Name: "Human", RenamedFrom: "person"}, // a seed class
		{Name: "Self", RenamedFrom: "self"},    // its own name
		{Name: "Partner", RenamedFrom: "Supplier"},
	}, true)
	a := FormerClassNames(classes)
	for _, name := range []string{"Policy", "Memo", "person", "Self"} {
		if got := a.Resolve(name); got != name {
			t.Errorf("Resolve(%q) = %q, want it unchanged", name, got)
		}
	}
	for _, class := range []string{"Rule", "Memo", "Note", "Human", "Self"} {
		if got := a.Former(class); got != "" {
			t.Errorf("Former(%q) = %q, want none", class, got)
		}
	}
	if got := a.Resolve("supplier"); got != "Partner" {
		t.Errorf("Resolve(supplier) = %q, want Partner", got)
	}
	issues := strings.Join(FormerClassNameIssues(classes), "\n")
	for _, want := range []string{
		`claimed by @renamed_from on more than one class (Memo, Rule)`,
		`@renamed_from Memo on the class "Note" names a class in force ("Memo")`,
		`@renamed_from person on the class "Human" names a class in force ("person")`,
		`@renamed_from self on the class "Self" names a class in force ("Self")`,
	} {
		if !strings.Contains(issues, want) {
			t.Errorf("issues lack %q:\n%s", want, issues)
		}
	}
	if strings.Contains(issues, "Supplier") {
		t.Errorf("an honoured name was reported:\n%s", issues)
	}
}

// TestTenantClassAliases_DraftHonoursNone: a draft's classes are not in force, so its
// former names are not honoured; the confirmed layer's are.
func TestTenantClassAliases_DraftHonoursNone(t *testing.T) {
	terms := []OntologyTerm{{Name: "Rule", RenamedFrom: "Policy"}}
	if a := TenantClassAliases(terms, false); a.Any() {
		t.Errorf("a draft honours %+v", a)
	}
	if got := TenantClassAliases(terms, true).Resolve("Policy"); got != "Rule" {
		t.Errorf("confirmed Resolve(Policy) = %q, want Rule", got)
	}
	if a := TenantClassAliases([]OntologyTerm{{Name: "Rule"}}, true); a.Any() {
		t.Errorf("no directive, yet %+v", a)
	}
}

// TestResolvePlacement_FormerClassNameTakesTheClassDeclaration: a fact typed with the
// former name of a class is placed where the class declares, as one typed with its
// name is; a misspelt type stays where the writer put it.
func TestResolvePlacement_FormerClassNameTakesTheClassDeclaration(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{
		{Name: "provider", MemoryScope: "tenant", RenamedFrom: "service"},
		{Name: "person", MemoryScope: "user"},
	})
	for typ, want := range map[string]string{"provider": "tenant", "service": "tenant", "Service": "tenant", "servce": "user"} {
		got := ResolvePlacement(base(PlacementInput{DeclaredType: typ, Subject: "checkout-api", Terms: terms}))
		if got.Scope != want {
			t.Errorf("type %q placed in %q, want %q (%s)", typ, got.Scope, want, got.Reason)
		}
	}
	// A subject recorded under both spellings of ONE class is not typed inconsistently.
	got := ResolvePlacement(base(PlacementInput{DeclaredType: "provider", Subject: "checkout-api", Terms: terms,
		SubjectTypes: []string{"service", "provider"}}))
	if got.Scope != "tenant" || got.Advisory != "" {
		t.Errorf("both spellings of one class: scope %q, advisory %q", got.Scope, got.Advisory)
	}
}

// TestParseClassBody_RenamedFromTakesTheWholeName: a class name may hold spaces, so the
// directive's whole value is the former name (it used to be cut to its first word,
// which no stored type equals); prose after a dash is a comment.
func TestParseClassBody_RenamedFromTakesTheWholeName(t *testing.T) {
	for body, want := range map[string]string{
		"@renamed_from Internal Policy\n":               "Internal Policy",
		"@renamed_from Policy\n":                        "Policy",
		"@renamed_from  Internal Policy — since 2026\n": "Internal Policy",
		"- `@renamed_from` Internal Policy — a note\n":  "Internal Policy",
		"A class.\n": "",
	} {
		if got := ParseClassBody(body).RenamedFrom; got != want {
			t.Errorf("ParseClassBody(%q).RenamedFrom = %q, want %q", body, got, want)
		}
	}
}
