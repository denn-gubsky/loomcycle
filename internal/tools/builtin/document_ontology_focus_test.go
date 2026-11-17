package builtin

// Tests for the scheme header and @focus (RFC GM P1.1).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// focusOntologyMD has a scheme with a full header, a concept titled like the class it
// focuses on (so the class lookup must pass over the concept itself), a concept that
// focuses on an instance, and a concept with no focus.
const focusOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provider\nA component that talks to a model vendor.\n\n" +
	"### Outage\n- `summary` text\n\n### Runbook\n\n" +
	"## Relations\n@section relations\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### components\nHow the runtime is built.\n@domain Runtime architecture: which part of the system a text is about\n" +
	"@projects Provider, Outage\n@origin adopted\n@source https://example.org/components\n@version 2026.1\n\n" +
	"#### Provider\n@focus Provider\n\n#### Acme work\n@focus Acme Bank\n\n#### Misc\n\n" +
	"## Instances\n@section instances\n\n### Acme Bank\n@a organization\n"

// ontologyFocusEdge is one focus statement the ontology wrote.
type ontologyFocusEdge struct {
	id, from, to string
	expired      bool
}

func ontologyFocusEdges(t *testing.T, d *Document, ctx context.Context) []ontologyFocusEdge {
	t.Helper()
	res, err := d.query(ctx, mustTenantKey(t, d, ctx), `SELECT id, from_id, to_id, coalesce(expired_at, 0)
		FROM chunk_edges WHERE kind = 'focus' AND method = ? ORDER BY from_id, to_id`, edgeMethodOntology)
	if err != nil {
		t.Fatal(err)
	}
	var out []ontologyFocusEdge
	for _, row := range res.Rows {
		out = append(out, ontologyFocusEdge{id: asStr(row[0]), from: asStr(row[1]), to: asStr(row[2]), expired: asInt64Val(row[3]) != 0})
	}
	return out
}

func liveFocusEdges(edges []ontologyFocusEdge) map[string]string {
	out := map[string]string{}
	for _, e := range edges {
		if !e.expired {
			out[e.from] = e.to
		}
	}
	return out
}

// focusFixtureIDs are the chunks the focus statements should join.
type focusFixtureIDs struct {
	providerConcept, acmeConcept, miscConcept string
	providerClass, runbookClass, acmeSubject  string
}

func focusIDs(t *testing.T, d *Document, ctx context.Context) focusFixtureIDs {
	t.Helper()
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("ontology read failed")
	}
	by := schemeConcepts(read)
	ids := focusFixtureIDs{providerConcept: by["components/provider"].ChunkID, acmeConcept: by["components/acme-work"].ChunkID,
		miscConcept: by["components/misc"].ChunkID}
	key := mustTenantKey(t, d, ctx)
	var err error
	if ids.providerClass, err = d.classChunkID(ctx, key, read, "Provider"); err != nil || ids.providerClass == "" ||
		ids.providerClass == ids.providerConcept {
		t.Fatalf("provider class chunk = %q (concept %q), %v", ids.providerClass, ids.providerConcept, err)
	}
	if ids.runbookClass, err = d.classChunkID(ctx, key, read, "Runbook"); err != nil || ids.runbookClass == "" {
		t.Fatalf("runbook class chunk = %q, %v", ids.runbookClass, err)
	}
	ids.acmeSubject, _ = d.chunkIDByNaturalKey(ctx, key, memrank.ConsolidatorEntityKey("organization", "Acme Bank"))
	return ids
}

// setConceptBody replaces a concept chunk's body, the way an operator edits it.
func setConceptBody(t *testing.T, d *Document, ctx context.Context, chunkID, body string) {
	t.Helper()
	g, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+chunkID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"body":%q}`,
		chunkID, int(g["revision"].(float64)), body)); r.IsError {
		t.Fatal(r.Text)
	}
}

// confirmFocus runs the confirm steps that matter here, in the handler's order.
func confirmFocus(t *testing.T, d *Document, ctx context.Context) SchemeFocusReport {
	t.Helper()
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rep, err := d.MirrorSchemeFocus(ctx, false)
	if err != nil {
		t.Fatalf("focus mirror: %v", err)
	}
	return rep
}

// TestOntology_SchemeWithoutHeaderReadsIdentically: an ontology that uses none of the
// new directives answers describe_ontology byte for byte as it did before them. The
// golden file was written by this test on main before the change
// (LOOMCYCLE_UPDATE_GOLDEN=1 rewrites it).
func TestOntology_SchemeWithoutHeaderReadsIdentically(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, schemesOntologyMD, memrank.OntologyConfirmedStatus)
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	got := normaliseIDs(res.Text)
	golden := filepath.Join("testdata", "describe_ontology_schemes.golden.json")
	if os.Getenv("LOOMCYCLE_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("describe_ontology changed for an ontology without a scheme header:\n got %s\nwant %s", got, want)
	}
}

// TestOntology_SchemeHeaderIsParsed: the header and @focus are read, and
// describe_ontology reports them.
func TestOntology_SchemeHeaderIsParsed(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	if len(desc.Schemes) != 1 {
		t.Fatalf("schemes = %#v", desc.Schemes)
	}
	s := desc.Schemes[0]
	if s.Domain != "Runtime architecture: which part of the system a text is about" ||
		strings.Join(s.Projects, "|") != "Provider|Outage" || s.Origin != memrank.SchemeOriginAdopted ||
		s.Source != "https://example.org/components" || s.Version != "2026.1" || s.Meaning != "How the runtime is built." {
		t.Errorf("header = %#v", s.OntologyScheme)
	}
	if len(s.Issues) != 0 {
		t.Errorf("a valid header has issues: %v", s.Issues)
	}
	focus := map[string]string{}
	for _, c := range s.Concepts {
		focus[c.Name] = c.Focus
		if len(c.Issues) != 0 {
			t.Errorf("concept %s has issues: %v", c.Name, c.Issues)
		}
	}
	if focus["Provider"] != "Provider" || focus["Acme work"] != "Acme Bank" || focus["Misc"] != "" {
		t.Errorf("focus = %v", focus)
	}
}

// TestOntology_ProjectsUnknownClassIsAnIssue: a @projects class nobody declares is
// reported on the scheme; declared, document and standard classes are not.
func TestOntology_ProjectsUnknownClassIsAnIssue(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(focusOntologyMD, "@projects Provider, Outage", "@projects Provider, organization, Document, Gadget", 1)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyDraftStatus)
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || len(read.Schemes) != 1 {
		t.Fatalf("read = %#v", read.Schemes)
	}
	issues := read.Schemes[0].Issues
	if len(issues) != 1 || !strings.Contains(issues[0], `"Gadget" is not a class`) {
		t.Errorf("issues = %v, want one naming Gadget (a draft is checked too)", issues)
	}
	if strings.Join(read.Schemes[0].Projects, "|") != "Provider|organization|Document|Gadget" {
		t.Errorf("projects = %v", read.Schemes[0].Projects)
	}
}

// TestOntology_FocusUnknownTargetIsAnIssue: a @focus that names no class or instance
// is reported on the concept, and the confirm writes nothing for it.
func TestOntology_FocusUnknownTargetIsAnIssue(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(focusOntologyMD, "@focus Acme Bank", "@focus Nobody Inc", 1)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	read, _ := d.tenantOntologyRead(ctx)
	c := schemeConcepts(read)["components/acme-work"]
	if len(c.Issues) != 1 || !strings.Contains(c.Issues[0], `"Nobody Inc" is not a class or an instance`) {
		t.Errorf("issues = %v", c.Issues)
	}
	rep := confirmFocus(t, d, ctx)
	if rep.Concepts != 2 || rep.Written != 1 {
		t.Errorf("report = %#v, want only the Provider focus written", rep)
	}
	if live := liveFocusEdges(ontologyFocusEdges(t, d, ctx)); live[c.ChunkID] != "" {
		t.Errorf("an unknown focus was written: %v", live)
	}
}

// TestOntologyConfirm_FocusBecomesAStatement: a class focus joins the concept to the
// class's entry (not to the concept of the same title), an instance focus to the
// subject the instance was adopted as.
func TestOntologyConfirm_FocusBecomesAStatement(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	rep := confirmFocus(t, d, ctx)
	if rep.Concepts != 2 || rep.Written != 2 || len(rep.Errors) != 0 {
		t.Fatalf("report = %#v", rep)
	}
	ids := focusIDs(t, d, ctx)
	if ids.acmeSubject == "" {
		t.Fatal("Acme Bank was not adopted")
	}
	live := liveFocusEdges(ontologyFocusEdges(t, d, ctx))
	if live[ids.providerConcept] != ids.providerClass || live[ids.acmeConcept] != ids.acmeSubject || len(live) != 2 {
		t.Errorf("focus statements = %v, want %s->%s and %s->%s", live,
			ids.providerConcept, ids.providerClass, ids.acmeConcept, ids.acmeSubject)
	}
	// Written as the ontology's, asserted.
	read, _ := d.tenantOntologyRead(ctx)
	_, r := docExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+read.DocumentID+`","kind":"focus","direction":"out"}`)
	if r.IsError || !strings.Contains(r.Text, `"method":"ontology"`) || !strings.Contains(r.Text, `"status":"asserted"`) {
		t.Errorf("get_edges = %s", r.Text)
	}
}

// TestOntologyConfirm_ReconfirmWritesNoDuplicate.
func TestOntologyConfirm_ReconfirmWritesNoDuplicate(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	confirmFocus(t, d, ctx)
	again := confirmFocus(t, d, ctx)
	if again.Written != 0 || again.Retired != 0 || len(again.Stale) != 0 {
		t.Errorf("a second confirm changed something: %#v", again)
	}
	if n := len(ontologyFocusEdges(t, d, ctx)); n != 2 {
		t.Errorf("%d focus statements after two confirms, want 2", n)
	}
}

// TestOntologyConfirm_RemovedFocusIsRetired: removing a @focus expires its statement
// (never deletes it), changing one retires the old target and writes the new one, and
// declaring it again brings the same statement back.
func TestOntologyConfirm_RemovedFocusIsRetired(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	confirmFocus(t, d, ctx)
	ids := focusIDs(t, d, ctx)
	before := ontologyFocusEdges(t, d, ctx)

	setConceptBody(t, d, ctx, ids.acmeConcept, "No focus any more.")
	setConceptBody(t, d, ctx, ids.providerConcept, "@focus Runbook")
	rep := confirmFocus(t, d, ctx)
	if rep.Retired != 2 || rep.Written != 1 {
		t.Fatalf("report = %#v, want 2 retired and 1 written", rep)
	}
	after := ontologyFocusEdges(t, d, ctx)
	if len(after) != 3 {
		t.Fatalf("statements = %#v, want the two old ones kept (expired) and one new", after)
	}
	live := liveFocusEdges(after)
	if len(live) != 1 || live[ids.providerConcept] != ids.runbookClass {
		t.Errorf("live = %v, want only provider -> Runbook", live)
	}

	// Declared again: the retired statement comes back, not a second one.
	setConceptBody(t, d, ctx, ids.acmeConcept, "@focus Acme Bank")
	if rep := confirmFocus(t, d, ctx); rep.Written != 1 || rep.Retired != 0 {
		t.Errorf("re-declare report = %#v", rep)
	}
	var revived bool
	for _, e := range ontologyFocusEdges(t, d, ctx) {
		for _, b := range before {
			if e.id == b.id && e.from == ids.acmeConcept && !e.expired {
				revived = true
			}
		}
	}
	if !revived || len(ontologyFocusEdges(t, d, ctx)) != 3 {
		t.Errorf("the Acme focus was not revived in place: %#v", ontologyFocusEdges(t, d, ctx))
	}
}

// TestOntologyConfirm_UnadoptedInstanceKeepsItsStatement: a focus whose target cannot
// be found THIS time (here: the instance lost its class, so it is not adopted) is an
// error, and the statement already written is kept rather than retired.
func TestOntologyConfirm_UnadoptedInstanceKeepsItsStatement(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	confirmFocus(t, d, ctx)
	ids := focusIDs(t, d, ctx)
	read, _ := d.tenantOntologyRead(ctx)
	setConceptBody(t, d, ctx, read.Instances[0].ChunkID, "@a nonexistent-class")

	rep := confirmFocus(t, d, ctx)
	if rep.Retired != 0 || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "components/acme-work") {
		t.Errorf("report = %#v", rep)
	}
	if live := liveFocusEdges(ontologyFocusEdges(t, d, ctx)); live[ids.acmeConcept] != ids.acmeSubject {
		t.Errorf("a focus that could not be resolved this time was retired: %v", live)
	}
}

// TestOntologyConfirm_DryRunWritesNoFocus.
func TestOntologyConfirm_DryRunWritesNoFocus(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatal(err)
	}
	dry, err := d.MirrorSchemeFocus(ctx, true)
	if err != nil || dry.Written != 2 || len(dry.Stale) != 2 {
		t.Fatalf("dry run = %#v, %v", dry, err)
	}
	if n := len(ontologyFocusEdges(t, d, ctx)); n != 0 {
		t.Fatalf("a dry run wrote %d focus statements", n)
	}
	confirmFocus(t, d, ctx)
	ids := focusIDs(t, d, ctx)
	setConceptBody(t, d, ctx, ids.acmeConcept, "No focus any more.")
	dry, _ = d.MirrorSchemeFocus(ctx, true)
	if dry.Retired != 1 || len(dry.Stale) != 1 || !strings.HasSuffix(dry.Stale[0], "(retire)") {
		t.Errorf("dry run of a removal = %#v", dry)
	}
	if live := liveFocusEdges(ontologyFocusEdges(t, d, ctx)); len(live) != 2 {
		t.Errorf("a dry run retired a statement: %v", live)
	}
}

// TestSubject_FromAFactChunkIsAMember (RFC GM D24): a fact of a class a scheme projects
// is placed under a concept by a `subject` statement FROM the fact chunk. The relation
// gate admits it — with the seed `subject` kind, and with a tenant `subject` whose
// domain is the fact class and whose range is concept.
func TestSubject_FromAFactChunkIsAMember(t *testing.T) {
	for name, relations := range map[string]string{
		"seed kind":     "",
		"declared kind": "### subject\nWhat a document or fact is about.\n@domain Outage\n@range concept\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			md := strings.Replace(focusOntologyMD, "## Relations\n@section relations\n\n",
				"## Relations\n@section relations\n\n"+relations, 1)
			ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
			ids := focusIDs(t, d, ctx)
			doc, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Incidents"}`)
			if r.IsError {
				t.Fatal(r.Text)
			}
			fact, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":%q,
				"natural_key":"outage:2026-11-01","type":"Outage","subject":"November outage","title":"The provider timed out for an hour",
				"body":"The provider timed out for an hour."}`, doc["document_id"]))
			if r.IsError {
				t.Fatal(r.Text)
			}
			factID := fact["id"].(string)
			key := mustTenantKey(t, d, ctx)
			if g := d.gateRelation(ctx, key, "subject", factID, ids.providerConcept, true); !g.Valid || g.Refuse != nil {
				t.Fatalf("the gate refused a fact's subject statement: %v", g.Warnings)
			}
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"subject"}`,
				factID, ids.providerConcept))
			if r.IsError {
				t.Fatalf("link_chunks subject from a fact: %s", refusalText(r))
			}
			if out["status"] != edgeStatusAsserted {
				t.Errorf("status = %v", out["status"])
			}
			st, err := d.statementByID(ctx, key, out["id"].(string))
			if err != nil || st == nil {
				t.Fatalf("statement: %v", err)
			}
			if p := d.characteristicProblem(ctx, key, st); p != nil {
				t.Errorf("characteristicProblem = %#v", p)
			}
			// The membership the rest of RFC GM counts: the fact is under the concept.
			res, err := d.query(ctx, key, `SELECT count(*) FROM chunk_edges WHERE kind = 'subject' AND to_id = ? AND from_id = ?
				AND expired_at IS NULL`, ids.providerConcept, factID)
			if err != nil || asInt(res.Rows[0][0]) != 1 {
				t.Errorf("membership rows = %v, %v", res.Rows, err)
			}
		})
	}
}

// TestOntologySchemes_FocusConfirmOnPostgres runs the focus mirror on the postgres tier:
// written, idempotent, retired and revived.
func TestOntologySchemes_FocusConfirmOnPostgres(t *testing.T) {
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
	base := tools.WithAgentName(context.Background(), "pg-focus")
	base = tools.WithRunIdentity(base, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)

	if dry, err := d.MirrorSchemeFocus(ctx, true); err != nil || dry.Written != 1 {
		// Acme Bank is not adopted yet, so only the class focus would be written.
		t.Fatalf("dry run on postgres = %#v, %v", dry, err)
	}
	if n := len(ontologyFocusEdges(t, d, ctx)); n != 0 {
		t.Fatalf("a dry run on postgres wrote %d statements", n)
	}
	if rep := confirmFocus(t, d, ctx); rep.Written != 2 {
		t.Fatalf("confirm on postgres = %#v", rep)
	}
	if rep := confirmFocus(t, d, ctx); rep.Written != 0 || rep.Retired != 0 {
		t.Errorf("reconfirm on postgres = %#v", rep)
	}
	ids := focusIDs(t, d, ctx)
	setConceptBody(t, d, ctx, ids.acmeConcept, "No focus any more.")
	if rep := confirmFocus(t, d, ctx); rep.Retired != 1 {
		t.Errorf("removal on postgres = %#v", rep)
	}
	setConceptBody(t, d, ctx, ids.acmeConcept, "@focus Acme Bank")
	if rep := confirmFocus(t, d, ctx); rep.Written != 1 {
		t.Errorf("re-declare on postgres = %#v", rep)
	}
	if edges := ontologyFocusEdges(t, d, ctx); len(edges) != 2 || len(liveFocusEdges(edges)) != 2 {
		t.Errorf("statements on postgres = %#v", edges)
	}
}
