package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// membershipOntologyMD declares document classes (Report with a subclass Brief,
// Drawing, and the operational Annotation) and two schemes: topics projects Report,
// systems declares no @projects and so arranges every document class.
const membershipOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n\n" +
	"#### Report\nA report.\n\n##### Brief\nA short report.\n\n" +
	"#### Drawing\nA drawing.\n\n" +
	"#### Annotation\nA folder's annotation.\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@projects Report\n\n#### Risk\n\n##### Outsourcing\n\n#### Reporting\n\n" +
	"### systems\n\n#### Hydraulics\n\n##### Pumps\n"

// memberFixture is a tenant corpus over membershipOntologyMD: the operator context
// that writes, a plain reader, and the concepts by mirror tag.
type memberFixture struct {
	t        *testing.T
	d        *Document
	op       context.Context
	reader   context.Context
	ontology string
	concepts map[string]string
}

func newMemberFixture(t *testing.T, d *Document, base context.Context) *memberFixture {
	t.Helper()
	op := installTenantOntology(t, d, base, membershipOntologyMD, memrank.OntologyConfirmedStatus)
	read, ok := d.tenantOntologyRead(op)
	if !ok || !read.Confirmed {
		t.Fatalf("ontology not confirmed: %#v", read)
	}
	f := &memberFixture{t: t, d: d, op: op, ontology: read.DocumentID, concepts: map[string]string{},
		reader: tools.WithRunPolicy(base, tools.RunPolicy{
			Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}},
			SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}},
		})}
	for tag, c := range schemeConcepts(read) {
		f.concepts[tag] = c.ChunkID
	}
	for _, tag := range []string{"topics/risk", "topics/risk/outsourcing", "topics/reporting", "systems/hydraulics", "systems/hydraulics/pumps"} {
		if f.concepts[tag] == "" {
			t.Fatalf("concept %s missing: %v", tag, f.concepts)
		}
	}
	return f
}

func (f *memberFixture) exec(v map[string]any) map[string]any {
	f.t.Helper()
	b, _ := json.Marshal(v)
	out, r := docExec(f.t, f.d, f.op, string(b))
	if r.IsError {
		f.t.Fatalf("%v: %s", v["op"], r.Text)
	}
	return out
}

// doc writes a tenant document at path, typed when typ is set.
func (f *memberFixture) doc(path, typ string) string {
	f.t.Helper()
	v := map[string]any{"op": "create_document", "scope": "tenant", "title": "Doc", "path": path}
	if typ != "" {
		v["type"] = typ
	}
	return f.exec(v)["document_id"].(string)
}

// place writes a chunk of doc and a statement from it to the concept at tag; how is
// link_chunks (asserted) or propose_statement. It returns the statement id.
func (f *memberFixture) place(doc, tag, how string) string {
	f.t.Helper()
	ch := f.exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc, "title": "Part", "body": "text"})["id"].(string)
	id, _ := f.exec(map[string]any{"op": how, "scope": "tenant", "from_id": ch, "to_id": f.concepts[tag], "kind": "subject"})["id"].(string)
	if id == "" {
		f.t.Fatalf("%s to %s returned no statement id", how, tag)
	}
	return id
}

// unplaced runs describe_ontology with_counts as ctx and returns each scheme's unplaced.
func (f *memberFixture) unplaced(ctx context.Context) map[string]int {
	f.t.Helper()
	desc := describeCounted(f.t, f.d, ctx)
	out := map[string]int{}
	for _, s := range desc.Schemes {
		if s.Unplaced == nil {
			f.t.Fatalf("scheme %s has no unplaced field", s.Name)
		}
		out[s.Name] = *s.Unplaced
	}
	return out
}

// unplacedCorpus writes, for the unplaced counts:
//
//	/kb/placed      Report   about topics/risk/outsourcing      placed in topics and (implied) nowhere else
//	/kb/brief       Brief    about systems/hydraulics          placed in systems only
//	/kb/bare        Report   about nothing
//	/kb/proposed    Report   a proposal into topics/reporting   (a proposal places nothing)
//	/kb/drawing     Drawing  about nothing
//	/kb/doc         Document about nothing
//	/kb/untyped     —        about nothing
//	/kb/_annotation Annotation about nothing                   (operational: never counted)
//
// and types the ontology document itself Document.
func unplacedCorpus(f *memberFixture) {
	f.place(f.doc("/kb/placed", "Report"), "topics/risk/outsourcing", "link_chunks")
	f.place(f.doc("/kb/brief", "Brief"), "systems/hydraulics", "link_chunks")
	f.doc("/kb/bare", "Report")
	f.place(f.doc("/kb/proposed", "Report"), "topics/reporting", "propose_statement")
	f.doc("/kb/drawing", "Drawing")
	f.doc("/kb/doc", "Document")
	f.doc("/kb/untyped", "")
	f.doc("/kb/_annotation", "Annotation")
	tkey, _, err := f.d.resolveScope(f.op, "tenant")
	if err != nil {
		f.t.Fatal(err)
	}
	// Typed directly: the ontology guard rightly refuses retyping its root through the
	// tool, and the count must leave it out whatever its type.
	if err := f.d.exec(f.op, tkey, `UPDATE documents SET type = ? WHERE id = ?`, "Document", f.ontology); err != nil {
		f.t.Fatalf("type the ontology document: %v", err)
	}
}

// TestDescribeOntology_UnplacedCountsOnlyProjectedClasses — topics projects Report: its
// unplaced is the Report documents (Brief, a subclass, included) under none of its
// concepts; a Drawing, a Document and an untyped document are not counted, and a
// document placed only in another scheme, or only by a proposal, is.
func TestDescribeOntology_UnplacedCountsOnlyProjectedClasses(t *testing.T) {
	d, base, _ := documentFixture(t)
	assertUnplacedCountsOnlyProjectedClasses(t, newMemberFixture(t, d, base))
}

func assertUnplacedCountsOnlyProjectedClasses(t *testing.T, f *memberFixture) {
	unplacedCorpus(f)
	// brief (placed in systems only), bare, proposed.
	if got := f.unplaced(f.reader)["topics"]; got != 3 {
		t.Errorf("topics unplaced = %d, want 3 (brief, bare, proposed)", got)
	}
}

// TestDescribeOntology_UnplacedWithoutProjectsCountsEveryDocumentClass — systems has
// no @projects, so its unplaced counts the documents of every document class under
// none of its concepts, leaving out untyped documents, the operational Annotation and
// the ontology document itself.
func TestDescribeOntology_UnplacedWithoutProjectsCountsEveryDocumentClass(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	unplacedCorpus(f)
	// placed, bare, proposed (Report), drawing (Drawing), doc (Document); brief is
	// placed in systems.
	if got := f.unplaced(f.reader)["systems"]; got != 5 {
		t.Errorf("systems unplaced = %d, want 5 (placed, bare, proposed, drawing, doc)", got)
	}
}

// TestDescribeOntology_UnplacedExcludesUnreadableDocuments — a document the caller may
// not read is not counted as unplaced, whichever scheme it would count in.
func TestDescribeOntology_UnplacedExcludesUnreadableDocuments(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	unplacedCorpus(f)
	f.doc("/kb/hidden/r1", "Report")
	f.doc("/kb/hidden/r2", "Drawing")

	if got := f.unplaced(f.reader); got["topics"] != 4 || got["systems"] != 7 {
		t.Fatalf("precondition, everything readable: unplaced = %v, want topics 4, systems 7", got)
	}
	deny := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/hidden/**"}})
	})
	if got := f.unplaced(deny); got["topics"] != 3 || got["systems"] != 5 {
		t.Errorf("deny /kb/hidden: unplaced = %v, want topics 3, systems 5", got)
	}
}

// TestOntologySchemes_UnplacedOnPostgres — the unplaced count on the Postgres tier
// (named for the CI filter's TestOntologySchemes_).
func TestOntologySchemes_UnplacedOnPostgres(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	assertUnplacedCountsOnlyProjectedClasses(t, newMemberFixture(t, d, ctx))
}

// TestProjectedClasses_FollowsSubclassesAndSkipsOperationalTypes — the class set a
// scheme arranges, on its own.
func TestProjectedClasses_FollowsSubclassesAndSkipsOperationalTypes(t *testing.T) {
	classes := []describedClass{
		{Name: "Document", Kind: "document"}, {Name: "Report", Parent: "Document", Kind: "document"},
		{Name: "Brief", Parent: "report", Kind: "document"}, {Name: "Annotation", Parent: "Document", Kind: "document"},
		{Name: "person", Kind: "chunk"},
	}
	keys := func(m map[string]bool) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(projectedClasses([]string{"report", "Nope"}, classes)); !reflect.DeepEqual(got, []string{"Brief", "Report"}) {
		t.Errorf("@projects report = %v, want Brief, Report", got)
	}
	if got := keys(projectedClasses(nil, classes)); !reflect.DeepEqual(got, []string{"Brief", "Document", "Report"}) {
		t.Errorf("no @projects = %v, want every document class but Annotation", got)
	}
	if got := keys(projectedClasses([]string{"person"}, classes)); !reflect.DeepEqual(got, []string{"person"}) {
		t.Errorf("@projects person = %v, want person", got)
	}
}
