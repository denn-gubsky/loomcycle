package builtin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// relationsOntologyMD declares classes (with a subclass, an anchor grammar) and a
// Relations section exercising every rule the gate enforces.
const relationsOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Policy\n\n" +
	"### Provision\n@anchor_grammar art\n- `anchor` anchor\n\n" +
	"#### grammar:art\npattern: ^Article (?P<n>\\d+)\nresolve: anchor=art{n}\n\n" +
	"## Relations\n@section relations\n\n" +
	"### references\n@inverse hijacked\n\n" +
	"#### cross_refers\nA cross-reference inside the rulebook.\n\n" +
	"### stricter_than\nSets a stricter version of the same requirement.\n" +
	"@domain Provision\n@range Provision\n@inverse looser_than\n@asymmetric\n" +
	"@qualifiers aspect(security|privacy)\n@discriminator aspect\n\n" +
	"### supervises\n@functional\n@irreflexive\n@renamed_from oversees\n@auto_assert\n@walk\n\n" +
	"### governs\n@domain Document\n"

type relFixture struct {
	d                     *Document
	ctx                   context.Context
	docID                 string
	provA, provB, untyped string
	policyRoot            string
}

func newRelFixture(t *testing.T, md string) relFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	f := relFixture{d: d, ctx: ctx}
	f.docID, _ = newUserDoc(t, d, ctx)
	mk := func(title, typ string) string {
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"type":%q,"body":"x"}`, f.docID, title, typ))
		if r.IsError {
			t.Fatalf("create %s: %s", title, r.Text)
		}
		return out["id"].(string)
	}
	f.provA, f.provB, f.untyped = mk("Article 1", "Provision"), mk("Article 2", "Provision"), mk("note", "")
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Outsourcing policy","type":"Policy"}`)
	if r.IsError {
		t.Fatalf("policy doc: %s", r.Text)
	}
	f.policyRoot = out["root_chunk_id"].(string)
	return f
}

func (f relFixture) link(t *testing.T, kind, from, to, extra string) (map[string]any, bool, string) {
	t.Helper()
	out, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":%q%s}`, from, to, kind, extra))
	return out, r.IsError, refusalText(r)
}

// refusalText is a refusal's message and its fix hint together: the hint travels in the
// result's error description, not in its text.
func refusalText(r tools.Result) string {
	if r.Error != nil && r.Error.Description != "" {
		return r.Text + " " + r.Error.Description
	}
	return r.Text
}

func (f relFixture) propose(t *testing.T, kind, from, to string) map[string]any {
	t.Helper()
	out, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":%q}`, from, to, kind))
	if r.IsError {
		t.Fatalf("propose_statement %s: %s", kind, r.Text)
	}
	return out
}

// TestRelationGate_WithoutARelationsSectionNothingChanges (RFC FO §8 P1b gate).
//
// A confirmed ontology with classes but no Relations section — and a draft one with a
// Relations section — leave every hook a no-op: any kind links, proposals are valid,
// filters do not widen, walks cover every kind, no inverse is named.
func TestRelationGate_WithoutARelationsSectionNothingChanges(t *testing.T) {
	for name, tc := range map[string]struct{ md, status string }{
		"classes only":    {"# Tenant Ontology\n\n## Classes\n@section classes\n\n### Provision\n", memrank.OntologyConfirmedStatus},
		"draft relations": {relationsOntologyMD, memrank.OntologyDraftStatus},
	} {
		t.Run(name, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			ctx := installTenantOntology(t, d, base, tc.md, tc.status)
			docID, _ := newUserDoc(t, d, ctx)
			a, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"a","body":"x"}`, docID))
			b, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"b","body":"x"}`, docID))
			if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"whatever"}`, a["id"], b["id"])); r.IsError {
				t.Errorf("a free-text kind was refused: %s", r.Text)
			}
			out, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"anything"}`, a["id"], b["id"]))
			if out["ontology_valid"] != true {
				t.Errorf("a proposal was annotated invalid: %v", out)
			}
			if d.walkKinds(ctx) != nil || d.inverseKindName(ctx, "cites") != "" || d.autoAssertKind(ctx, "supervises") {
				t.Error("a walk set, inverse or auto-assert appeared without a confirmed Relations section")
			}
			if got, added := d.expandKindFilter(ctx, []string{"references"}); len(got) != 1 || added != nil {
				t.Errorf("kind filter widened: %v %v", got, added)
			}
		})
	}
}

// TestRelationGate_UndeclaredKindIsRefusedWhenLinkedAndAnnotatedWhenProposed (D8).
func TestRelationGate_UndeclaredKindIsRefusedWhenLinkedAndAnnotatedWhenProposed(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	_, refused, text := f.link(t, "blocks", f.provA, f.provB, "")
	if !refused || !strings.Contains(text, "kind_not_declared") {
		t.Fatalf("an undeclared kind was not refused: %s", text)
	}
	if p := f.propose(t, "blocks", f.provA, f.provB); p["ontology_valid"] != false {
		t.Errorf("an extracted undeclared kind should land annotated: %v", p)
	}
	if _, _, text = f.link(t, "looser_than", f.provA, f.provB, ""); !strings.Contains(text, "other end") {
		t.Errorf("an inverse name should point at the declared kind: %s", text)
	}
	if _, _, text = f.link(t, "oversees", f.provA, f.provB, ""); !strings.Contains(text, "renamed to supervises") {
		t.Errorf("an old name should point at the new one: %s", text)
	}
	if _, refused, text = f.link(t, "cites", f.provA, f.provB, ""); refused {
		t.Errorf("a seed kind should be valid once a Relations section is declared: %s", text)
	}
}

// TestRelationGate_DomainAndRangeAreClosedWorldUnderTheClassTree.
func TestRelationGate_DomainAndRangeAreClosedWorldUnderTheClassTree(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	if _, refused, text := f.link(t, "stricter_than", f.provA, f.untyped, ""); !refused || !strings.Contains(text, "domain_range_violation") {
		t.Errorf("an untyped target was accepted for a Provision range: %s", text)
	}
	if _, refused, text := f.link(t, "stricter_than", f.provA, f.provB, ""); refused {
		t.Errorf("Provision → Provision refused: %s", text)
	}
	// Policy is a subclass of Document, so a Policy root satisfies @domain Document.
	if _, refused, text := f.link(t, "governs", f.policyRoot, f.provA, ""); refused {
		t.Errorf("a subclass did not satisfy its parent's domain: %s", text)
	}
	if _, refused, _ := f.link(t, "governs", f.provA, f.provB, ""); !refused {
		t.Error("a Provision satisfied @domain Document")
	}
	if p := f.propose(t, "stricter_than", f.untyped, f.provA); p["ontology_valid"] != false {
		t.Errorf("an extracted domain violation should land annotated: %v", p)
	}
}

// TestRelationGate_CharacteristicsHoldAndAReLinkIsIdempotent.
func TestRelationGate_CharacteristicsHoldAndAReLinkIsIdempotent(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	if _, refused, text := f.link(t, "supervises", f.provA, f.provA, ""); !refused || !strings.Contains(text, "characteristic_violation") {
		t.Errorf("an irreflexive self-link was accepted: %s", text)
	}
	if _, refused, text := f.link(t, "supervises", f.provA, f.provB, ""); refused {
		t.Fatalf("first functional link refused: %s", text)
	}
	if _, refused, text := f.link(t, "supervises", f.provA, f.provB, ""); refused {
		t.Errorf("re-linking the same statement tripped a rule against itself: %s", text)
	}
	if _, refused, _ := f.link(t, "supervises", f.provA, f.untyped, ""); !refused {
		t.Error("a functional kind accepted a second target")
	}
	if _, refused, text := f.link(t, "stricter_than", f.provA, f.provB, ""); refused {
		t.Fatalf("stricter_than A→B refused: %s", text)
	}
	if _, refused, text := f.link(t, "stricter_than", f.provB, f.provA, ""); !refused || !strings.Contains(text, "asymmetric") {
		t.Errorf("an asymmetric kind accepted the reverse statement: %s", text)
	}
}

// TestRelationGate_ASyncedStatementsOriginMethodIsNotAQualifier: sync records a pulled
// statement's original method in attributes.origin_method and push sends it through the
// peer's link_chunks. It is the statement layer's bookkeeping; read as a qualifier, a
// kind that declares qualifiers would refuse every pushed statement.
func TestRelationGate_ASyncedStatementsOriginMethodIsNotAQualifier(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	if _, refused, text := f.link(t, "stricter_than", f.provA, f.provB, `,"attributes":{"aspect":"security","origin_method":"extraction"}`); refused {
		t.Errorf("a pushed statement carrying origin_method was refused: %s", text)
	}
	if g := f.d.gateAttributes(f.ctx, "stricter_than", map[string]any{"aspect": "security", "origin_method": "extraction"}, false); !g.Valid || len(g.Warnings) != 0 {
		t.Errorf("a pulled statement carrying origin_method was annotated: valid %v, %v", g.Valid, g.Warnings)
	}
}

// TestRelationGate_QualifiersAndTheDiscriminator.
func TestRelationGate_QualifiersAndTheDiscriminator(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	if _, refused, text := f.link(t, "stricter_than", f.provA, f.provB, `,"attributes":{"aspect":"finance"}`); !refused || !strings.Contains(text, "qualifier_violation") {
		t.Errorf("a value outside the enum was accepted: %s", text)
	}
	if _, refused, _ := f.link(t, "stricter_than", f.provA, f.provB, `,"attributes":{"colour":"red"}`); !refused {
		t.Error("an undeclared qualifier was accepted")
	}
	if g := f.d.gateAttributes(f.ctx, "stricter_than", map[string]any{"aspect": "security", "revision_of": "x", "ontology_valid": false}, true); !g.Valid {
		t.Errorf("declared value plus statement-layer keys should pass: %v", g.Warnings)
	}
	if got := f.d.discriminatorFor(f.ctx, "stricter_than", map[string]any{"aspect": "Privacy"}); got != "Privacy" {
		t.Errorf("discriminator = %q", got)
	}
	if f.d.discriminatorFor(f.ctx, "supervises", map[string]any{"aspect": "x"}) != "" {
		t.Error("a kind with no @discriminator produced one")
	}
	// Two statements of one triple that differ in the discriminator coexist.
	for _, aspect := range []string{"security", "privacy"} {
		if _, refused, text := f.link(t, "stricter_than", f.provA, f.provB, `,"attributes":{"aspect":"`+aspect+`"}`); refused {
			t.Fatalf("aspect %s refused: %s", aspect, text)
		}
	}
	key := sidecarScope(t, f.d, f.ctx)
	res, err := f.d.query(f.ctx, key, `SELECT count(*) FROM chunk_edges WHERE kind = 'stricter_than' AND discriminator <> ''`)
	if err != nil || asInt(res.Rows[0][0]) != 2 {
		t.Errorf("want two discriminated statements: %v %v", res, err)
	}
}

// TestRelationGate_ReservedKindsAreFixed (RFC FO 10d.1): a tenant entry named after a
// reserved kind changes nothing about it, but may anchor sub-kinds.
func TestRelationGate_ReservedKindsAreFixed(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	key := sidecarScope(t, f.d, f.ctx)
	if g := f.d.gateRelation(f.ctx, key, "references", f.untyped, f.untyped, true); !g.Valid || g.Refuse != nil {
		t.Errorf("a reserved kind did not pass: %v", g.Warnings)
	}
	if inv := f.d.inverseKindName(f.ctx, "references"); inv != "referenced_by" {
		t.Errorf("the tenant redefined a reserved kind's inverse: %q", inv)
	}
	got, added := f.d.expandKindFilter(f.ctx, []string{"references"})
	for _, want := range []string{"cites", "based_on", "cross_refers"} {
		if !containsStr(got, want) || !containsStr(added, want) {
			t.Errorf("references should expand to %s: %v / %v", want, got, added)
		}
	}
}

// TestRelationGate_WalkInverseAndAutoAssert.
func TestRelationGate_WalkInverseAndAutoAssert(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	walk := f.d.walkKinds(f.ctx)
	for _, want := range []string{"cites", "supervises", "supersedes"} {
		if !containsStr(walk, want) {
			t.Errorf("walk set %v lacks %s", walk, want)
		}
	}
	if f.d.inverseKindName(f.ctx, "stricter_than") != "looser_than" || f.d.inverseKindName(f.ctx, "conflicts_with") != "conflicts_with" {
		t.Error("declared or symmetric inverse not named")
	}
	if !f.d.autoAssertKind(f.ctx, "supervises") || f.d.autoAssertKind(f.ctx, "stricter_than") {
		t.Error("auto-assert misreported")
	}
}

// TestOntologyCache_ReusesTheParseUntilTheOntologyChanges (RFC FO 10d.4).
func TestOntologyCache_ReusesTheParseUntilTheOntologyChanges(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	r1, _ := f.d.tenantOntologyRead(f.ctx)
	r2, _ := f.d.tenantOntologyRead(f.ctx)
	if len(r1.Terms) == 0 || &r1.Terms[0] != &r2.Terms[0] {
		t.Fatal("an unchanged ontology was parsed again")
	}
	// Edit one class body: the next read must see it.
	var provChunk string
	for _, tm := range r1.Terms {
		if tm.Name == "Provision" {
			provChunk = chunkIDByTitle(t, f, "Provision")
		}
	}
	g, _ := docExec(t, f.d, f.ctx, `{"op":"get_chunk","scope":"tenant","id":"`+provChunk+`"}`)
	if _, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"body":"@anchor_grammar art\n- `+"`anchor`"+` anchor\n- `+"`number`"+` number\n"}`,
		provChunk, int(g["revision"].(float64)))); r.IsError {
		t.Fatalf("edit Provision: %s", r.Text)
	}
	r3, _ := f.d.tenantOntologyRead(f.ctx)
	for _, tm := range r3.Terms {
		if tm.Name == "Provision" && !containsStr(tm.Fields, "number") {
			t.Errorf("a body edit was served from the stale cache: %v", tm.Fields)
		}
	}
	// Adding a relation kind is seen too.
	sec := r3.SectionChunks[memrank.SectionRelations]
	if _, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"mentors","body":"x"}`, r3.DocumentID, sec)); r.IsError {
		t.Fatalf("add kind: %s", r.Text)
	}
	r4, _ := f.d.tenantOntologyRead(f.ctx)
	found := false
	for _, rel := range r4.Relations {
		found = found || rel.Name == "mentors"
	}
	if !found {
		t.Error("a new relation kind was served from the stale cache")
	}
}

// TestProposeEntity_RelationKinds.
func TestProposeEntity_RelationKinds(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	out, r := docExec(t, f.d, f.ctx, `{"op":"propose_entity","scope":"user","section":"relations","name":"mentors","parent":"supervises","body":"evidence"}`)
	if r.IsError {
		t.Fatalf("propose relation: %s", r.Text)
	}
	read, _ := f.d.tenantOntologyRead(f.ctx)
	var prop *memrank.OntologyProposal
	for i := range read.Proposals {
		if read.Proposals[i].Name == "mentors" {
			prop = &read.Proposals[i]
		}
	}
	if prop == nil || prop.Section != memrank.SectionRelations || prop.Parent != "supervises" {
		t.Fatalf("proposal not filed in the Relations section under supervises: %#v (out %v)", prop, out)
	}
	for name, body := range map[string]string{
		"duplicate":  `{"op":"propose_entity","scope":"user","section":"relations","name":"mentors"}`,
		"seed":       `{"op":"propose_entity","scope":"user","section":"relations","name":"cites"}`,
		"reserved":   `{"op":"propose_entity","scope":"user","section":"relations","name":"about"}`,
		"declared":   `{"op":"propose_entity","scope":"user","section":"relations","name":"supervises"}`,
		"bad parent": `{"op":"propose_entity","scope":"user","section":"relations","name":"x_rel","parent":"nope"}`,
		"later":      `{"op":"propose_entity","scope":"user","section":"schemes","name":"topic"}`,
	} {
		if _, r := docExec(t, f.d, f.ctx, body); !r.IsError {
			t.Errorf("%s: should be refused", name)
		}
	}
	// A class proposal in a sectioned document lands inside the classes section.
	if _, r := docExec(t, f.d, f.ctx, `{"op":"propose_entity","scope":"user","name":"Memo"}`); r.IsError {
		t.Fatalf("propose class: %s", r.Text)
	}
	read, _ = f.d.tenantOntologyRead(f.ctx)
	for _, p := range read.Proposals {
		if p.Name == "Memo" && p.Section != "" {
			t.Errorf("a class proposal was filed as %q", p.Section)
		}
	}
}

// TestProposeEntity_RelationNeedsADeclaredSection.
func TestProposeEntity_RelationNeedsADeclaredSection(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, "# Tenant Ontology\n\n## Classes\n@section classes\n\n### Provision\n", memrank.OntologyConfirmedStatus)
	if _, r := docExec(t, d, ctx, `{"op":"propose_entity","scope":"user","section":"relations","name":"mentors"}`); !r.IsError || !strings.Contains(r.Text, "no Relations section") {
		t.Errorf("a relation proposal without a Relations section: %s", r.Text)
	}
}

// TestAnchorGate_ClassGrammarKeysParts.
func TestAnchorGate_ClassGrammarKeysParts(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	key := sidecarScope(t, f.d, f.ctx)
	out, r := docExec(t, f.d, f.ctx, `{"op":"create_document","scope":"user","title":"Rulebook","type":"Provision"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if g := f.d.anchorGrammarFor(f.ctx, key, out["root_chunk_id"].(string)); g != "art" {
		t.Fatalf("anchorGrammarFor = %q, want art", g)
	}
	if g := f.d.anchorGrammarFor(f.ctx, key, f.policyRoot); g != "" {
		t.Errorf("a class with no grammar returned %q", g)
	}
	if a := f.d.anchorFor(f.ctx, "art", []string{"Chapter 1", "Article 12"}); a != "art12" {
		t.Errorf("anchorFor = %q, want art12", a)
	}
	if a := f.d.anchorFor(f.ctx, "art", []string{"Preamble"}); a != "" {
		t.Errorf("a non-matching heading produced %q", a)
	}
	if a := f.d.anchorFor(f.ctx, "missing", []string{"Article 1"}); a != "" {
		t.Errorf("an unknown grammar produced %q", a)
	}
}

// TestClassRules_CollisionWithAPlaceholderSaysToFillIt.
func TestClassRules_CollisionWithAPlaceholderSaysToFillIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, typedOntologyMD, memrank.OntologyConfirmedStatus)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Outsourcing Policy","type":"Policy"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	root := out["root_chunk_id"].(string)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+root+`"}`)
	if _, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%d,"status":"unresolved"}`, root, int(g["revision"].(float64)))); r.IsError {
		t.Fatal(r.Text)
	}
	_, r = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"outsourcing policy","type":"Policy"}`)
	if txt := refusalText(r); !r.IsError || !strings.Contains(txt, "unresolved placeholder") || !strings.Contains(txt, "reimport:true") {
		t.Errorf("the refusal should say to fill the placeholder: %s", txt)
	}

	// A placeholder filed by resolve_anchor holds its key as a WORK key, not a class
	// identity; creating the work directly must still meet it, not make a second document.
	key, _, _ := d.resolveScope(ctx, "user")
	res, err := d.query(ctx, key, `SELECT identity_key FROM chunks WHERE id = ?`, root)
	if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) == "" {
		t.Fatalf("the typed root has no identity key: %v %v", res, err)
	}
	idKey := asStr(res.Rows[0][0])
	if _, r = docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+out["document_id"].(string)+`"}`); r.IsError {
		t.Fatal(r.Text)
	}
	ph, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"resolve_anchor","scope":"user","work_key":%q,"create_placeholder":true}`, idKey))
	if r.IsError || ph["placeholder"] != true {
		t.Fatalf("placeholder: %s", r.Text)
	}
	if res, err = d.query(ctx, key, `SELECT COALESCE(work_key, '') FROM chunks WHERE id = ?`, ph["root_chunk_id"]); err != nil || asStr(res.Rows[0][0]) != idKey {
		t.Fatalf("the placeholder does not hold its key as a work key: %v %v", res, err)
	}
	_, r = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"outsourcing policy","type":"Policy"}`)
	if txt := refusalText(r); !r.IsError || !strings.Contains(txt, "unresolved placeholder") {
		t.Errorf("creating the work beside its placeholder should be refused: %s", txt)
	}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func chunkIDByTitle(t *testing.T, f relFixture, title string) string {
	t.Helper()
	read, _ := f.d.tenantOntologyRead(f.ctx)
	res, err := f.d.query(f.ctx, mustTenantKey(t, f.d, f.ctx), `SELECT id FROM chunks WHERE document_id = ? AND title = ? LIMIT 1`, read.DocumentID, title)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("chunk %q: %v", title, err)
	}
	return asStr(res.Rows[0][0])
}

// failingBodyStore fails chunk-body reads while fail is set — a Memory fault, or a read
// cut short by a cancelled request, in the middle of the ontology walk.
type failingBodyStore struct {
	store.Store
	fail bool
}

func (s *failingBodyStore) MemoryGet(ctx context.Context, tenant string, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	if s.fail && strings.HasPrefix(key, "doc.chunk:") {
		return store.MemoryEntry{}, errors.New("transient memory fault")
	}
	return s.Store.MemoryGet(ctx, tenant, scope, scopeID, key)
}

// TestOntologyCache_APartialReadIsNeverCached.
//
// The walk reads every chunk body and treats a failed read as an empty body — before the
// cache that was a one-request blip. Cached, it would strip every class of its fields and
// every relation of its constraints until someone happened to edit the ontology: the
// gate weakened by a transient fault. A read with any failed sub-read is returned to its
// caller but never stored.
func TestOntologyCache_APartialReadIsNeverCached(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	fs := &failingBodyStore{Store: f.d.Store}
	d2 := &Document{Store: fs, SqlMem: f.d.SqlMem, Bus: f.d.Bus}
	// Change the ontology so the next read is a fresh build, not the fixture's cached one.
	read, _ := d2.tenantOntologyRead(f.ctx)
	sec := read.SectionChunks[memrank.SectionRelations]
	if _, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"mentors","body":"x"}`, read.DocumentID, sec)); r.IsError {
		t.Fatal(r.Text)
	}

	fs.fail = true
	partial, ok := d2.tenantOntologyRead(f.ctx)
	if !ok {
		t.Fatal("a partial read should still be returned")
	}
	if hasConstrainedStricter(partial) {
		t.Fatal("the fault did not reach the walk; the test proves nothing")
	}

	fs.fail = false
	healed, _ := d2.tenantOntologyRead(f.ctx)
	if !hasConstrainedStricter(healed) {
		t.Error("a read cut short by a fault was cached: stricter_than (with its domain) is missing until the next edit")
	}
}

// hasConstrainedStricter reports whether a read carries stricter_than WITH its domain —
// present and complete, not merely absent-and-therefore-unchecked.
func hasConstrainedStricter(r OntologyRead) bool {
	for _, rel := range r.Relations {
		if rel.Name == "stricter_than" && len(rel.Domain) > 0 {
			return true
		}
	}
	return false
}

// TestOntologyRelations_GateAndCacheOnPostgres runs the relation gate and the ontology
// cache on the postgres tier: the edge queries (asymmetric / functional through the
// reader's visibility rule) and the fingerprint's aggregates are where the two SQL tiers
// could disagree.
func TestOntologyRelations_GateAndCacheOnPostgres(t *testing.T) {
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
	base := tools.WithAgentName(context.Background(), "pg-relations")
	base = tools.WithRunIdentity(base, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	ctx := installTenantOntology(t, d, base, relationsOntologyMD, memrank.OntologyConfirmedStatus)
	docID, _ := newUserDoc(t, d, ctx)
	mk := func(title, typ string) string {
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"type":%q,"body":"x"}`, docID, title, typ))
		if r.IsError {
			t.Fatalf("create %s: %s", title, r.Text)
		}
		return out["id"].(string)
	}
	a, b, c := mk("Article 1", "Provision"), mk("Article 2", "Provision"), mk("note", "")
	link := func(kind, from, to string) tools.Result {
		_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":%q}`, from, to, kind))
		return r
	}
	if r := link("blocks", a, b); !r.IsError || !strings.Contains(r.Text, "kind_not_declared") {
		t.Errorf("undeclared kind on postgres: %s", r.Text)
	}
	if r := link("stricter_than", a, c); !r.IsError || !strings.Contains(r.Text, "domain_range_violation") {
		t.Errorf("domain/range on postgres: %s", r.Text)
	}
	if r := link("supervises", a, b); r.IsError {
		t.Fatalf("functional first link on postgres: %s", r.Text)
	}
	if r := link("supervises", a, c); !r.IsError || !strings.Contains(r.Text, "characteristic_violation") {
		t.Errorf("functional second target on postgres: %s", r.Text)
	}
	if r := link("stricter_than", a, b); r.IsError {
		t.Fatalf("stricter_than on postgres: %s", r.Text)
	}
	if r := link("stricter_than", b, a); !r.IsError {
		t.Error("asymmetric reverse accepted on postgres")
	}
	r1, _ := d.tenantOntologyRead(ctx)
	r2, _ := d.tenantOntologyRead(ctx)
	if len(r1.Terms) == 0 || &r1.Terms[0] != &r2.Terms[0] {
		t.Error("the cache did not hold an unchanged ontology on postgres")
	}
	sec := r2.SectionChunks[memrank.SectionRelations]
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"mentors","body":"x"}`, r2.DocumentID, sec)); r.IsError {
		t.Fatal(r.Text)
	}
	r3, _ := d.tenantOntologyRead(ctx)
	found := false
	for _, rel := range r3.Relations {
		found = found || rel.Name == "mentors"
	}
	if !found {
		t.Error("a new kind was served from a stale cache on postgres")
	}
}
