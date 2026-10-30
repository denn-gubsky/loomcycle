package memory

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseDirectives_ReadsBothSpellings (RFC FO decision 10b.1).
//
// The bare line is the RFC's spelling and the bullet is the template's; both must mean
// the same directive, or an operator copying from one into the other changes behaviour.
func TestParseDirectives_ReadsBothSpellings(t *testing.T) {
	body := "@section classes\n" +
		"@identifier_pattern (?P<number>\\d+-[IVX]+)\n" +
		"- `@memory_scope` tenant — everyone reads these\n" +
		"- `title` text @required\n" +
		"prose that mentions @identity in passing\n"
	got := ParseDirectives(body)
	want := []Directive{
		{Name: "section", Value: "classes"},
		{Name: "identifier_pattern", Value: `(?P<number>\d+-[IVX]+)`},
		{Name: "memory_scope", Value: "tenant"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("directives:\n got %#v\nwant %#v", got, want)
	}
}

// TestParseDirectives_ExamplesInCodeBlocksAreNotDeclarations.
//
// The template shows directives inside indented and fenced blocks to teach them. Read
// as declarations, the template's own documentation would configure the classes it
// sits in.
func TestParseDirectives_ExamplesInCodeBlocksAreNotDeclarations(t *testing.T) {
	body := "Write it like this:\n\n" +
		"    @identity\n" +
		"\t@anchor_grammar akn\n" +
		"```\n@exclude toc\n```\n"
	if got := ParseDirectives(body); len(got) != 0 {
		t.Errorf("examples were read as directives: %#v", got)
	}
}

// TestSectionKind_OnlyTheFirstDirectiveDeclaresASection.
func TestSectionKind_OnlyTheFirstDirectiveDeclaresASection(t *testing.T) {
	cases := map[string]string{
		"@section classes\n":                SectionClasses,
		"@section Relations — kinds\n":      "",
		"@section relations\n":              SectionRelations,
		"- `@section` schemes\n":            SectionSchemes,
		"@section instances\n@a Post\n":     SectionInstances,
		"- `name` text\n@section classes\n": SectionClasses, // a field is not a directive (RFC: first DIRECTIVE line)
		"@identity\n@section classes\n":     "",             // not the first directive
		"@section rules\n":                  "",             // unknown kind → class (fail-open)
		"no directives at all\n":            "",
	}
	for body, want := range cases {
		if got := SectionKind(body); got != want {
			t.Errorf("SectionKind(%q) = %q, want %q", body, got, want)
		}
	}
	if SectionIssue("@section rules\n") == "" {
		t.Error("an unknown section kind must be reported")
	}
	if SectionIssue("@section classes\n") != "" || SectionIssue("- `name`\n") != "" {
		t.Error("a valid section, or no section, must not be reported")
	}
}

// TestParseOntologyFieldSpecs_UntypedBulletsReadAsBefore.
//
// Every field written before typed fields existed is prose after the name. Reading a
// type out of that prose would retype fields nobody meant to type — so only a type
// KEYWORD in first position counts, and the names list is unchanged.
func TestParseOntologyFieldSpecs_UntypedBulletsReadAsBefore(t *testing.T) {
	body := "- `summary` — one sentence on what happened\n" +
		"- `cause` what turned out to be responsible\n" +
		"- `title` text @required\n" +
		"- `identifier` text @identity @unique — the act number\n" +
		"- `issued` date\n" +
		"- `in_force` enum(in_force, not_in_force, partially_in_force)\n" +
		"- `rank` concept(normative-rank) @required\n" +
		"- `parent` ref(Document)\n" +
		"- `anchor` anchor @identity\n" +
		"- `count` number\n" +
		"- `@memory_scope` tenant\n"
	got := ParseOntologyFieldSpecs(body)
	want := []FieldSpec{
		{Name: "summary", Type: FieldText},
		{Name: "cause", Type: FieldText},
		{Name: "title", Type: FieldText, Required: true},
		{Name: "identifier", Type: FieldText, Identity: true, Unique: true},
		{Name: "issued", Type: FieldDate},
		{Name: "in_force", Type: FieldEnum, Enum: []string{"in_force", "not_in_force", "partially_in_force"}},
		{Name: "rank", Type: FieldConcept, Range: "normative-rank", Required: true},
		{Name: "parent", Type: FieldRef, Range: "Document"},
		{Name: "anchor", Type: FieldAnchor, Identity: true},
		{Name: "count", Type: FieldNumber},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("field specs:\n got %#v\nwant %#v", got, want)
	}
	names := make([]string, len(got))
	for i, fs := range got {
		names[i] = fs.Name
	}
	if !reflect.DeepEqual(names, ParseOntologyFields(body)) {
		t.Errorf("typed and untyped readers disagree on names: %v vs %v", names, ParseOntologyFields(body))
	}
}

// TestParseOntologyFieldSpecs_MalformedTypeIsReportedAndReadAsText.
func TestParseOntologyFieldSpecs_MalformedTypeIsReportedAndReadAsText(t *testing.T) {
	got := ParseOntologyFieldSpecs("- `state` enum(a, b\n- `x` text @mandatory\n")
	if len(got) != 2 || got[0].Type != FieldText || got[0].Issue == "" {
		t.Fatalf("unclosed enum should be text with an issue: %#v", got)
	}
	if got[1].Issue == "" {
		t.Errorf("an unknown flag should be reported: %#v", got[1])
	}
}

// TestParseClassBody_DirectivesAndIssues.
//
// A broken pattern is reported and IGNORED — the class stays in force. A typo'd
// directive is reported, not acted on. Directives of later sections are known words
// and are not reported as unknown.
func TestParseClassBody_DirectivesAndIssues(t *testing.T) {
	d := ParseClassBody("@identifier_pattern (?P<number>\\d+)\n" +
		"@title_pattern ([unclosed\n" +
		"@anchor_grammar akn-ua extra words\n" +
		"@exclude transitional, boilerplate, toc\n" +
		"@definitions (?i)^article 1\\b|definitions\n" +
		"@identity\n" +
		"@identiy\n" +
		"@inverse referenced_by\n" +
		"@memory_scope tenant\n" +
		"- `number` text\n")
	if d.IdentifierPattern != `(?P<number>\d+)` {
		t.Errorf("identifier pattern = %q", d.IdentifierPattern)
	}
	if d.TitlePattern != "" {
		t.Errorf("an uncompilable pattern must be ignored, got %q", d.TitlePattern)
	}
	if d.AnchorGrammar != "akn-ua" || !d.TitleIdentity || d.MemoryScope != "tenant" {
		t.Errorf("decl = %#v", d)
	}
	if !reflect.DeepEqual(d.Exclude, []string{"transitional", "boilerplate", "toc"}) {
		t.Errorf("exclude = %v", d.Exclude)
	}
	if d.Definitions != `(?i)^article 1\b|definitions` {
		t.Errorf("definitions = %q", d.Definitions)
	}
	if bad := ParseClassBody("@definitions ([unclosed\n"); bad.Definitions != "" || len(bad.Issues) != 1 ||
		!strings.Contains(bad.Issues[0], "@definitions") {
		t.Errorf("an uncompilable @definitions must be ignored and reported: %#v", bad)
	}
	joined := strings.Join(d.Issues, "\n")
	if !strings.Contains(joined, "@title_pattern") || !strings.Contains(joined, "@identiy") {
		t.Errorf("issues missing the bad pattern or the typo:\n%s", joined)
	}
	if strings.Contains(joined, "@inverse") {
		t.Errorf("a later-section directive was reported as unknown:\n%s", joined)
	}
}

// TestParseOntologyScope_BareLineDeclaresScope (RFC FO decision 10b.1).
func TestParseOntologyScope_BareLineDeclaresScope(t *testing.T) {
	if sc, iss := ParseOntologyScope("@memory_scope tenant — everyone\n- `name`\n"); sc != "tenant" || iss != "" {
		t.Errorf("bare scope = (%q, %q)", sc, iss)
	}
	if sc, iss := ParseOntologyScope("@memory_scope everyone\n"); sc != "" || iss == "" {
		t.Errorf("a bad bare scope must be reported, not guessed: (%q, %q)", sc, iss)
	}
	if sc, _ := ParseOntologyScope("    @memory_scope tenant\n"); sc != "" {
		t.Errorf("an indented example must not declare a scope, got %q", sc)
	}
}

// TestParseGrammarBody_BrokenGrammarIsDisabled.
//
// A grammar that cannot be used is disabled and reported — never half-run. And a
// resolve line naming a group no pattern captures is broken even though every part
// compiles.
func TestParseGrammarBody_BrokenGrammarIsDisabled(t *testing.T) {
	ok := ParseGrammarBody("law-ua", "NormativeAct",
		"pattern: Закон України № ?(?P<number>\\d+-[IVX]+)\n- resolve: work_key=law:{number}\n")
	if ok.Issue != "" || len(ok.Patterns) != 1 || ok.Resolve != "work_key=law:{number}" {
		t.Fatalf("valid grammar: %#v", ok)
	}
	for name, body := range map[string]string{
		"no pattern":    "resolve: work_key={n}\n",
		"no resolve":    "pattern: (?P<n>\\d+)\n",
		"bad regex":     "pattern: (?P<n>\\d+\nresolve: work_key={n}\n",
		"unknown group": "pattern: (?P<n>\\d+)\nresolve: work_key={number}\n",
		"too long":      "pattern: " + strings.Repeat("a", MaxOntologyPatternLen+1) + "\nresolve: x\n",
	} {
		g := ParseGrammarBody("g", "", body)
		if g.Issue == "" || len(g.Patterns) != 0 {
			t.Errorf("%s: want a disabled grammar with an issue, got %#v", name, g)
		}
	}
	if GrammarName("grammar:law-ua") != "law-ua" || GrammarName("Grammar: akn") != "akn" ||
		GrammarName("grammar:") != "" || GrammarName("law") != "" {
		t.Error("GrammarName misreads titles")
	}
}

// TestRenderOntology_DocumentClassesNeverReachThePrompt (RFC FO decision 10b.2).
//
// The seed now carries a `Document` class. If it — or any class under it — reached the
// prompt, every deployment's extraction prompt would change. The flat render must stay
// byte-identical with document classes present in the input.
func TestRenderOntology_DocumentClassesNeverReachThePrompt(t *testing.T) {
	flat := []OntologyTerm{
		{Name: "person", Fields: []string{"name", "aliases"}},
		{Name: "event", Fields: []string{"occurred_at"}},
	}
	want := RenderOntology(flat, true)
	withDocs := append(append([]OntologyTerm{}, flat...), BaseSeedDocumentClasses()...)
	withDocs = append(withDocs, OntologyTerm{Name: "Law", Parent: "Document", IsDocumentClass: true})
	if got := RenderOntology(ResolveInheritance(withDocs), true); got != want {
		t.Errorf("document classes changed the prompt:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(RenderOntology(EffectiveClasses(nil, false), false), "Document") {
		t.Error("the seed Document class reached the seed-only prompt")
	}
	if !reflect.DeepEqual(RenderOntology(EffectiveOntology(nil, false), false),
		RenderOntology(EffectiveClasses(nil, false), false)) {
		t.Error("EffectiveClasses changed the seed-only render")
	}
}

// TestEffectiveClasses_TenantDocumentOverridesSeed.
func TestEffectiveClasses_TenantDocumentOverridesSeed(t *testing.T) {
	got := EffectiveClasses([]OntologyTerm{{Name: "Document", Fields: []string{"title", "issued"}, IsDocumentClass: true}}, true)
	n := 0
	for _, c := range got {
		if strings.EqualFold(c.Name, "document") {
			n++
			if c.Source != "tenant" || len(c.Fields) != 2 {
				t.Errorf("tenant Document did not replace the seed: %#v", c)
			}
		}
	}
	if n != 1 {
		t.Errorf("want exactly one Document class, got %d", n)
	}
	if len(EffectiveClasses(nil, false)) != len(BaseSeedOntology())+1 {
		t.Error("the seed-only class set should be the entity seed plus Document")
	}
}

// TestResolveInheritance_TypedSpecsIdentityAndDirectivesInherit.
func TestResolveInheritance_TypedSpecsIdentityAndDirectivesInherit(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{
		{Name: "Document", IsDocumentClass: true, TitleIdentity: true,
			FieldSpecs: []FieldSpec{{Name: "title", Type: FieldText, Required: true}, {Name: "issued", Type: FieldDate}}},
		{Name: "NormativeAct", Parent: "Document", IsDocumentClass: true,
			IdentifierPattern: `(?P<n>\d+)`, CitationGrammar: "law-ua", Definitions: `^Article 1\b`,
			FieldSpecs: []FieldSpec{{Name: "issued", Type: FieldText}}},
		{Name: "Law", Parent: "NormativeAct", IsDocumentClass: true, CitationGrammar: "law-ua-v2"},
	})
	law := terms[2]
	specs := AllFieldSpecs(law)
	if len(specs) != 2 || specs[0].Name != "title" || specs[1].Name != "issued" || specs[1].Type != FieldText {
		t.Errorf("Law's specs should be title (from Document) then issued as redeclared by NormativeAct: %#v", specs)
	}
	if !reflect.DeepEqual(IdentityFields(law), []string{"title"}) {
		t.Errorf("bare @identity should inherit as title identity: %v", IdentityFields(law))
	}
	if EffectiveDirective(law, "identifier_pattern") != `(?P<n>\d+)` {
		t.Error("identifier_pattern should inherit from NormativeAct")
	}
	if EffectiveDirective(law, "definitions") != `^Article 1\b` {
		t.Error("definitions should inherit from NormativeAct")
	}
	if EffectiveDirective(law, "citation_grammar") != "law-ua-v2" || law.InheritedDirectives["citation_grammar"] != "" {
		t.Error("a declared directive must win over, and not be shadowed by, the inherited one")
	}
}

// TestIdentityKey_NormalisesAndRefusesPartialKeys.
func TestIdentityKey_NormalisesAndRefusesPartialKeys(t *testing.T) {
	c := OntologyTerm{Name: "NormativeAct", FieldSpecs: []FieldSpec{
		{Name: "issuer", Identity: true}, {Name: "number", Identity: true}}}
	got, ok := IdentityKey(c, map[string]any{"issuer": "  Verkhovna   Rada ", "number": "2341-III"}, "")
	if !ok || got != "normativeact:verkhovna rada|2341-iii" {
		t.Errorf("key = %q ok=%v", got, ok)
	}
	if _, ok := IdentityKey(c, map[string]any{"issuer": "x"}, ""); ok {
		t.Error("a key missing one identity value must not be produced")
	}
	titled := OntologyTerm{Name: "Policy", TitleIdentity: true}
	if got, ok := IdentityKey(titled, nil, "Outsourcing Policy"); !ok || got != "policy:outsourcing policy" {
		t.Errorf("title-identity key = %q ok=%v", got, ok)
	}
	if _, ok := IdentityKey(OntologyTerm{Name: "x"}, map[string]any{"a": "b"}, "t"); ok {
		t.Error("a class without identity fields has no automatic key")
	}
}

// TestValidateFields_WarnsNeverRejects.
func TestValidateFields_WarnsNeverRejects(t *testing.T) {
	c := ResolveInheritance([]OntologyTerm{{Name: "Provision", FieldSpecs: []FieldSpec{
		{Name: "title", Type: FieldText, Required: true},
		{Name: "number", Type: FieldNumber},
		{Name: "issued", Type: FieldDate},
		{Name: "kind", Type: FieldEnum, Enum: []string{"definition", "obligation"}},
		{Name: "anchor", Type: FieldAnchor, Required: true},
	}}})[0]
	clean := ValidateFields(c, map[string]any{"number": "12", "issued": "2026-10-29", "kind": "Obligation", "anchor": "art-5"}, "Article 5")
	if len(clean) != 0 {
		t.Errorf("valid fields warned: %v", clean)
	}
	warns := ValidateFields(c, map[string]any{"number": "twelve", "issued": "yesterday", "kind": "rule", "extra": 1, "title": nil}, "")
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"number: expected a number", "issued: expected a date", `"rule" is not one of`,
		"anchor is required", "title is required", "extra is not declared"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}
	if ValidateFields(OntologyTerm{Name: "untyped"}, map[string]any{"x": 1}, "") != nil {
		t.Error("a class with no typed fields has nothing to validate against")
	}
}

func TestIsReservedStructuralType(t *testing.T) {
	for _, r := range []string{"root", "Section", "image", "mermaid", "canvas"} {
		if !IsReservedStructuralType(r) {
			t.Errorf("%q should be reserved", r)
		}
	}
	if IsReservedStructuralType("Provision") {
		t.Error("a class is not a structural type")
	}
}

// TestOntologyTemplate_SectionExampleDeclaresNothing.
//
// The template teaches sections with a fenced example. Anything in it that read as a
// heading or a directive would configure the very document it documents — a class
// named after the example, or a @section on the root.
func TestOntologyTemplate_SectionExampleDeclaresNothing(t *testing.T) {
	tpl := OntologyTemplate()
	if !strings.Contains(tpl, "@section classes") {
		t.Fatal("the template no longer teaches sections")
	}
	if strings.Contains(tpl, "\n### Sections") {
		t.Error("the section explainer is a heading, so it would be imported as a class")
	}
	if ds := ParseDirectives(tpl); len(ds) != 0 {
		// The bullet-form @memory_scope example is indented, and bullets are read
		// wherever they are — so only a bare-line leak is a regression here.
		for _, d := range ds {
			if d.Name != "memory_scope" {
				t.Errorf("the template body declares @%s %s", d.Name, d.Value)
			}
		}
	}
}

// TestValidateFields_TheClassificationRecordIsNotAnUndeclaredField: the record classify
// keeps on a document's root is the runtime's, never a class's to declare.
func TestValidateFields_TheClassificationRecordIsNotAnUndeclaredField(t *testing.T) {
	law := OntologyTerm{Name: "Law", FieldSpecs: []FieldSpec{{Name: "number", Type: FieldText}}}
	warns := ValidateFields(law, map[string]any{"number": "1-I", "classification": map[string]any{"by": "classify"}, "colour": "red"}, "")
	if len(warns) != 1 || !strings.Contains(warns[0], "colour") {
		t.Errorf("warnings = %v, want only the undeclared colour", warns)
	}
}

// TestParseClassBody_ACitationGrammarMayNameSeveral: a class citing laws and regulator
// acts lists both grammars; names are split on commas or spaces, deduplicated, in
// order. An anchor grammar stays one name.
func TestParseClassBody_ACitationGrammarMayNameSeveral(t *testing.T) {
	d := ParseClassBody("@citation_grammar nbu-resolution, law-ua law-ua\n@anchor_grammar nbu-point, other\n")
	if d.CitationGrammar != "nbu-resolution, law-ua" || d.AnchorGrammar != "nbu-point" {
		t.Errorf("citation %q anchor %q", d.CitationGrammar, d.AnchorGrammar)
	}
	if got := GrammarNames(" a,,b  a ,c"); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("GrammarNames = %v", got)
	}
}
