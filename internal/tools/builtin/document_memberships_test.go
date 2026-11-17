package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memberships runs op=memberships as ctx with the extra arguments and decodes it.
func (f *memberFixture) memberships(ctx context.Context, args string) membershipsAnswer {
	f.t.Helper()
	_, r := docExec(f.t, f.d, ctx, `{"op":"memberships","scope":"tenant",`+args+`}`)
	if r.IsError {
		f.t.Fatalf("memberships %s: %s", args, r.Text)
	}
	var out membershipsAnswer
	if err := json.Unmarshal([]byte(r.Text), &out); err != nil {
		f.t.Fatalf("decode: %v\n%s", err, r.Text)
	}
	return out
}

// conceptsByTag flattens an answer to tag → concept.
func conceptsByTag(a membershipsAnswer) map[string]memberConcept {
	out := map[string]memberConcept{}
	for _, tx := range a.Taxonomies {
		for _, c := range tx.Concepts {
			out[c.Tag] = c
		}
	}
	return out
}

// TestMemberships_ListsEveryTaxonomyOfAThing — a document placed in two schemes is
// listed under both, each placement with the statement that made it, beside its class
// chain and its folder.
func TestMemberships_ListsEveryTaxonomyOfAThing(t *testing.T) {
	d, base, _ := documentFixture(t)
	assertMembershipsListEveryTaxonomy(t, newMemberFixture(t, d, base))
}

func assertMembershipsListEveryTaxonomy(t *testing.T, f *memberFixture) {
	doc := f.doc("/kb/reports/q3", "Brief")
	s1 := f.place(doc, "topics/risk/outsourcing", "link_chunks")
	s2 := f.place(doc, "systems/hydraulics", "link_chunks")

	got := f.memberships(f.reader, fmt.Sprintf(`"id":%q`, doc))
	if got.DocumentID != doc || got.Scope != "tenant" {
		t.Errorf("document_id/scope = %s/%s", got.DocumentID, got.Scope)
	}
	if want := []string{"Brief", "Report", "Document"}; !reflect.DeepEqual(got.Classes, want) {
		t.Errorf("classes = %v, want %v", got.Classes, want)
	}
	var schemes []string
	for _, tx := range got.Taxonomies {
		schemes = append(schemes, tx.Scheme)
	}
	if want := []string{"topics", "systems"}; !reflect.DeepEqual(schemes, want) {
		t.Errorf("taxonomies = %v, want %v (the ontology's order)", schemes, want)
	}
	byTag := conceptsByTag(got)
	for tag, stmt := range map[string]string{"topics/risk/outsourcing": s1, "systems/hydraulics": s2} {
		c := byTag[tag]
		if len(c.Placements) != 1 {
			t.Fatalf("%s: placements = %+v", tag, c.Placements)
		}
		p := c.Placements[0]
		if p.StatementID != stmt || p.Kind != "subject" || p.Status != edgeStatusAsserted || p.Method == "" || p.ChunkID == "" {
			t.Errorf("%s: placement = %+v, want statement %s, subject, asserted, a method and a chunk", tag, p, stmt)
		}
		if c.ChunkID != f.concepts[tag] || c.Implied {
			t.Errorf("%s: concept = %+v", tag, c)
		}
	}
	if c := byTag["topics/risk/outsourcing"]; !reflect.DeepEqual(c.Path, []string{"Risk", "Outsourcing"}) || c.Name != "Outsourcing" {
		t.Errorf("outsourcing path/name = %v/%s", c.Path, c.Name)
	}
	if want := []membershipFolder{{Folder: "/kb/reports", Path: "/kb/reports/q3"}}; !reflect.DeepEqual(got.Folders, want) {
		t.Errorf("folders = %+v, want %+v", got.Folders, want)
	}
}

// TestMemberships_ImpliedAncestorsAreMarked — a placement under a narrower concept
// lists each broader concept as implied, with no placement of its own; a sibling the
// document is not under is not listed.
func TestMemberships_ImpliedAncestorsAreMarked(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	doc := f.doc("/kb/a", "Report")
	f.place(doc, "topics/risk/outsourcing", "link_chunks")
	f.place(doc, "systems/hydraulics/pumps", "link_chunks")

	byTag := conceptsByTag(f.memberships(f.reader, fmt.Sprintf(`"id":%q`, doc)))
	for _, tag := range []string{"topics/risk", "systems/hydraulics"} {
		c, ok := byTag[tag]
		if !ok || !c.Implied || len(c.Placements) != 0 {
			t.Errorf("%s = %+v (listed %v), want implied with no placement", tag, c, ok)
		}
	}
	for _, tag := range []string{"topics/risk/outsourcing", "systems/hydraulics/pumps"} {
		if c := byTag[tag]; c.Implied || len(c.Placements) != 1 {
			t.Errorf("%s = %+v, want a direct placement", tag, c)
		}
	}
	if _, ok := byTag["topics/reporting"]; ok {
		t.Errorf("topics/reporting listed though the document is not under it: %v", byTag)
	}
}

// TestMemberships_ProposedPlacementShownPerRule — as get_edges: a proposal is listed by
// default and left out with include_proposed false; it implies no broader concept;
// get_document with_memberships follows the same rule.
func TestMemberships_ProposedPlacementShownPerRule(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	doc := f.doc("/kb/p", "Report")
	proposal := f.place(doc, "topics/risk/outsourcing", "propose_statement")
	f.place(doc, "systems/hydraulics", "link_chunks")

	byTag := conceptsByTag(f.memberships(f.reader, fmt.Sprintf(`"id":%q`, doc)))
	c := byTag["topics/risk/outsourcing"]
	if len(c.Placements) != 1 || c.Placements[0].StatementID != proposal || c.Placements[0].Status != edgeStatusProposed {
		t.Errorf("default: outsourcing = %+v, want the proposal %s", c, proposal)
	}
	if _, ok := byTag["topics/risk"]; ok {
		t.Errorf("a proposal implied topics/risk: %v", byTag)
	}

	off := conceptsByTag(f.memberships(f.reader, fmt.Sprintf(`"id":%q,"include_proposed":false`, doc)))
	if _, ok := off["topics/risk/outsourcing"]; ok {
		t.Errorf("include_proposed:false still lists the proposal: %v", off)
	}
	if _, ok := off["systems/hydraulics"]; !ok {
		t.Errorf("include_proposed:false dropped a live placement: %v", off)
	}

	for args, want := range map[string]bool{``: true, `,"include_proposed":false`: false} {
		out, r := docExec(t, d, f.reader, fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q,"with_memberships":true%s}`, doc, args))
		if r.IsError {
			t.Fatalf("get_document: %s", r.Text)
		}
		listed := strings.Contains(fmt.Sprint(out["memberships"]), proposal)
		if listed != want {
			t.Errorf("get_document with_memberships%s: proposal listed = %v, want %v", args, listed, want)
		}
	}
}

// TestMemberships_UnreadableThingIsRefused — a document the caller may not read is
// refused exactly as get_document refuses it; a caller who may not read the ontology
// gets the document's own type and folders and no taxonomy.
func TestMemberships_UnreadableThingIsRefused(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	hidden := f.doc("/kb/hidden/x", "Report")
	f.place(hidden, "topics/risk", "link_chunks")
	open := f.doc("/kb/open/y", "Report")
	f.place(open, "topics/risk", "link_chunks")

	deny := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/hidden/**"}})
	})
	_, viaGet := docExec(t, d, deny, fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, hidden))
	_, viaMem := docExec(t, d, deny, fmt.Sprintf(`{"op":"memberships","scope":"tenant","id":%q}`, hidden))
	if !viaGet.IsError || !viaMem.IsError {
		t.Fatalf("an unreadable document was answered: get_document %v, memberships %v: %s", viaGet.IsError, viaMem.IsError, viaMem.Text)
	}
	if strings.ReplaceAll(viaMem.Text, "memberships", "get_document") != viaGet.Text {
		t.Errorf("memberships refused differently from get_document:\n %s\n %s", viaMem.Text, viaGet.Text)
	}
	if strings.Contains(viaMem.Text, f.concepts["topics/risk"]) {
		t.Errorf("the refusal names a concept: %s", viaMem.Text)
	}

	noOntology := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:" + memrank.OntologyPath}})
	})
	got := f.memberships(noOntology, fmt.Sprintf(`"id":%q`, open))
	if len(got.Taxonomies) != 0 || !reflect.DeepEqual(got.Classes, []string{"Report"}) || len(got.Issues) == 0 {
		t.Errorf("ontology unreadable: classes %v, taxonomies %+v, issues %v; want the own type, none, an issue", got.Classes, got.Taxonomies, got.Issues)
	}
	if len(got.Folders) != 1 {
		t.Errorf("ontology unreadable: folders = %+v, want the document's own", got.Folders)
	}
}

// TestMemberships_ByPathResolvesTheDocument — a Path name answers for the document
// filed there, the same answer as by id.
func TestMemberships_ByPathResolvesTheDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	doc := f.doc("/kb/by/path", "Report")
	f.place(doc, "topics/reporting", "link_chunks")
	f.doc("/kb/by/other", "Report")

	byID := f.memberships(f.reader, fmt.Sprintf(`"id":%q`, doc))
	byPath := f.memberships(f.reader, `"path":"/kb/by/path"`)
	if byPath.DocumentID != doc || !reflect.DeepEqual(byID, byPath) {
		t.Errorf("by path = %+v, by id = %+v", byPath, byID)
	}
	if _, r := docExec(t, d, f.reader, `{"op":"memberships","scope":"tenant","path":"/kb/by/none"}`); !r.IsError {
		t.Errorf("a path naming nothing was answered: %s", r.Text)
	}
}

// TestGetDocument_WithoutMembershipsIsUnchanged — get_document without
// with_memberships (absent or false) is byte-identical; with it the answer gains
// memberships alone, and that is op=memberships' answer.
func TestGetDocument_WithoutMembershipsIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	doc := f.doc("/kb/g", "Report")
	f.place(doc, "topics/risk/outsourcing", "link_chunks")
	text := func(body string) string {
		t.Helper()
		_, r := docExec(t, d, f.reader, body)
		if r.IsError {
			t.Fatalf("%s: %s", body, r.Text)
		}
		return r.Text
	}
	plain := text(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, doc))
	if off := text(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q,"with_memberships":false}`, doc)); off != plain {
		t.Errorf("with_memberships:false differs:\n%s\n%s", off, plain)
	}
	if strings.Contains(plain, `"memberships"`) {
		t.Errorf("without with_memberships the answer carries memberships: %s", plain)
	}
	var a, b map[string]any
	_ = json.Unmarshal([]byte(plain), &a)
	_ = json.Unmarshal([]byte(text(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q,"with_memberships":true}`, doc))), &b)
	mem := b["memberships"]
	delete(b, "memberships")
	if !reflect.DeepEqual(a, b) {
		t.Errorf("with_memberships changed more than memberships:\n%v\n%v", a, b)
	}
	var op any
	_ = json.Unmarshal([]byte(text(fmt.Sprintf(`{"op":"memberships","scope":"tenant","id":%q}`, doc))), &op)
	if mem == nil || !reflect.DeepEqual(mem, op) {
		t.Errorf("memberships = %v, op=memberships = %v", mem, op)
	}
}

// TestOntologySchemes_MembershipsOnPostgres — the memberships answer on the Postgres
// tier (named for the CI filter's TestOntologySchemes_).
func TestOntologySchemes_MembershipsOnPostgres(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	assertMembershipsListEveryTaxonomy(t, newMemberFixture(t, d, ctx))
}
