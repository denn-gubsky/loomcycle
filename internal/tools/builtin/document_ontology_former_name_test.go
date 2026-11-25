package builtin

// document_ontology_former_name_test.go — RFC GQ P5.1: a class's former name
// (@renamed_from) reads as the class.
//
// Every test starts from an EXISTING state: rows are written under the old name first,
// and only then does the ontology gain the directive. Each positive has a minimal-pair
// negative beside it (a class that was not renamed, a misspelt name, an ontology
// without the directive).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// formerOntologyMD is the ontology BEFORE any rename: two document classes with rows
// (Policy, with a subclass, and Memo), two entity classes (Supplier, Vendor), a relation
// whose domain is Policy and a scheme that projects Policy.
const formerOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Policy\nA rule the organisation sets.\n@annotate the scope; the owner\n@definitions ^Definitions$\n\n" +
	"##### Internal Policy\n\n" +
	"#### Memo\n\n" +
	"### Supplier\n- `name` text\n\n" +
	"### Vendor\n- `name` text\n\n" +
	"## Relations\n@section relations\n\n" +
	"### governs\n@domain Policy\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@projects Policy\n\n#### Risk\n"

// formerFixture is formerOntologyMD with rows written under its names.
type formerFixture struct {
	*retiredFixture
	policy, internal, memo, plain string // documents of the tenant scope
	policyRoot, memoRoot          string
	definitions                   string // a chunk of policy titled Definitions
}

func newFormerFixtureOn(t *testing.T, d *Document, base context.Context) *formerFixture {
	t.Helper()
	f := &formerFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, formerOntologyMD, memrank.OntologyConfirmedStatus))}}
	doc := func(title, typ string) (string, string) {
		v := map[string]any{"op": "create_document", "scope": "tenant", "title": title,
			"path": "/kb/" + strings.ToLower(strings.ReplaceAll(title, " ", "-"))}
		if typ != "" {
			v["type"] = typ
		}
		out := f.must(v)
		return out["document_id"].(string), out["root_chunk_id"].(string)
	}
	f.policy, f.policyRoot = doc("Access policy", "Policy")
	f.internal, _ = doc("Internal rules", "Internal Policy")
	f.memo, f.memoRoot = doc("Memo one", "Memo")
	f.plain, _ = doc("Plain note", "")
	f.definitions = f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": f.policy,
		"parent_id": f.policyRoot, "title": "Definitions", "body": "Access means reading."})["id"].(string)
	// A subject in the user scope, typed with the entity class.
	f.must(map[string]any{"op": "create_document", "scope": "user", "title": "Acme", "type": "Supplier",
		"subject": "Acme", "natural_key": "supplier:acme", "path": "/facts/acme"})
	return f
}

func newFormerFixture(t *testing.T) *formerFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	return newFormerFixtureOn(t, d, base)
}

// ontologyEntryID finds an ontology chunk by its title.
func ontologyEntryID(t *testing.T, d *Document, ctx context.Context, title string) (id, typ string) {
	t.Helper()
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("the ontology could not be read")
	}
	res, err := d.query(ctx, mustTenantKey(t, d, ctx),
		`SELECT id, coalesce(type, '') FROM chunks WHERE document_id = ? AND title = ?`, read.DocumentID, title)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("ontology entry %q: %v %v", title, err, res)
	}
	return asStr(res.Rows[0][0]), asStr(res.Rows[0][1])
}

// editOntologyEntry rewrites an ontology entry's title and body in the store, bumping
// its revision as update_chunk does (so the cached read is dropped).
//
// Written to the store, not through update_chunk, on purpose: renaming a class that
// rows rest on is refused there (rename_job_pending) until the rename job exists, and
// this is the state that job's first step leaves — the new title and @renamed_from
// in place while the stored rows still carry the old name (plan 4.2 step 1).
func editOntologyEntry(t *testing.T, d *Document, ctx context.Context, title, newTitle string, body func(string) string) {
	t.Helper()
	key := mustTenantKey(t, d, ctx)
	id, typ := ontologyEntryID(t, d, ctx, title)
	cb, err := d.readBody(ctx, store.MemoryScopeTenant, key, id)
	if err != nil {
		t.Fatalf("read %q: %v", title, err)
	}
	if err := d.writeBody(ctx, store.MemoryScopeTenant, key, id, typ, body(cb.Body), cb.Fields); err != nil {
		t.Fatalf("write %q: %v", title, err)
	}
	if err := d.exec(ctx, key, `UPDATE chunks SET title = ?, revision = revision + 1 WHERE id = ?`, newTitle, id); err != nil {
		t.Fatalf("retitle %q: %v", title, err)
	}
}

// renameOntologyClass renames the class old to name and records the old name on it.
func renameOntologyClass(t *testing.T, d *Document, ctx context.Context, old, name string) {
	t.Helper()
	editOntologyEntry(t, d, ctx, old, name, func(b string) string {
		return strings.TrimRight(b, "\n") + "\n@renamed_from " + old + "\n"
	})
}

// addFormerName records old as the former name of the class named class, which keeps
// its own name.
func addFormerName(t *testing.T, d *Document, ctx context.Context, class, old string) {
	t.Helper()
	editOntologyEntry(t, d, ctx, class, class, func(b string) string {
		return strings.TrimRight(b, "\n") + "\n@renamed_from " + old + "\n"
	})
}

// rename is the whole accepted change of a rename of Policy to Rule and of Supplier to
// Partner: the ontology's own mentions of the class follow in the same change (RFC GQ
// 9b C12). No stored row is touched.
func (f *formerFixture) rename() {
	f.t.Helper()
	renameOntologyClass(f.t, f.d, f.ctx, "Policy", "Rule")
	renameOntologyClass(f.t, f.d, f.ctx, "Supplier", "Partner")
	for _, entry := range []string{"governs", "topics"} {
		editOntologyEntry(f.t, f.d, f.ctx, entry, entry, func(b string) string { return strings.ReplaceAll(b, "Policy", "Rule") })
	}
	if got := f.docType(f.policy); got != "Policy" {
		f.t.Fatalf("the stored type moved to %q; these tests need it left as written", got)
	}
}

func (f *formerFixture) docType(doc string) string {
	f.t.Helper()
	res, err := f.d.query(f.ctx, mustTenantKey(f.t, f.d, f.ctx), `SELECT coalesce(type, '') FROM documents WHERE id = ?`, doc)
	if err != nil || len(res.Rows) != 1 {
		f.t.Fatalf("document %s: %v", doc, err)
	}
	return asStr(res.Rows[0][0])
}

// rowTitles is the sorted titles of the rows under key in a read's answer.
func rowTitles(out map[string]any, key string) []string {
	rows, _ := out[key].([]any)
	got := []string{}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		got = append(got, asStr(m["title"]))
	}
	sort.Strings(got)
	return got
}

func (f *formerFixture) documentsOfType(typ string) []string {
	f.t.Helper()
	return rowTitles(f.must(map[string]any{"op": "query_documents", "scope": "tenant", "type": typ}), "documents")
}

func wantTitles(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(got, append([]string{}, want...)) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// typeFilterFindsFormerName is the body of the type-filter test, run on both tiers.
func typeFilterFindsFormerName(t *testing.T, f *formerFixture) {
	t.Helper()
	// Before the rename the fixture reads as any ontology does.
	wantTitles(t, "type=Policy before the rename", f.documentsOfType("Policy"), "Access policy", "Internal rules")
	wantTitles(t, "type=Rule before the rename", f.documentsOfType("Rule"))
	f.rename()

	// The new name finds the rows still stored under the former one, and the subclass's.
	wantTitles(t, "query_documents type=Rule", f.documentsOfType("Rule"), "Access policy", "Internal rules")
	// The former name asks for the class.
	wantTitles(t, "query_documents type=Policy", f.documentsOfType("Policy"), "Access policy", "Internal rules")
	// Minimal pairs: a class that was not renamed finds only its own, and a name no
	// class has or had finds nothing.
	wantTitles(t, "query_documents type=Memo", f.documentsOfType("Memo"), "Memo one")
	wantTitles(t, "query_documents type=Polic", f.documentsOfType("Polic"))
	// A new document of the class, under its new name, is found beside the old ones.
	f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "New rule", "type": "Rule", "path": "/kb/new-rule"})
	wantTitles(t, "query_documents type=Rule, old and new rows", f.documentsOfType("Rule"), "Access policy", "Internal rules", "New rule")
	wantTitles(t, "query_documents type=Policy, old and new rows", f.documentsOfType("Policy"), "Access policy", "Internal rules", "New rule")

	out := f.must(map[string]any{"op": "query_documents", "scope": "tenant", "type": "Rule"})
	if got := fmt.Sprint(out["type_expanded_to"]); got != "[Rule Internal Policy Policy]" {
		t.Errorf("type_expanded_to = %s, want the class, its subclass and its former name", got)
	}
	// The other ops that take a type go through the same expansion.
	chunks := rowTitles(f.must(map[string]any{"op": "query_chunks", "scope": "tenant", "type": "Rule"}), "chunks")
	wantTitles(t, "query_chunks type=Rule", chunks, "Access policy", "Internal rules", "New rule")
	snap := f.must(map[string]any{"op": "graph_snapshot", "scope": "tenant", "types": []string{"Rule"}, "include": []string{"documents"}})
	if n := snapshotDocumentCount(t, snap); n != 3 {
		t.Errorf("graph_snapshot types=[Rule] returned %d documents, want 3: %v", n, snap["documents"])
	}
	facts := rowTitles(f.must(map[string]any{"op": "list_facts", "scope": "user", "type": "Partner"}), "facts")
	wantTitles(t, "list_facts type=Partner", facts, "Acme")
	wantTitles(t, "list_facts type=Vendor", rowTitles(f.must(map[string]any{"op": "list_facts", "scope": "user", "type": "Vendor"}), "facts"))

	// A renamed SUBCLASS is still reached from its parent: the expansion brings each
	// subclass's former name, not only the asked class's.
	renameOntologyClass(t, f.d, f.ctx, "Internal Policy", "Internal Rule")
	wantTitles(t, "type=Rule after its subclass was renamed too", f.documentsOfType("Rule"), "Access policy", "Internal rules", "New rule")
	wantTitles(t, "type=Internal Rule", f.documentsOfType("Internal Rule"), "Internal rules")
}

// snapshotDocumentCount is the number of documents a graph_snapshot page carries (its
// documents section is columnar).
func snapshotDocumentCount(t *testing.T, snap map[string]any) int {
	t.Helper()
	docs, _ := snap["documents"].(map[string]any)
	for _, col := range docs {
		if list, ok := col.([]any); ok {
			return len(list)
		}
	}
	if list, ok := snap["documents"].([]any); ok {
		return len(list)
	}
	t.Fatalf("graph_snapshot documents = %v", snap["documents"])
	return 0
}

// TestOntology_TypeFilterFindsRowsUnderTheFormerClassName: after a class is renamed
// with @renamed_from, a type filter for the new name returns the rows still stored
// under the old one (and the old name asks for the class), on every op that takes a
// type; a class that was not renamed and a name nobody had are unaffected.
func TestOntology_TypeFilterFindsRowsUnderTheFormerClassName(t *testing.T) {
	typeFilterFindsFormerName(t, newFormerFixture(t))
}

// TestOntology_TypeFilterFindsRowsUnderTheFormerClassNameOnPostgres is the same on the
// postgres tier.
func TestOntology_TypeFilterFindsRowsUnderTheFormerClassNameOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	typeFilterFindsFormerName(t, newFormerFixtureOn(t, d, base))
}

// TestOntology_FormerClassNameIsNotUndeclared: a type that is a class's former name is
// declared — to the entity gate, to a subject's re-typing, to the class rules of a
// write, and to the class chain of a document — while a misspelt name stays undeclared.
func TestOntology_FormerClassNameIsNotUndeclared(t *testing.T) {
	f := newFormerFixture(t)
	subject := func(name, typ string) (map[string]any, string) {
		out, r := f.do(f.ctx, map[string]any{"op": "create_document", "scope": "user", "title": name, "type": typ,
			"subject": name, "natural_key": strings.ToLower(typ + ":" + name), "path": "/facts/" + strings.ToLower(name)})
		if !r.IsError {
			return out, ""
		}
		return out, refusalText(r)
	}
	// Before the rename "Partner" is not a type, as any unknown name is not.
	if _, text := subject("Early", "Partner"); !strings.Contains(text, "not an entity type") {
		t.Fatalf("a subject of an undeclared type before the rename: %q", text)
	}
	f.rename()

	if _, text := subject("Globex", "Supplier"); text != "" {
		t.Errorf("a subject typed with the class's former name was refused: %s", text)
	}
	if _, text := subject("Initech", "Partner"); text != "" {
		t.Errorf("a subject typed with the class's name was refused: %s", text)
	}
	if _, text := subject("Hooli", "Suplier"); !strings.Contains(text, "not an entity type") {
		t.Errorf("a misspelt type passed the entity gate: %q", text)
	}
	if declared, undeclared := f.d.undeclaredEntityType(f.ctx, "supplier"); undeclared {
		t.Errorf("the former name in another case is undeclared (declared: %v)", declared)
	}

	// The class rules of a write: a document typed with the former name is of a
	// declared document class; a misspelt one is warned about, as before.
	out := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Late policy", "type": "Policy", "path": "/kb/late"})
	if w := warningsOf(out); strings.Contains(w, "names no class") {
		t.Errorf("a document typed with the former name was warned as undeclared: %s", w)
	}
	out = f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Typo policy", "type": "Polcy", "path": "/kb/typo"})
	if w := warningsOf(out); !strings.Contains(w, "names no class") {
		t.Errorf("a misspelt document type drew no warning: %q", w)
	}

	// The class chain of a document still stored under the former name is the class's.
	mem := f.must(map[string]any{"op": "memberships", "scope": "tenant", "document_id": f.policy})
	if got := fmt.Sprint(mem["classes"]); got != "[Rule Document]" {
		t.Errorf("memberships classes = %s, want [Rule Document]", got)
	}
	mem = f.must(map[string]any{"op": "memberships", "scope": "tenant", "document_id": f.memo})
	if got := fmt.Sprint(mem["classes"]); got != "[Memo Document]" {
		t.Errorf("memberships classes of an unrenamed class = %s, want [Memo Document]", got)
	}
}

// issuesOf is describe_ontology's issues that mention @renamed_from or a former name.
func (f *formerFixture) formerNameIssues() []string {
	f.t.Helper()
	out := f.must(map[string]any{"op": "describe_ontology"})
	var got []string
	issues, _ := out["issues"].([]any)
	for _, i := range issues {
		if s := asStr(i); strings.Contains(s, "renamed_from") || strings.Contains(s, "former name") {
			got = append(got, s)
		}
	}
	return got
}

// TestOntology_TwoClassesClaimingOneFormerNameIsAnIssue: a former name two classes
// claim is reported and reads as NEITHER (its rows stay undeclared rather than be
// filed under a class chosen by document order); a former name that is a class in
// force is reported and the class in force keeps its rows. One class claiming a free
// name raises nothing.
func TestOntology_TwoClassesClaimingOneFormerNameIsAnIssue(t *testing.T) {
	f := newFormerFixture(t)
	f.rename() // one claim: Rule was Policy
	if got := f.formerNameIssues(); len(got) != 0 {
		t.Fatalf("one class claiming a free former name raised %v", got)
	}

	// Memo claims Policy too.
	addFormerName(t, f.d, f.ctx, "Memo", "Policy")
	got := f.formerNameIssues()
	if len(got) != 1 || !strings.Contains(got[0], `"Policy"`) || !strings.Contains(got[0], "Memo, Rule") || !strings.Contains(got[0], "none of them") {
		t.Errorf("issues = %q, want one naming Policy and both classes", got)
	}
	wantTitles(t, "type=Rule with the name contested", f.documentsOfType("Rule"), "Internal rules")
	wantTitles(t, "type=Memo with the name contested", f.documentsOfType("Memo"), "Memo one")
	if _, undeclared := f.d.undeclaredEntityType(f.ctx, "Policy"); !undeclared {
		t.Error("a former name two classes claim reads as declared")
	}
	// A plan reports what the ontology would say AFTER the change: one that leaves both
	// claims in place carries the issue, one that removes a claimant does not.
	for entry, want := range map[string]bool{"Vendor": true, "Memo": false} {
		plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: f.chunk(entry)}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(plan.Issues, "\n"), "none of them"); got != want {
			t.Errorf("a plan retiring %s reports the contested name = %v, want %v: %q", entry, got, want, plan.Issues)
		}
	}
}

// TestOntology_FormerNameOfAClassInForceIsAnIssueAndTheClassKeepsIt: @renamed_from
// naming a class in force is ignored and reported; the named class keeps its rows.
func TestOntology_FormerNameOfAClassInForceIsAnIssueAndTheClassKeepsIt(t *testing.T) {
	f := newFormerFixture(t)
	addFormerName(t, f.d, f.ctx, "Policy", "Memo")
	got := f.formerNameIssues()
	if len(got) != 1 || !strings.Contains(got[0], "names a class in force") || !strings.Contains(got[0], `"Policy"`) {
		t.Errorf("issues = %q, want one saying Memo is a class in force", got)
	}
	wantTitles(t, "type=Policy", f.documentsOfType("Policy"), "Access policy", "Internal rules")
	wantTitles(t, "type=Memo", f.documentsOfType("Memo"), "Memo one")
	// A seed class is a class in force too.
	addFormerName(t, f.d, f.ctx, "Vendor", "person")
	if got := f.formerNameIssues(); len(got) != 2 {
		t.Errorf("issues = %q, want the seed class's name reported too", got)
	}
}

// TestOntology_RetiredClassResolvesItsFormerName: a retired class reads its former
// name as it reads its own — the rows stay found and declared — and nothing new may be
// made of it under either name. Without the retirement the former name takes new rows.
func TestOntology_RetiredClassResolvesItsFormerName(t *testing.T) {
	f := newFormerFixture(t)
	f.rename()
	if _, r := f.do(f.ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "Live", "type": "Policy", "path": "/kb/live"}); r.IsError {
		t.Fatalf("a new document under the former name of a LIVE class: %s", r.Text)
	}
	f.retire("Rule")

	wantTitles(t, "type=Rule after the retirement", f.documentsOfType("Rule"), "Access policy", "Internal rules", "Live")
	for _, typ := range []string{"Rule", "Policy"} {
		_, r := f.do(f.ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "New " + typ, "type": typ, "path": "/kb/new-" + typ})
		if !r.IsError || !strings.Contains(r.Text, "entry_retired") {
			t.Errorf("a new document typed %s, of a retired class: %q", typ, r.Text)
		}
	}
	if c, ok := retiredClassNamed(f.read(), "policy"); !ok || c.Name != "Rule" {
		t.Errorf("retiredClassNamed(policy) = %v %v, want the retired Rule", c.Name, ok)
	}
	if _, ok := retiredClassNamed(f.read(), "Memo"); ok {
		t.Error("a live class reads as retired")
	}
	// The document that already has the type is still editable.
	rev := int(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": f.policyRoot})["revision"].(float64))
	if _, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": f.policyRoot, "revision": rev, "title": "Access policy, edited"}); r.IsError {
		t.Errorf("editing a document stored under the former name of a retired class: %s", r.Text)
	}
}

// TestRelationGate_EndUnderTheFormerClassNameIsInTheDomain: an end still typed with
// the former name satisfies a domain naming the class; an end of another class does not.
func TestRelationGate_EndUnderTheFormerClassNameIsInTheDomain(t *testing.T) {
	f := newFormerFixture(t)
	link := func(from string) string {
		_, r := f.do(f.ctx, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": from, "to_id": f.definitions, "kind": "governs"})
		if !r.IsError {
			return ""
		}
		return refusalText(r)
	}
	f.rename()
	if text := link(f.policyRoot); text != "" {
		t.Errorf("a Policy-typed end was refused for @domain Rule: %s", text)
	}
	if text := link(f.memoRoot); !strings.Contains(text, "domain_range_violation") {
		t.Errorf("a Memo end passed @domain Rule: %q", text)
	}
}

// TestDescribeOntology_CountsRowsUnderTheFormerClassName: with_counts counts a document
// stored under the former name as the class's, in the class's documents, in a
// concept's members_by_class and in a scheme's unplaced; node_stats takes the same
// documents as the class's members.
func TestDescribeOntology_CountsRowsUnderTheFormerClassName(t *testing.T) {
	f := newFormerFixture(t)
	risk := conceptOfTag(f.read(), "topics/risk").ChunkID
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": f.policyRoot, "to_id": risk, "kind": "subject"})
	f.rename()

	desc := describeCounted(t, f.d, f.ctx)
	docs := map[string]int{}
	for _, c := range desc.Classes {
		if c.Documents != nil {
			docs[c.Name] = *c.Documents
		}
	}
	if docs["Rule"] != 2 || docs["Memo"] != 1 || docs["Internal Policy"] != 1 {
		t.Errorf("documents per class = %v, want Rule 2 (itself and its subclass), Memo 1, Internal Policy 1", docs)
	}
	_, byClass := conceptMembers(t, desc)
	if got := byClass["topics/risk"]; !reflect.DeepEqual(got, map[string]int{"Rule": 1}) {
		t.Errorf("members_by_class = %v, want the member under the class's name", got)
	}
	// topics projects Rule: the placed policy is placed, the internal one is not.
	if u := desc.Schemes[0].Unplaced; u == nil || *u != 1 {
		t.Errorf("unplaced = %v, want 1", u)
	}

	nodes, err := f.d.classStatNodes(f.ctx, mustTenantKey(t, f.d, f.ctx), f.read(), nil)
	if err != nil {
		t.Fatal(err)
	}
	members := map[string][]string{}
	for _, n := range nodes {
		members[n.Name] = n.members
	}
	want := []string{f.policy, f.internal}
	sort.Strings(want)
	if !reflect.DeepEqual(members["Rule"], want) {
		t.Errorf("node_stats members of Rule = %v, want %v", members["Rule"], want)
	}
	if !reflect.DeepEqual(members["Memo"], []string{f.memo}) {
		t.Errorf("node_stats members of Memo = %v, want its one document", members["Memo"])
	}
}

// TestOntologyPlan_CountsRowsUnderTheFormerClassName: a plan of a change to a class
// counts the rows that rest on it under its former name — the documents, the chunks
// and the subject keys — and a plan of another class does not.
func TestOntologyPlan_CountsRowsUnderTheFormerClassName(t *testing.T) {
	f := newFormerFixture(t)
	f.rename()
	count := func(entry, dependent string) int {
		t.Helper()
		plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: f.chunk(entry)}})
		if err != nil {
			t.Fatal(err)
		}
		c, ok := plan.Changes[0].Counts[dependent]
		if !ok {
			t.Fatalf("plan of %s has no %s count: %+v", entry, dependent, plan.Changes[0].Counts)
		}
		return c.N
	}
	if n := count("Rule", "typed_documents"); n != 2 {
		t.Errorf("typed_documents of Rule = %d, want the two stored under Policy and Internal Policy", n)
	}
	if n := count("Memo", "typed_documents"); n != 1 {
		t.Errorf("typed_documents of Memo = %d, want 1", n)
	}
	if n := count("Partner", "subject_keys"); n != 1 {
		t.Errorf("subject_keys of Partner = %d, want the supplier:acme subject", n)
	}
	if n := count("Vendor", "subject_keys"); n != 0 {
		t.Errorf("subject_keys of Vendor = %d, want 0", n)
	}
}

// TestDocumentClass_RowUnderTheFormerNameKeepsTheClassRules: a document still stored
// under the former name is read with its class's directives — the definitional
// pattern, the annotation slots and meaning, and the class the anchor gate resolves —
// and a document of another class is not.
func TestDocumentClass_RowUnderTheFormerNameKeepsTheClassRules(t *testing.T) {
	f := newFormerFixture(t)
	memoDefs := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": f.memo,
		"parent_id": f.memoRoot, "title": "Definitions", "body": "None."})["id"].(string)
	definitional := func(id string) bool {
		v, _ := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["definitional"].(bool)
		return v
	}
	if !definitional(f.definitions) {
		t.Fatal("the fixture's Definitions chunk is not definitional before the rename")
	}
	f.rename()

	if !definitional(f.definitions) {
		t.Error("a document stored under the former name lost its class's @definitions")
	}
	if definitional(memoDefs) {
		t.Error("a Memo's chunk is definitional")
	}
	in := annInputs(t, f.d, f.ctx, `"document_id":"`+f.policy+`"`)
	if in["class_description"] != "A rule the organisation sets." || in["slots"] != "the scope; the owner" {
		t.Errorf("annotation inputs = %v / %v, want the class's meaning and slots", in["class_description"], in["slots"])
	}
	in = annInputs(t, f.d, f.ctx, `"document_id":"`+f.memo+`"`)
	if in["class_description"] != nil || in["slots"] != nil {
		t.Errorf("a Memo's annotation inputs carry %v / %v", in["class_description"], in["slots"])
	}
	scope := f.d.newAnnotationScope(mustTenantKey(t, f.d, f.ctx), store.MemoryScopeTenant)
	if term, ok := scope.classTerm(f.ctx, "Policy"); !ok || term.Name != "Rule" {
		t.Errorf("classTerm(Policy) = %q %v, want Rule", term.Name, ok)
	}
	if _, ok := scope.classTerm(f.ctx, "Polcy"); ok {
		t.Error("classTerm found a misspelt class")
	}
	key := mustTenantKey(t, f.d, f.ctx)
	if _, class, _ := f.d.classGrammar(f.ctx, key, f.policyRoot, "anchor_grammar"); class != "Rule" {
		t.Errorf("the anchor gate resolves the document's class as %q, want Rule", class)
	}
	if _, class, _ := f.d.classGrammar(f.ctx, key, f.memoRoot, "anchor_grammar"); class != "Memo" {
		t.Errorf("the anchor gate resolves a Memo as %q", class)
	}
}

// TestClassify_NeighbourUnderTheFormerClassNameVotesForTheClass: the neighbourhood vote
// counts a neighbour still typed with the former name as a document of the class.
func TestClassify_NeighbourUnderTheFormerClassNameVotesForTheClass(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, true)
	target, _ := voteCorpus(f, "tenant")
	renameOntologyClass(t, f.d, f.ctx, "Order", "Directive")
	a := f.classify(f.ctx, "tenant", target, nil)
	if a.Class != "Directive" {
		t.Fatalf("answer = %+v, want the renamed class proposed", a)
	}
	if vote := a.Steps[1]; vote.Chosen != "Directive" || vote.Neighbours != 2 {
		t.Errorf("the vote = %+v, want both Order-typed neighbours counted for Directive", vote)
	}
}

// TestMeasure_UnplacedCountsADocumentUnderTheFormerClassName: a document typed with a
// name that later becomes a projected class's former name is one of the scheme's
// projected things, so it is counted unplaced; before the directive it is not.
func TestMeasure_UnplacedCountsADocumentUnderTheFormerClassName(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(conceptOntologyMD, "### Provision\n\n", "### Provision\n\n### Document\n\n#### Report\n\n", 1)
	k := newMeasureCorpus(t, d, base, md)
	k.doc(t, day(2026, 6, 2), k.rep)
	out := k.f.c.exec(map[string]any{"op": "create_document", "scope": "tenant", "type": "Paper", "title": "A paper", "path": "/kb/paper"})
	paper := out["document_id"].(string)
	k.roots[paper] = out["root_chunk_id"].(string)
	k.date(t, paper, day(2026, 6, 5))
	unplaced := func() float64 {
		t.Helper()
		k.compute(t)
		ans := measureRun(t, d, k.f.reader, `{"op":"measure","measure":"unplaced","scheme":"topics"}`)
		if len(ans.Nodes) != 1 || len(ans.Nodes[0].Points) == 0 {
			t.Fatalf("unplaced = %+v", ans.Nodes)
		}
		return ans.Nodes[0].Points[len(ans.Nodes[0].Points)-1].Value
	}
	if v := unplaced(); v != 0 {
		t.Fatalf("unplaced before the directive = %v, want 0 (Paper is no class)", v)
	}
	addFormerName(t, d, k.f.operator, "Report", "Paper")
	if v := unplaced(); v != 1 {
		t.Errorf("unplaced after Report was declared renamed from Paper = %v, want 1", v)
	}
}

var updateFormerGolden = flag.Bool("update-former-golden", false, "rewrite testdata/ontology_without_renamed_from.golden")

// TestOntology_WithoutRenamedFromReadsIdentically: with no @renamed_from anywhere,
// every read this change touches answers exactly as before it. The golden was written
// by the code before RFC GQ P5.1 (origin/main 2e0333253) with -update-former-golden;
// ids are masked, everything else is compared byte for byte.
func TestOntology_WithoutRenamedFromReadsIdentically(t *testing.T) {
	f := newFormerFixture(t)
	risk := conceptOfTag(f.read(), "topics/risk").ChunkID
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": f.policyRoot, "to_id": risk, "kind": "subject"})
	read := f.read()
	parts, _ := json.MarshalIndent(map[string]any{"terms": read.Terms, "issues": read.Issues}, "", " ")
	var b strings.Builder
	section := func(name, text string) { b.WriteString("== " + name + "\n" + text + "\n") }
	section("read", string(parts))
	_, res := docExec(t, f.d, f.ctx, `{"op":"describe_ontology","with_counts":true}`)
	section("describe_ontology with_counts", res.Text)
	for _, typ := range []string{"Policy", "Internal Policy", "Memo", "Document", "Rule", "policy"} {
		out := f.must(map[string]any{"op": "query_documents", "scope": "tenant", "type": typ})
		section("query_documents type="+typ, fmt.Sprint(rowTitles(out, "documents"), out["type_expanded_to"]))
		out = f.must(map[string]any{"op": "query_chunks", "scope": "tenant", "type": typ})
		section("query_chunks type="+typ, fmt.Sprint(rowTitles(out, "chunks"), out["type_expanded_to"]))
	}
	for _, typ := range []string{"Supplier", "Vendor", "Partner"} {
		out := f.must(map[string]any{"op": "list_facts", "scope": "user", "type": typ})
		section("list_facts type="+typ, fmt.Sprint(rowTitles(out, "facts"), out["type_expanded_to"]))
		declared, undeclared := f.d.undeclaredEntityType(f.ctx, typ)
		section("undeclaredEntityType "+typ, fmt.Sprint(declared, undeclared))
	}
	for _, doc := range []string{f.policy, f.internal, f.memo, f.plain} {
		section("memberships", fmt.Sprint(f.must(map[string]any{"op": "memberships", "scope": "tenant", "document_id": doc})["classes"]))
	}
	for _, entry := range []string{"Policy", "Memo", "Supplier"} {
		plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: f.chunk(entry)}})
		if err != nil {
			t.Fatal(err)
		}
		plan.Fingerprint = ""
		pj, _ := json.MarshalIndent(plan, "", " ")
		section("plan retire "+entry, string(pj))
	}
	nodes, err := f.d.classStatNodes(f.ctx, mustTenantKey(t, f.d, f.ctx), read, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		section("node_stats members "+n.Name, fmt.Sprint(len(n.members)))
	}
	_, r := f.do(f.ctx, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": f.memoRoot, "to_id": f.definitions, "kind": "governs"})
	section("link out of the domain", fmt.Sprint(r.IsError, " ", refusalText(r)))
	out := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Typo policy", "type": "Polcy", "path": "/kb/typo"})
	section("create_document of an undeclared type", warningsOf(out))
	section("get_chunk definitional", fmt.Sprint(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": f.definitions})["definitional"]))

	got := hexIDRe.ReplaceAllString(b.String(), "<id>")
	path := filepath.Join("testdata", "ontology_without_renamed_from.golden")
	if *updateFormerGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("an ontology with no @renamed_from no longer reads as before;\n--- got\n%s\n--- want\n%s", got, want)
	}
}
