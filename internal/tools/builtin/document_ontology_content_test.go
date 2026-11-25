package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// RFC GQ P3.1 — the ontology's content version.

// contentOntologyMD has an entry of every hashed kind (classes with a field, a directive
// and a grammar, relation kinds with a sub-kind, a plain and an @ordered scheme, an
// instance) and everything the version must ignore: a Review section and a section of
// a kind this build does not know.
const contentOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Provision\nA rule a regulator sets.\n@citation_grammar act-cite\n- `number` text @identity\n\n" +
	"##### Article\nA numbered part of one.\n\n" +
	"##### grammar:act-cite\npattern: Act No (?P<number>\\d+)\nresolve: work_key=act:{number}\n\n" +
	"### Company\nAn organisation.\n@memory_scope tenant\n\n" +
	"## Relations\n@section relations\n\n" +
	"### filed_under\nFiled under another.\n@domain Provision\n@functional\n\n" +
	"#### filed_mainly_under\n\n" +
	"### cites\n@walk\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@projects Provision\n\n" +
	"#### Risk Management\n@retention 7y\n\n##### outsourcing\n\n#### reporting\n\n" +
	"### priority\n@ordered\n\n#### high\n\n#### low\n\n" +
	"## Instances\n@section instances\n\n### Acme\n@a Company\n@alt Acme Corp\n\n" +
	"## Review\n@section review\njudge_floor: 0.8\n\n" +
	"- accept kind:cites method:rule:citation when quotes:resolved\n" +
	"- human  *\n\n" +
	"## Questions\n@section questions\n\n### why-late\n- `answer` text\n"

// contentOntologyVersion is contentOntologyMD's content version with each chunk id
// replaced by "<kind>:<name>" (ids are random per import). It moves when the parsed
// shape of an entry, the items hashed or the hash itself changes — every one of which
// re-versions every tenant's ontology, so change it only on purpose.
const contentOntologyVersion = "145f9ebeab60d2a2"

// contentVersionRe matches the content_version field where it closes an answer
// (describe_ontology's and a plan's both end with it).
var contentVersionRe = regexp.MustCompile(`,\s*"content_version":\s*"[0-9a-f]*"`)

// withoutContentVersion drops the field from rendered answers, for the golden tests
// that compare a whole answer with one written before the field existed. Its value
// hashes the chunk ids, which are random per run, so no golden can hold it; the tests
// in this file are what pin it.
func withoutContentVersion(s string) string {
	return contentVersionRe.ReplaceAllString(s, "")
}

type contentFixture struct {
	t   *testing.T
	d   *Document
	ctx context.Context
}

func newContentFixture(t *testing.T, d *Document, base context.Context) *contentFixture {
	t.Helper()
	return &contentFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, contentOntologyMD, memrank.OntologyConfirmedStatus))}
}

func (f *contentFixture) must(v map[string]any) map[string]any {
	f.t.Helper()
	b, _ := json.Marshal(v)
	out, r := docExec(f.t, f.d, f.ctx, string(b))
	if r.IsError {
		f.t.Fatalf("%v: %s", v["op"], r.Text)
	}
	return out
}

// read is the cached read every write path uses, so a version that did not follow an
// edit through the cache would show here.
func (f *contentFixture) read() OntologyRead {
	f.t.Helper()
	read, ok := f.d.tenantOntologyRead(f.ctx)
	if !ok || read.DocumentID == "" {
		f.t.Fatal("the ontology could not be read")
	}
	return read
}

func (f *contentFixture) version() string {
	f.t.Helper()
	v := f.read().ContentVersion
	if len(v) != ontologyContentVersionLen || strings.Trim(v, "0123456789abcdef") != "" {
		f.t.Fatalf("content version = %q, want %d hex characters", v, ontologyContentVersionLen)
	}
	return v
}

func (f *contentFixture) fingerprint() string {
	f.t.Helper()
	fp, err := f.d.ontologyFingerprint(f.ctx, mustTenantKey(f.t, f.d, f.ctx), f.read().DocumentID)
	if err != nil {
		f.t.Fatal(err)
	}
	return fp
}

func (f *contentFixture) chunk(title string) string {
	f.t.Helper()
	res, err := f.d.query(f.ctx, mustTenantKey(f.t, f.d, f.ctx),
		`SELECT id FROM chunks WHERE document_id = ? AND title = ?`, f.read().DocumentID, title)
	if err != nil || len(res.Rows) != 1 {
		f.t.Fatalf("chunk %q: %v %v", title, err, res)
	}
	return asStr(res.Rows[0][0])
}

func (f *contentFixture) body(title string) string {
	f.t.Helper()
	b, _ := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": f.chunk(title)})["body"].(string)
	return b
}

// update is the operator's update_chunk of one entry (nothing rests on any of them, so
// no plan is asked for).
func (f *contentFixture) update(title string, change map[string]any) {
	f.t.Helper()
	id := f.chunk(title)
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	v := map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": int(g["revision"].(float64))}
	for k, x := range change {
		v[k] = x
	}
	f.must(v)
}

// treeParent reads the document's chunk tree, as ontologyForKey builds it.
func (f *contentFixture) treeParent() map[string]string {
	f.t.Helper()
	res, err := f.d.query(f.ctx, mustTenantKey(f.t, f.d, f.ctx),
		`SELECT id, coalesce(parent_id, '') FROM chunks WHERE document_id = ?`, f.read().DocumentID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range res.Rows {
		out[asStr(r[0])] = asStr(r[1])
	}
	return out
}

// items are the hashed items keyed "<kind>:<name>", each rendered as JSON.
func (f *contentFixture) items() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for label, it := range f.labelled() {
		b, err := json.Marshal(it)
		if err != nil {
			f.t.Fatal(err)
		}
		out[label] = string(b)
	}
	return out
}

// labelled are the hashed items with every chunk id replaced by "<kind>:<name>", in
// the item's own id, its parent and its entry.
func (f *contentFixture) labelled() map[string]ontologyContentItem {
	f.t.Helper()
	read := f.read()
	labels := map[string]string{}
	for id, e := range planEntries(read) {
		labels[id] = e.Kind + ":" + e.Name
	}
	for _, g := range read.Grammars {
		labels[g.ChunkID] = planKindGrammar + ":" + g.Name
	}
	out := map[string]ontologyContentItem{}
	for _, it := range ontologyContentItems(read, f.treeParent()) {
		label, ok := labels[it.ChunkID]
		if !ok {
			f.t.Fatalf("a hashed item is neither a plan entry nor a grammar: %+v", it)
		}
		if _, dup := out[label]; dup {
			f.t.Fatalf("two hashed items are %s", label)
		}
		entry, err := json.Marshal(it.Entry)
		if err != nil {
			f.t.Fatal(err)
		}
		for id, l := range labels {
			entry = []byte(strings.ReplaceAll(string(entry), `"`+id+`"`, `"`+l+`"`))
		}
		parent := it.Parent
		if parent != "" {
			if parent, ok = labels[it.Parent]; !ok {
				f.t.Fatalf("%s has a parent that is not a hashed entry: %s", label, it.Parent)
			}
		}
		out[label] = ontologyContentItem{ChunkID: label, Kind: it.Kind, Parent: parent, Entry: json.RawMessage(entry)}
	}
	return out
}

// changedItems names the items that differ between two renderings.
func changedItems(before, after map[string]string) []string {
	var out []string
	for k, v := range before {
		if after[k] != v {
			out = append(out, k)
		}
	}
	for k := range after {
		if _, had := before[k]; !had {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestOntologyContentVersion_EditOfOneClassChangesIt: a directive added to one class
// changes the version and that class's item alone, and undoing the edit brings the
// old version back although every revision has moved on.
func TestOntologyContentVersion_EditOfOneClassChangesIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	// Article: nothing rests on it, so its directives change without a plan.
	v0, items0, body0 := f.version(), f.items(), f.body("Article")

	f.update("Article", map[string]any{"body": body0 + "\n- `issued` date\n"})
	v1 := f.version()
	if v1 == v0 {
		t.Fatalf("a field added to a class left the content version at %s", v0)
	}
	if got := changedItems(items0, f.items()); !reflect.DeepEqual(got, []string{"class:Article"}) {
		t.Errorf("items changed = %v, want the edited class alone", got)
	}

	f.update("Article", map[string]any{"body": body0})
	if v2 := f.version(); v2 != v0 {
		t.Errorf("the edit undone gives %s, want the first version %s", v2, v0)
	}
}

// TestOntologyContentVersion_MeaningEditChangesIt: an edit the plan calls meaning-only
// (the entry's signature is unchanged) still changes the version.
func TestOntologyContentVersion_MeaningEditChangesIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	before := f.read()
	entry := planEntries(before)[f.chunk("Provision")]

	f.update("Provision", map[string]any{"body": strings.Replace(f.body("Provision"), "A rule a regulator sets.", "A rule a regulator issues.", 1)})
	after := f.read()
	if term, _ := termByName(after, "Provision"); term.Meaning != "A rule a regulator issues." {
		t.Fatalf("the meaning was not edited: %q", term.Meaning)
	}
	if entrySignature(before, entry) != entrySignature(after, entry) {
		t.Fatal("the edit changed the entry's signature; it is not a meaning-only edit")
	}
	if after.ContentVersion == before.ContentVersion {
		t.Errorf("an edit of a class's meaning left the content version at %s", before.ContentVersion)
	}
}

// TestOntologyContentVersion_RetiringAnEntryChangesIt: a retired concept stays in the
// read, and the version says the ontology is not what it was.
func TestOntologyContentVersion_RetiringAnEntryChangesIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	v0, items0 := f.version(), f.items()

	f.update("reporting", map[string]any{"status": memrank.OntologyStatusRetired})
	if c := conceptOfTag(f.read(), "topics/reporting"); c.EntryStatus != memrank.OntologyStatusRetired {
		t.Fatalf("the concept was not retired: %+v", c)
	}
	if v1 := f.version(); v1 == v0 {
		t.Errorf("retiring a concept left the content version at %s", v0)
	}
	if got := changedItems(items0, f.items()); !reflect.DeepEqual(got, []string{"concept:reporting"}) {
		t.Errorf("items changed = %v, want the retired concept alone", got)
	}
}

// TestOntologyContentVersion_ProposalDoesNotChangeIt: proposals filed into three
// sections are in the document (the read lists them, the plan fingerprint moves) and
// not in the version.
func TestOntologyContentVersion_ProposalDoesNotChangeIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	v0, fp0, n0 := f.version(), f.fingerprint(), len(f.read().Proposals)

	for _, p := range []map[string]any{
		{"name": "Subsidiary", "parent": "Company", "body": "A company another owns."},
		{"section": "relations", "name": "escalates_to", "body": "Hands a matter up to another."},
		{"section": "schemes", "name": "vendor risk", "parent": "topics/risk-management"},
	} {
		p["op"], p["scope"] = "propose_entity", "user"
		f.must(p)
	}
	if n := len(f.read().Proposals); n != n0+3 {
		t.Fatalf("proposals = %d, want %d: the proposals were not filed", n, n0+3)
	}
	if f.fingerprint() == fp0 {
		t.Fatal("the plan fingerprint did not move; the document did not change")
	}
	if v1 := f.version(); v1 != v0 {
		t.Errorf("three proposals moved the content version from %s to %s", v0, v1)
	}
}

// TestOntologyContentVersion_ChunkUnderAnOutOfForceSectionDoesNotChangeIt: a section of
// a kind this build does not know is not in force, so an edit or a new chunk under it
// changes the document and not the version.
func TestOntologyContentVersion_ChunkUnderAnOutOfForceSectionDoesNotChangeIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	v0, fp0 := f.version(), f.fingerprint()

	f.update("why-late", map[string]any{"body": "@memory_scope tenant\n- `answer` text\n- `owner` text\n", "title": "why-so-late"})
	f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": f.read().DocumentID,
		"parent_id": f.chunk("Questions"), "title": "why-early", "body": "@functional\n- `answer` text\n"})
	if f.fingerprint() == fp0 {
		t.Fatal("the plan fingerprint did not move; the document did not change")
	}
	if v1 := f.version(); v1 != v0 {
		t.Errorf("chunks under an unknown section moved the content version from %s to %s", v0, v1)
	}
}

// TestOntologyContentVersion_ReviewRuleEditDoesNotChangeIt: the Review section is
// policy over proposals, not vocabulary (owner ruling Q3): a changed floor and a new
// rule are read, and the version stays.
func TestOntologyContentVersion_ReviewRuleEditDoesNotChangeIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	before := f.read()
	if before.Review == nil {
		t.Fatal("the fixture's Review section was not read")
	}

	f.update("Review", map[string]any{"body": "@section review\njudge_floor: 0.5\n\n" +
		"- accept kind:cites method:rule:citation when quotes:resolved\n" +
		"- reject kind:filed_under method:extractor:relate when confidence<0.6\n" +
		"- human  *\n"})
	after := f.read()
	if after.Review == nil || reflect.DeepEqual(*before.Review, *after.Review) {
		t.Fatalf("the review policy did not change: %+v", after.Review)
	}
	if after.ContentVersion != before.ContentVersion {
		t.Errorf("an edit of the Review section moved the content version from %s to %s", before.ContentVersion, after.ContentVersion)
	}
}

// TestOntologyContentVersion_SiblingOrderCountsOnlyInAnOrderedScheme: where concepts sit
// among their siblings is not content, except in an @ordered scheme, where it is their
// rank.
func TestOntologyContentVersion_SiblingOrderCountsOnlyInAnOrderedScheme(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	v0, fp0 := f.version(), f.fingerprint()

	f.must(map[string]any{"op": "reorder_chunk", "scope": "tenant", "id": f.chunk("reporting"), "direction": "up"})
	if f.fingerprint() == fp0 {
		t.Fatal("the plan fingerprint did not move; the concept was not reordered")
	}
	if v1 := f.version(); v1 != v0 {
		t.Errorf("a reorder in a scheme that is not ordered moved the content version from %s to %s", v0, v1)
	}

	f.must(map[string]any{"op": "reorder_chunk", "scope": "tenant", "id": f.chunk("low"), "direction": "up"})
	if v2 := f.version(); v2 == v0 {
		t.Errorf("a reorder in an @ordered scheme left the content version at %s", v0)
	}
}

// TestOntologyContentVersion_IsTheSameOnBothTiers: the same document gives the same
// version on SQLite and on Postgres, and that value is pinned.
func TestOntologyContentVersion_IsTheSameOnBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertContentVersionOfTheFixture(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertContentVersionOfTheFixture(t, d, ctx)
	})
}

func assertContentVersionOfTheFixture(t *testing.T, d *Document, base context.Context) {
	t.Helper()
	f := newContentFixture(t, d, base)
	read := f.read()
	// The version the read carries is the hash of its items, under the real chunk ids.
	if want := ontologyContentHash(ontologyContentItems(read, f.treeParent())); f.version() != want {
		t.Errorf("the read carries %s, its items hash to %s", read.ContentVersion, want)
	}

	labelled := f.labelled()
	var labels []string
	for l := range labelled {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	wantLabels := []string{"class:Article", "class:Company", "class:Document", "class:Provision",
		"concept:Risk Management", "concept:high", "concept:low", "concept:outsourcing", "concept:reporting",
		"grammar:act-cite", "instance:Acme", "relation:cites", "relation:filed_mainly_under", "relation:filed_under",
		"scheme:priority", "scheme:topics"}
	if !reflect.DeepEqual(labels, wantLabels) {
		t.Fatalf("hashed items = %v\nwant %v", labels, wantLabels)
	}
	// Each entry's parent is the entry the reader put it under, by chunk.
	for label, parent := range map[string]string{
		"class:Document": "", "class:Provision": "class:Document", "class:Article": "class:Provision",
		"grammar:act-cite": "class:Provision", "relation:filed_mainly_under": "relation:filed_under",
		"scheme:topics": "", "concept:Risk Management": "scheme:topics", "concept:outsourcing": "concept:Risk Management",
		"concept:low": "scheme:priority", "instance:Acme": "",
	} {
		if got := labelled[label].Parent; got != parent {
			t.Errorf("%s: parent = %q, want %q", label, got, parent)
		}
	}

	items := make([]ontologyContentItem, 0, len(labels))
	for _, l := range labels {
		items = append(items, labelled[l])
	}
	if got := ontologyContentHash(items); got != contentOntologyVersion {
		b, _ := json.Marshal(items)
		t.Errorf("content version = %s, want %s\nitems: %s", got, contentOntologyVersion, b)
	}
}

// TestOntologyContentVersion_DescribeAndThePlanShowTheReadsValue: describe_ontology and
// a plan carry the version of the read they were computed from, and a plan's own
// changes (it writes nothing) do not move it.
func TestOntologyContentVersion_DescribeAndThePlanShowTheReadsValue(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newContentFixture(t, d, base)
	want := f.version()

	if got := describeOntologyVia(t, d, f.ctx).ContentVersion; got != want {
		t.Errorf("describe_ontology content_version = %q, want %q", got, want)
	}
	plan, err := d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: f.chunk("reporting")}})
	if err != nil {
		t.Fatalf("PlanOntology: %v", err)
	}
	if plan.ContentVersion != want {
		t.Errorf("plan content_version = %q, want %q", plan.ContentVersion, want)
	}
	if got := f.version(); got != want {
		t.Errorf("planning a retire moved the content version to %s", got)
	}
}
