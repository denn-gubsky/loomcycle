package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const subjectOutline = "# Subject headings\n" +
	"Headings for the library catalogue.\n\n" +
	"- `QA` Mathematics\n" +
	"  alt: Maths\n" +
	"  note: The science of number and space.\n" +
	"  - `QA76` Computer science\n" +
	"    alt: Computing\n" +
	"    alt: Informatics\n" +
	"    exact: http://example.org/lcc/QA76\n" +
	"  - `QA273` Probability\n" +
	"## Physics\n" +
	"notation: QC\n" +
	"- `QC1` Mechanics\n"

// importScheme files one import_scheme call from an agent and returns the scheme chunk.
func importSchemeCall(t *testing.T, d *Document, ctx context.Context, req map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	req["op"] = "import_scheme"
	raw, _ := json.Marshal(req)
	return docExec(t, d, ctx, string(raw))
}

// describedConcepts is describe_ontology's concepts of one scheme, by tag.
func describedConcepts(t *testing.T, d *Document, ctx context.Context, scheme string) (map[string]any, map[string]map[string]any) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"describe_ontology","scope":"tenant"}`)
	if r.IsError {
		t.Fatalf("describe_ontology: %s", r.Text)
	}
	schemes, _ := out["schemes"].([]any)
	for _, s := range schemes {
		sm := s.(map[string]any)
		if sm["name"] != scheme {
			continue
		}
		by := map[string]map[string]any{}
		cs, _ := sm["concepts"].([]any)
		for _, c := range cs {
			cm := c.(map[string]any)
			by[cm["tag"].(string)] = cm
		}
		return sm, by
	}
	return nil, nil
}

func acceptOrFail(t *testing.T, d *Document, ctx context.Context, id string) SchemeAcceptReport {
	t.Helper()
	handled, rep, err := d.AcceptSchemeProposal(ctx, "tenant", id)
	if !handled || err != nil {
		t.Fatalf("accept: %v %v", handled, err)
	}
	return rep
}

// TestImportScheme_OutlineRoundTrips: an outline imported, accepted and described reads
// back as the same tree, with its notations, alternative labels, notes and matches.
func TestImportScheme_OutlineRoundTrips(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": subjectOutline,
		"header": map[string]any{"source": "Library of Example", "version": "2026"}})
	if r.IsError {
		t.Fatalf("import: %s", refusalText(r))
	}
	if out["proposed"] != "Subject headings" || out["concepts"] != float64(5) {
		t.Fatalf("result = %v", out)
	}
	rep := acceptOrFail(t, d, opCtx, out["chunk_id"].(string))
	if rep.Concepts != 5 {
		t.Errorf("accepted %d concepts", rep.Concepts)
	}
	s, by := describedConcepts(t, d, opCtx, "Subject headings")
	if s == nil {
		t.Fatal("the scheme is not described")
	}
	if s["origin"] != "adopted" || s["source"] != "Library of Example" || s["version"] != "2026" ||
		s["meaning"] != "Headings for the library catalogue." {
		t.Errorf("scheme = %v", s)
	}
	want := map[string]string{ // tag → notation|alt|parent|exact
		"subject-headings/mathematics":                  "QA|Maths||",
		"subject-headings/mathematics/computer-science": "QA76|Computing,Informatics|Mathematics|http://example.org/lcc/QA76",
		"subject-headings/mathematics/probability":      "QA273||Mathematics|",
		"subject-headings/physics":                      "QC|||",
		"subject-headings/physics/mechanics":            "QC1||Physics|",
	}
	if len(by) != len(want) {
		t.Errorf("described %d concepts, want %d: %v", len(by), len(want), by)
	}
	for tag, w := range want {
		c := by[tag]
		if c == nil {
			t.Errorf("%s missing", tag)
			continue
		}
		var alt []string
		for _, a := range asSlice(c["alt"]) {
			alt = append(alt, fmt.Sprint(a))
		}
		got := fmt.Sprintf("%v|%s|%v|%v", str(c["notation"]), strings.Join(alt, ","), str(c["parent"]), str(c["exact_match"]))
		if got != w {
			t.Errorf("%s = %s, want %s", tag, got, w)
		}
		if c["origin"] != "adopted" {
			t.Errorf("%s origin = %v", tag, c["origin"])
		}
	}
	if m := by["subject-headings/mathematics"]["meaning"]; m != "The science of number and space." {
		t.Errorf("note = %v", m)
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

const skosDoc = `<?xml version="1.0" encoding="UTF-8"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
         xmlns:skos="http://www.w3.org/2004/02/skos/core#">
  <skos:ConceptScheme rdf:about="http://example.org/fp">
    <skos:prefLabel xml:lang="en">File plan</skos:prefLabel>
    <skos:prefLabel xml:lang="uk">План справ</skos:prefLabel>
    <skos:definition xml:lang="en">Where records are filed.</skos:definition>
    <skos:hasTopConcept rdf:resource="http://example.org/fp/1"/>
  </skos:ConceptScheme>
  <skos:Concept rdf:about="http://example.org/fp/1">
    <skos:prefLabel xml:lang="en-GB">Finance</skos:prefLabel>
    <skos:prefLabel xml:lang="uk">Фінанси</skos:prefLabel>
    <skos:altLabel xml:lang="en">Money</skos:altLabel>
    <skos:altLabel xml:lang="uk">Гроші</skos:altLabel>
    <skos:notation>01</skos:notation>
    <skos:scopeNote xml:lang="en">Budgets and accounts.</skos:scopeNote>
    <skos:topConceptOf rdf:resource="http://example.org/fp"/>
    <skos:narrower>
      <skos:Concept rdf:about="http://example.org/fp/1/1">
        <skos:prefLabel xml:lang="en">Budgets</skos:prefLabel>
        <skos:prefLabel xml:lang="uk">Бюджети</skos:prefLabel>
        <skos:notation>01.1</skos:notation>
        <skos:exactMatch rdf:resource="http://other.example/budget"/>
      </skos:Concept>
    </skos:narrower>
  </skos:Concept>
  <rdf:Description rdf:about="http://example.org/fp/1/2">
    <rdf:type rdf:resource="http://www.w3.org/2004/02/skos/core#Concept"/>
    <skos:prefLabel xml:lang="en">Accounts</skos:prefLabel>
    <skos:prefLabel xml:lang="uk">Рахунки</skos:prefLabel>
    <skos:notation>01.2</skos:notation>
    <skos:broader rdf:resource="http://example.org/fp/1"/>
  </rdf:Description>
</rdf:RDF>`

// TestImportScheme_SKOSNotationAndLabelsAreKept: a SKOS scheme's labels, notations,
// notes, matches and hierarchy (nested, broader and narrower) survive the import.
func TestImportScheme_SKOSNotationAndLabelsAreKept(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, classesOnlyOntologyMD)
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"skos_xml": skosDoc})
	if r.IsError {
		t.Fatalf("import: %s", refusalText(r))
	}
	acceptOrFail(t, d, opCtx, out["chunk_id"].(string))
	s, by := describedConcepts(t, d, opCtx, "File plan")
	if s == nil || s["meaning"] != "Where records are filed." {
		t.Fatalf("scheme = %v", s)
	}
	fin, bud, acc := by["file-plan/finance"], by["file-plan/finance/budgets"], by["file-plan/finance/accounts"]
	if fin == nil || bud == nil || acc == nil {
		t.Fatalf("concepts = %v", by)
	}
	if fin["notation"] != "01" || fmt.Sprint(fin["alt"]) != "[Money]" || fin["meaning"] != "Budgets and accounts." {
		t.Errorf("finance = %v", fin)
	}
	if bud["notation"] != "01.1" || bud["exact_match"] != "http://other.example/budget" || acc["notation"] != "01.2" {
		t.Errorf("budgets = %v, accounts = %v", bud, acc)
	}
}

// TestImportScheme_SKOSPicksTheRequestedLanguage: language picks the labels; without it
// the default (here en, the tenant naming none) is used, en-GB counting as en.
func TestImportScheme_SKOSPicksTheRequestedLanguage(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, classesOnlyOntologyMD)
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"skos_xml": skosDoc, "language": "uk"})
	if r.IsError {
		t.Fatalf("import uk: %s", refusalText(r))
	}
	if out["proposed"] != "План справ" {
		t.Errorf("uk scheme name = %v", out["proposed"])
	}
	acceptOrFail(t, d, opCtx, out["chunk_id"].(string))
	_, by := describedConcepts(t, d, opCtx, "План справ")
	fin := by[memrank.MirrorTag("План справ", []string{"Фінанси"})]
	if fin == nil || fmt.Sprint(fin["alt"]) != "[Гроші]" {
		t.Errorf("uk concepts = %v", by)
	}
	if by[memrank.MirrorTag("План справ", []string{"Фінанси", "Бюджети"})] == nil {
		t.Errorf("uk narrower missing: %v", by)
	}

	d2, op2, agent2 := schemeProposalFixture(t, classesOnlyOntologyMD)
	out, r = importSchemeCall(t, d2, agent2, map[string]any{"skos_xml": skosDoc})
	if r.IsError {
		t.Fatalf("import default: %s", refusalText(r))
	}
	if out["proposed"] != "File plan" {
		t.Errorf("default-language scheme name = %v", out["proposed"])
	}
	read := readOntologyNow(t, d2, op2)
	if p := schemeProposalNamed(read, "File plan"); p == nil || p.Scheme.Outline[0].Name != "Finance" {
		t.Errorf("en-GB label not taken for en: %#v", p)
	}
}

// TestImportScheme_IsAProposalUntilAccepted: an import changes nothing in force; its
// proposal carries @origin adopted, which accepting puts on the scheme and every concept.
func TestImportScheme_IsAProposalUntilAccepted(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": subjectOutline})
	if r.IsError {
		t.Fatalf("import: %s", refusalText(r))
	}
	if s, _ := describedConcepts(t, d, opCtx, "Subject headings"); s != nil {
		t.Fatal("an imported scheme is in force before it is accepted")
	}
	p := schemeProposalNamed(readOntologyNow(t, d, opCtx), "Subject headings")
	if p == nil || p.Scheme.Header.Origin != memrank.SchemeOriginAdopted || p.Status != memrank.OntologyStatusProposed {
		t.Fatalf("proposal = %#v", p)
	}
	if _, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": subjectOutline, "header": map[string]any{"origin": "generated"}}); !r.IsError {
		t.Error("an import as origin generated was accepted")
	}
	acceptOrFail(t, d, opCtx, out["chunk_id"].(string))
	s, by := describedConcepts(t, d, opCtx, "Subject headings")
	if s == nil || s["origin"] != "adopted" {
		t.Fatalf("scheme = %v", s)
	}
	for tag, c := range by {
		if c["origin"] != "adopted" {
			t.Errorf("%s origin = %v", tag, c["origin"])
		}
	}
}

// TestImportScheme_DeepSchemeKeepsEightLevels: a scheme eight levels deep is kept whole;
// a ninth level is lifted onto the eighth with a warning, as the reader would.
func TestImportScheme_DeepSchemeKeepsEightLevels(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	var b strings.Builder
	b.WriteString("# Deep\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "%s- L%d\n", strings.Repeat("  ", i-1), i)
	}
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": b.String()})
	if r.IsError {
		t.Fatalf("import: %s", refusalText(r))
	}
	if !strings.Contains(fmt.Sprint(out["warnings"]), "deeper than 8") {
		t.Errorf("no depth warning: %v", out["warnings"])
	}
	acceptOrFail(t, d, opCtx, out["chunk_id"].(string))
	read := readOntologyNow(t, d, opCtx)
	s := schemeNamed(read, "Deep")
	if s == nil || len(s.Concepts) != 9 || len(s.Issues) != 0 {
		t.Fatalf("scheme = %#v", s)
	}
	depth := map[string]int{}
	for _, c := range s.Concepts {
		depth[c.Name] = len(c.Path)
	}
	if depth["L8"] != memrank.SchemeMaxDepth || depth["L9"] != memrank.SchemeMaxDepth || depth["L1"] != 1 {
		t.Errorf("depths = %v", depth)
	}
}

// TestImportScheme_OverCapNamesTheLimit: an import over the cap is refused, naming the
// limit and that a larger scheme waits for a document of its own.
func TestImportScheme_OverCapNamesTheLimit(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	var b strings.Builder
	b.WriteString("# Big\n")
	for i := 0; i <= memrank.SchemeProposalMaxConcepts; i++ {
		fmt.Fprintf(&b, "- c%d\n", i)
	}
	_, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": b.String()})
	text := refusalText(r)
	if !r.IsError || !strings.Contains(text, fmt.Sprint(memrank.SchemeProposalMaxConcepts)) || !strings.Contains(text, "P1.5") {
		t.Fatalf("over the cap: %s", text)
	}
	if schemeProposalNamed(readOntologyNow(t, d, opCtx), "Big") != nil {
		t.Error("a refused import was filed")
	}
	var x strings.Builder
	x.WriteString(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:skos="http://www.w3.org/2004/02/skos/core#">`)
	for i := 0; i <= memrank.SchemeProposalMaxConcepts; i++ {
		fmt.Fprintf(&x, `<skos:Concept rdf:about="c%d"><skos:prefLabel>c%d</skos:prefLabel></skos:Concept>`, i, i)
	}
	x.WriteString(`</rdf:RDF>`)
	_, r = importSchemeCall(t, d, agentCtx, map[string]any{"skos_xml": x.String(), "name": "Big"})
	if text := refusalText(r); !r.IsError || !strings.Contains(text, "P1.5") {
		t.Errorf("SKOS over the cap: %s", text)
	}
}

// TestImportScheme_MalformedXMLIsRefused: broken XML, a DOCTYPE with an entity bomb and
// an external entity are each refused, quickly, and file nothing.
func TestImportScheme_MalformedXMLIsRefused(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	bomb := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">` +
		`<!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">]>` +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:skos="http://www.w3.org/2004/02/skos/core#">` +
		`<skos:Concept rdf:about="a"><skos:prefLabel>&lol3;</skos:prefLabel></skos:Concept></rdf:RDF>`
	external := `<?xml version="1.0"?><!DOCTYPE r [<!ENTITY x SYSTEM "file:///etc/passwd">]>` +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:skos="http://www.w3.org/2004/02/skos/core#">` +
		`<skos:Concept rdf:about="a"><skos:prefLabel>&x;</skos:prefLabel></skos:Concept></rdf:RDF>`
	undeclared := `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:skos="http://www.w3.org/2004/02/skos/core#">` +
		`<skos:Concept rdf:about="a"><skos:prefLabel>&x;</skos:prefLabel></skos:Concept></rdf:RDF>`
	for name, in := range map[string]string{
		"unclosed":     `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><skos:Concept>`,
		"not xml":      "this is not xml",
		"entity bomb":  bomb,
		"external":     external,
		"undeclared":   undeclared,
		"no concept":   `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"/>`,
		"deep nesting": strings.Repeat("<a>", 200) + strings.Repeat("</a>", 200),
	} {
		start := time.Now()
		_, r := importSchemeCall(t, d, agentCtx, map[string]any{"skos_xml": in, "name": "X"})
		if !r.IsError {
			t.Errorf("%s: accepted", name)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("%s: took %s", name, time.Since(start))
		}
		if strings.Contains(r.Text, "root:") {
			t.Errorf("%s: the refusal echoes a file's content: %s", name, r.Text)
		}
	}
	if len(readOntologyNow(t, d, opCtx).Proposals) != 0 {
		t.Error("a refused import filed a proposal")
	}
}

// TestOntologySchemes_WholeSchemeProposalOnPostgres files, reads and accepts a whole
// scheme on the postgres tier.
func TestOntologySchemes_WholeSchemeProposalOnPostgres(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres tier")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	base := tools.WithAgentName(context.Background(), "pg-scheme-proposal")
	base = tools.WithRunIdentity(base, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	opCtx := installTenantOntology(t, d, base, classesOnlyOntologyMD, memrank.OntologyConfirmedStatus)

	out, r := importSchemeCall(t, d, base, map[string]any{"markdown": subjectOutline})
	if r.IsError {
		t.Fatalf("import on postgres: %s", refusalText(r))
	}
	if p := schemeProposalNamed(readOntologyNow(t, d, opCtx), "Subject headings"); p == nil || p.Scheme.Concepts != 5 {
		t.Fatalf("proposal on postgres = %#v", p)
	}
	rep := acceptOrFail(t, d, opCtx, out["chunk_id"].(string))
	if rep.Concepts != 5 || !rep.SectionCreated {
		t.Errorf("accept on postgres = %#v", rep)
	}
	s := schemeNamed(readOntologyNow(t, d, opCtx), "Subject headings")
	if s == nil || len(s.Concepts) != 5 || s.Origin != "adopted" {
		t.Fatalf("scheme on postgres = %#v", s)
	}
}
