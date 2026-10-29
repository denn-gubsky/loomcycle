package builtin

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

const schemesOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\n- `anchor` anchor\n\n" +
	"## Relations\n@section relations\n\n### filed_under\n@domain Provision\n@range concept\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@render\n\n#### Risk Management\n@notation RM\n\n##### outsourcing\n@alt аутсорсинг\n\n" +
	"### file-plan\n\n#### compliance\n@retention 10y\n@acl compliance\n\n##### reporting\n@retention 5y\n\n" +
	"### rank\n@ordered\n\n#### Law\n\n#### Policy\n"

func schemeConcepts(r OntologyRead) map[string]memrank.OntologyConcept {
	out := map[string]memrank.OntologyConcept{}
	for _, s := range r.Schemes {
		for _, c := range s.Concepts {
			out[c.Tag] = c
		}
	}
	return out
}

// TestOntologyTree_SchemesSectionReadsConceptsNotClasses (RFC FO P1c).
func TestOntologyTree_SchemesSectionReadsConceptsNotClasses(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, schemesOntologyMD, memrank.OntologyConfirmedStatus)
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || len(read.Schemes) != 3 {
		t.Fatalf("schemes = %#v", read.Schemes)
	}
	for _, tm := range read.Terms {
		if tm.Name == "outsourcing" || tm.Name == "topics" || tm.Name == "Law" {
			t.Errorf("a concept or scheme was read as a class: %s", tm.Name)
		}
	}
	by := schemeConcepts(read)
	out := by["topics/risk-management/outsourcing"]
	if out.Parent != "Risk Management" || len(out.Alt) != 1 || out.ChunkID == "" {
		t.Errorf("outsourcing = %#v", out)
	}
	if by["topics/risk-management"].Notation != "RM" {
		t.Errorf("notation not read: %#v", by["topics/risk-management"])
	}
	if c := by["file-plan/compliance/reporting"]; c.Retention != "5y" || c.ACL != "" {
		t.Errorf("reporting = %#v", c)
	}
	if by["rank/law"].Rank != 1 || by["rank/policy"].Rank != 2 || by["topics/risk-management"].Rank != 0 {
		t.Error("ranks should follow document order only in an @ordered scheme")
	}
}

// TestMirrorSchemeTags_IsIdempotentAndFollowsARename (RFC FO §8 P1c gate).
//
// The mirror gives each concept chunk its tag, a second run changes nothing, and a
// renamed concept loses its old mirror tag — but a concept chunk's other tags, and every
// tag on an ordinary document, are left alone.
func TestMirrorSchemeTags_IsIdempotentAndFollowsARename(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, schemesOntologyMD, memrank.OntologyConfirmedStatus)
	key := mustTenantKey(t, d, ctx)

	dry, err := d.MirrorSchemeTags(ctx, true)
	if err != nil || dry.Concepts != 6 || len(dry.Stale) != 6 {
		t.Fatalf("dry run = %#v, %v", dry, err)
	}
	read, _ := d.tenantOntologyRead(ctx)
	out := schemeConcepts(read)["topics/risk-management/outsourcing"]
	if tags, _ := d.listChunkTags(ctx, key, out.ChunkID); len(tags) != 0 {
		t.Fatalf("a dry run wrote tags: %v", tags)
	}

	// An operator's own tag on a concept chunk, and a document carrying a concept tag.
	if err := d.replaceChunkTags(ctx, key, out.ChunkID, []string{"keep-me"}); err != nil {
		t.Fatal(err)
	}
	docOut, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Vendor policy","tags":["topics/risk-management/outsourcing"]}`)
	if r.IsError {
		t.Fatal(r.Text)
	}

	if rep, err := d.MirrorSchemeTags(ctx, false); err != nil || rep.Added != 6 {
		t.Fatalf("mirror = %#v, %v", rep, err)
	}
	if rep, _ := d.MirrorSchemeTags(ctx, false); len(rep.Stale) != 0 {
		t.Errorf("a second run changed tags: %#v", rep)
	}
	tags, _ := d.listChunkTags(ctx, key, out.ChunkID)
	sort.Strings(tags)
	if strings.Join(tags, ",") != "keep-me,topics/risk-management/outsourcing" {
		t.Errorf("concept tags after mirror = %v", tags)
	}

	// Rename the concept: the next mirror replaces the old tag, keeps keep-me.
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+out.ChunkID+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"title":"Vendor Outsourcing"}`,
		out.ChunkID, int(g["revision"].(float64)))); r.IsError {
		t.Fatal(r.Text)
	}
	if rep, _ := d.MirrorSchemeTags(ctx, false); rep.Removed != 1 || rep.Added != 1 {
		t.Errorf("rename mirror = %#v", rep)
	}
	tags, _ = d.listChunkTags(ctx, key, out.ChunkID)
	sort.Strings(tags)
	if strings.Join(tags, ",") != "keep-me,topics/risk-management/vendor-outsourcing" {
		t.Errorf("concept tags after rename = %v", tags)
	}
	// The document keeps the classification it was given; the mirror never re-files it.
	dg, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+docOut["document_id"].(string)+`"}`)
	if fmt.Sprint(dg["tags"]) != "[topics/risk-management/outsourcing]" {
		t.Errorf("the mirror touched a document's tags: %v", dg["tags"])
	}
}

// TestMirrorSchemeTags_RefusesAPartialRead: an unreadable section body hides the whole
// section, and a mirror run on that read would strip every concept's tag.
func TestMirrorSchemeTags_RefusesAPartialRead(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, schemesOntologyMD, memrank.OntologyConfirmedStatus)
	fs := &failingBodyStore{Store: d.Store, fail: true}
	d2 := &Document{Store: fs, SqlMem: d.SqlMem, Bus: d.Bus}
	if _, err := d2.MirrorSchemeTags(ctx, false); err == nil {
		t.Error("a mirror run on a partial read was not refused")
	}
}

// TestRelationGate_ConceptRangeIsATenantConceptChunk.
func TestRelationGate_ConceptRangeIsATenantConceptChunk(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, schemesOntologyMD, memrank.OntologyConfirmedStatus)
	read, _ := d.tenantOntologyRead(ctx)
	concept := schemeConcepts(read)["file-plan/compliance/reporting"].ChunkID

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Returns 2026","type":"Provision"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	prov := out["root_chunk_id"].(string)
	plain, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"note","body":"x"}`, out["document_id"]))
	link := func(scope, from, to string) (bool, string) {
		_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":%q,"from_id":%q,"to_id":%q,"kind":"filed_under"}`, scope, from, to))
		return r.IsError, refusalText(r)
	}
	if refused, text := link("tenant", prov, concept); refused {
		t.Errorf("a link to a concept chunk was refused: %s", text)
	}
	if refused, text := link("tenant", prov, plain["id"].(string)); !refused || !strings.Contains(text, "tag it with the concept's tag") {
		t.Errorf("a non-concept target should be refused with the tag hint: %s", text)
	}
	// In a user scope there are no concept chunks: the range cannot be met there.
	udoc, _ := newUserDoc(t, d, ctx)
	ua, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"a","type":"Provision","body":"x"}`, udoc))
	ub, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"b","body":"x"}`, udoc))
	if refused, _ := link("user", ua["id"].(string), ub["id"].(string)); !refused {
		t.Error("a concept range was satisfied by a user-scope chunk")
	}
}

// TestProposeEntity_Concepts.
func TestProposeEntity_Concepts(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, schemesOntologyMD, memrank.OntologyConfirmedStatus)
	if _, r := docExec(t, d, ctx, `{"op":"propose_entity","scope":"user","section":"schemes","name":"Cloud Outsourcing","parent":"topics/risk-management/outsourcing","body":"evidence"}`); r.IsError {
		t.Fatalf("propose concept: %s", refusalText(r))
	}
	read, _ := d.tenantOntologyRead(ctx)
	found := false
	for _, p := range read.Proposals {
		found = found || (p.Name == "Cloud Outsourcing" && p.Section == memrank.SectionSchemes && p.Parent == "outsourcing")
	}
	if !found {
		t.Errorf("concept proposal not filed under outsourcing: %#v", read.Proposals)
	}
	for name, body := range map[string]string{
		"existing":   `{"op":"propose_entity","scope":"user","section":"schemes","name":"outsourcing","parent":"Risk Management"}`,
		"no parent":  `{"op":"propose_entity","scope":"user","section":"schemes","name":"x"}`,
		"bad parent": `{"op":"propose_entity","scope":"user","section":"schemes","name":"x","parent":"nope"}`,
	} {
		if _, r := docExec(t, d, ctx, body); !r.IsError {
			t.Errorf("%s: should be refused", name)
		}
	}
	d2, base2, _ := documentFixture(t)
	ctx2 := installTenantOntology(t, d2, base2, "# Tenant Ontology\n\n## Classes\n@section classes\n\n### Provision\n", memrank.OntologyConfirmedStatus)
	if _, r := docExec(t, d2, ctx2, `{"op":"propose_entity","scope":"user","section":"schemes","name":"x","parent":"topics"}`); !r.IsError || !strings.Contains(r.Text, "no Schemes section") {
		t.Errorf("a concept proposal without a Schemes section: %s", r.Text)
	}
}
