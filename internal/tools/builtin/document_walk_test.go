package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Walking the classification graph (RFC GM §6, plan P7): lineage's concept chain,
// op=place and search facets with baseline, over membershipOntologyMD:
//
//	topics  (@projects Report): Risk › Outsourcing, Reporting
//	systems:                    Hydraulics › Pumps
//	classes: Document › Report › Brief, Document › Drawing

// placedChunk writes a chunk of doc and, when tag is set, a live subject statement
// from it to the concept at tag; it returns the chunk id.
func (f *memberFixture) placedChunk(doc, tag string) string {
	f.t.Helper()
	ch := f.exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc, "title": "Part", "body": "text"})["id"].(string)
	if tag != "" {
		f.exec(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": ch, "to_id": f.concepts[tag], "kind": "subject"})
	}
	return ch
}

// brief writes an operator's own annotation on a node of the taxonomy.
func (f *memberFixture) brief(level, id, text string) {
	f.t.Helper()
	f.exec(map[string]any{"op": "set_annotation", "scope": "tenant", "level": level, "id": id,
		"brief": text, "abstract": "About " + text + ".", "manual": true})
}

func (f *memberFixture) schemeID(name string) string {
	f.t.Helper()
	read, _ := f.d.tenantOntologyRead(f.op)
	for _, s := range read.Schemes {
		if s.Name == name {
			return s.ChunkID
		}
	}
	f.t.Fatalf("no scheme %s", name)
	return ""
}

// run executes body as ctx and returns the answer's text, failing on a refusal.
func (f *memberFixture) run(ctx context.Context, body string) string {
	f.t.Helper()
	_, r := docExec(f.t, f.d, ctx, body)
	if r.IsError {
		f.t.Fatalf("%s: %s", body, r.Text)
	}
	return r.Text
}

func lineageMembers(t *testing.T, text string) lineageMemberships {
	t.Helper()
	var out struct {
		Memberships *lineageMemberships `json:"memberships"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Memberships == nil {
		t.Fatalf("no memberships in %s (%v)", text, err)
	}
	return *out.Memberships
}

func chainTags(c lineageConceptChain) []string {
	var out []string
	for _, n := range c.Chain {
		out = append(out, n.Tag)
	}
	return out
}

// TestLineage_WithMembershipsClimbsTheConceptChain — lineage with_memberships gives
// the hit's class chain and, per taxonomy, each live placement's chain up to the
// scheme's top, nearest first, each node with its brief; a broader placement under
// a narrower one is not a chain of its own, and a proposal is no chain at all.
func TestLineage_WithMembershipsClimbsTheConceptChain(t *testing.T) {
	d, base, _ := documentFixture(t)
	assertLineageClimbsTheConceptChain(t, newMemberFixture(t, d, base))
}

func assertLineageClimbsTheConceptChain(t *testing.T, f *memberFixture) {
	doc := f.doc("/kb/reports/q3", "Brief")
	hit := f.placedChunk(doc, "topics/risk/outsourcing")
	f.placedChunk(doc, "topics/risk")                     // broader: part of the chain above
	f.placedChunk(doc, "systems/hydraulics/pumps")        // a second taxonomy
	f.place(doc, "topics/reporting", "propose_statement") // a proposal: no chain
	f.brief("concept", f.concepts["topics/risk"], "Risk in every form")
	f.brief("scheme", f.schemeID("topics"), "What the documents are about")
	read, _ := f.d.tenantOntologyRead(f.op)
	f.brief("class", read.classChunkID("Report"), "A report to the board")

	m := lineageMembers(t, f.run(f.reader, fmt.Sprintf(`{"op":"lineage","scope":"tenant","id":%q,"with_memberships":true}`, hit)))
	var classes []string
	for _, c := range m.Classes {
		classes = append(classes, c.Name)
	}
	if want := []string{"Brief", "Report", "Document"}; !reflect.DeepEqual(classes, want) {
		t.Errorf("classes = %v, want %v", classes, want)
	}
	if m.Classes[1].Brief != "A report to the board" || m.Classes[1].ChunkID == "" {
		t.Errorf("Report = %+v, want its chunk and brief", m.Classes[1])
	}
	if len(m.Concepts) != 2 {
		t.Fatalf("concepts = %+v, want one chain per taxonomy", m.Concepts)
	}
	topics, systems := m.Concepts[0], m.Concepts[1]
	if topics.Scheme.Name != "topics" || topics.Scheme.Brief != "What the documents are about" {
		t.Errorf("topics scheme = %+v", topics.Scheme)
	}
	if got, want := chainTags(topics), []string{"topics/risk/outsourcing", "topics/risk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("topics chain = %v, want %v (nearest first)", got, want)
	}
	if topics.Chain[1].Brief != "Risk in every form" || topics.Chain[1].ChunkID != f.concepts["topics/risk"] {
		t.Errorf("topics/risk = %+v", topics.Chain[1])
	}
	if got, want := chainTags(systems), []string{"systems/hydraulics/pumps", "systems/hydraulics"}; !reflect.DeepEqual(got, want) {
		t.Errorf("systems chain = %v, want %v", got, want)
	}
}

// TestLineage_WithoutMembershipsIsUnchanged — without with_memberships (absent or
// false) lineage answers byte-identically; with it the answer gains memberships alone.
func TestLineage_WithoutMembershipsIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	hit := f.placedChunk(f.doc("/kb/g", "Report"), "topics/risk")
	plain := f.run(f.reader, fmt.Sprintf(`{"op":"lineage","scope":"tenant","id":%q}`, hit))
	if off := f.run(f.reader, fmt.Sprintf(`{"op":"lineage","scope":"tenant","id":%q,"with_memberships":false}`, hit)); off != plain {
		t.Errorf("with_memberships:false differs:\n%s\n%s", off, plain)
	}
	if strings.Contains(plain, `"memberships"`) {
		t.Errorf("plain lineage carries memberships: %s", plain)
	}
	var a, b map[string]any
	_ = json.Unmarshal([]byte(plain), &a)
	_ = json.Unmarshal([]byte(f.run(f.reader, fmt.Sprintf(`{"op":"lineage","scope":"tenant","id":%q,"with_memberships":true}`, hit))), &b)
	delete(b, "memberships")
	if !reflect.DeepEqual(a, b) {
		t.Errorf("with_memberships changed more than memberships:\n%v\n%v", a, b)
	}
}

// TestLineage_ConceptChainWithheldWithoutTheOntology — a caller who may not read the
// ontology document gets the hit's own type and no class above it, no chain, no node
// id and no brief, and an issue saying why.
func TestLineage_ConceptChainWithheldWithoutTheOntology(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	hit := f.placedChunk(f.doc("/kb/open/y", "Report"), "topics/risk/outsourcing")
	f.brief("concept", f.concepts["topics/risk"], "Risk in every form")
	noOntology := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:" + memrank.OntologyPath}})
	})
	text := f.run(noOntology, fmt.Sprintf(`{"op":"lineage","scope":"tenant","id":%q,"with_memberships":true}`, hit))
	m := lineageMembers(t, text)
	if !reflect.DeepEqual(m.Classes, []walkNode{{Name: "Report"}}) || len(m.Concepts) != 0 || len(m.Issues) == 0 {
		t.Errorf("ontology unreadable: classes %+v concepts %+v issues %v; want the own type alone, no chain, an issue", m.Classes, m.Concepts, m.Issues)
	}
	for _, leak := range []string{f.concepts["topics/risk"], "Risk in every form", "Outsourcing"} {
		if strings.Contains(text, leak) {
			t.Errorf("the answer names %q: %s", leak, text)
		}
	}
}

// TestLineage_NodeBriefsNeedTheTenantScope — in a user scope the chain comes from the
// chunk's tags; a caller without the tenant scope gets the chain with no brief, and
// an issue saying why.
func TestLineage_NodeBriefsNeedTheTenantScope(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	f.brief("concept", f.concepts["topics/risk"], "Risk in every form")
	userOnly := tools.WithRunPolicy(base, tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"user"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}},
	})
	doc := f.run(userOnly, `{"op":"create_document","scope":"user","title":"Mine","type":"Report"}`)
	var created map[string]any
	_ = json.Unmarshal([]byte(doc), &created)
	ch := f.run(userOnly, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Part","body":"text","tags":["topics/risk/outsourcing"]}`, created["document_id"]))
	var chunk map[string]any
	_ = json.Unmarshal([]byte(ch), &chunk)

	text := f.run(userOnly, fmt.Sprintf(`{"op":"lineage","scope":"user","id":%q,"with_memberships":true}`, chunk["id"]))
	m := lineageMembers(t, text)
	if len(m.Concepts) != 1 || !reflect.DeepEqual(chainTags(m.Concepts[0]), []string{"topics/risk/outsourcing", "topics/risk"}) {
		t.Fatalf("user-scope chain = %+v", m.Concepts)
	}
	if strings.Contains(text, "Risk in every form") || !strings.Contains(strings.Join(m.Issues, " "), "tenant scope") {
		t.Errorf("a caller without the tenant scope got a brief or no issue: %s", text)
	}
}

// --- op=place ---

func placeOf(t *testing.T, text string) placeAnswer {
	t.Helper()
	var out placeAnswer
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return out
}

func taxonomyOf(a placeAnswer, scheme string) *placeTaxonomy {
	for i := range a.Taxonomies {
		if a.Taxonomies[i].Scheme == scheme {
			return &a.Taxonomies[i]
		}
	}
	return nil
}

func idsJSON(ids ...string) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

// TestPlace_NamesTheLowestNodeCoveringTheShare — per taxonomy the deepest concept
// covering min_share of the set (a placement counting under every broader concept
// too), else the scheme itself, else no node; for the class tree the deepest class.
func TestPlace_NamesTheLowestNodeCoveringTheShare(t *testing.T) {
	d, base, _ := documentFixture(t)
	assertPlaceNamesTheLowestNode(t, newMemberFixture(t, d, base))
}

func assertPlaceNamesTheLowestNode(t *testing.T, f *memberFixture) {
	d1 := f.doc("/kb/a/1", "Report")
	f.placedChunk(d1, "topics/risk/outsourcing")
	d2 := f.doc("/kb/a/2", "Report")
	f.placedChunk(d2, "topics/risk/outsourcing")
	d3 := f.doc("/kb/a/3", "Brief")
	f.placedChunk(d3, "topics/risk")
	d4 := f.doc("/kb/a/4", "Report")
	f.placedChunk(d4, "topics/reporting")
	f.brief("concept", f.concepts["topics/risk/outsourcing"], "Outsourced services")

	got := placeOf(t, f.run(f.reader, `{"op":"place","scope":"tenant","document_ids":`+idsJSON(d1, d2, d3)+`}`))
	if got.MinShare != 0.6 || !reflect.DeepEqual(got.Documents, []string{d1, d2, d3}) {
		t.Errorf("min_share %v documents %v", got.MinShare, got.Documents)
	}
	tx := taxonomyOf(got, "topics")
	if tx == nil || tx.Members != 3 || tx.Node == nil {
		t.Fatalf("topics = %+v", tx)
	}
	if n := tx.Node; n.Level != "concept" || n.Tag != "topics/risk/outsourcing" || n.Count != 2 || math.Abs(n.Share-2.0/3) > 1e-9 ||
		n.Brief != "Outsourced services" || n.ChunkID != f.concepts["topics/risk/outsourcing"] || !reflect.DeepEqual(n.Path, []string{"Risk", "Outsourcing"}) {
		t.Errorf("topics node = %+v, want outsourcing covering 2 of 3 with its brief", n)
	}
	if taxonomyOf(got, "systems") != nil {
		t.Errorf("systems listed though it holds none of the set: %+v", got.Taxonomies)
	}
	if c := got.Class; c == nil || c.Name != "Report" || c.Count != 3 || c.ChunkID == "" {
		t.Errorf("class = %+v, want Report covering all three", c)
	}

	strict := placeOf(t, f.run(f.reader, `{"op":"place","scope":"tenant","min_share":0.9,"document_ids":`+idsJSON(d1, d2, d3)+`}`))
	if n := taxonomyOf(strict, "topics").Node; n == nil || n.Tag != "topics/risk" || n.Count != 3 {
		t.Errorf("min_share 0.9: node = %+v, want topics/risk (outsourcing implies it)", n)
	}

	spread := placeOf(t, f.run(f.reader, `{"op":"place","scope":"tenant","document_ids":`+idsJSON(d1, d4)+`}`))
	if n := taxonomyOf(spread, "topics").Node; n == nil || n.Level != "scheme" || n.Name != "topics" || n.Count != 2 {
		t.Errorf("two documents in two branches: node = %+v, want the scheme itself", n)
	}
	thin := placeOf(t, f.run(f.reader, `{"op":"place","scope":"tenant","min_share":1,"document_ids":`+idsJSON(d1, f.doc("/kb/a/5", "Report"))+`}`))
	if tx := taxonomyOf(thin, "topics"); tx == nil || tx.Node != nil || tx.Members != 1 || tx.Share != 0.5 {
		t.Errorf("half the set placed, min_share 1: topics = %+v, want no node", tx)
	}
}

// TestPlace_UnreadableDocumentIsAnsweredAsMissing — a document the caller may not
// read is listed with the ids that name nothing, never told apart, and counts
// nowhere; without the ontology there is no taxonomy and no node id.
func TestPlace_UnreadableDocumentIsAnsweredAsMissing(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	hidden := f.doc("/kb/hidden/x", "Report")
	f.placedChunk(hidden, "topics/reporting")
	open := f.doc("/kb/open/y", "Report")
	f.placedChunk(open, "topics/risk")

	deny := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/hidden/**"}})
	})
	text := f.run(deny, `{"op":"place","scope":"tenant","document_ids":`+idsJSON(hidden, open, "no-such-document")+`}`)
	got := placeOf(t, text)
	want := []string{"no-such-document", hidden}
	if hidden < "no-such-document" {
		want = []string{hidden, "no-such-document"}
	}
	if !reflect.DeepEqual(got.Documents, []string{open}) || !reflect.DeepEqual(got.NotFound, want) {
		t.Errorf("documents %v not_found %v, want [%s] and %v", got.Documents, got.NotFound, open, want)
	}
	if n := taxonomyOf(got, "topics").Node; n == nil || n.Tag != "topics/risk" || n.Count != 1 || n.Share != 1 {
		t.Errorf("node = %+v, want topics/risk over the readable document alone", n)
	}
	if strings.Contains(text, "Reporting") {
		t.Errorf("the hidden document's concept shows: %s", text)
	}

	noOntology := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:" + memrank.OntologyPath}})
	})
	got = placeOf(t, f.run(noOntology, `{"op":"place","scope":"tenant","document_ids":`+idsJSON(open)+`}`))
	if len(got.Taxonomies) != 0 || got.Class == nil || got.Class.Name != "Report" || got.Class.ChunkID != "" || len(got.Issues) == 0 {
		t.Errorf("ontology unreadable: %+v, want no taxonomy, the own type with no chunk, an issue", got)
	}
}

// TestPlace_RefusesMalformedArguments — exactly one of document_ids and query, a
// share in (0, 1], at most 50 documents.
func TestPlace_RefusesMalformedArguments(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newMemberFixture(t, d, base)
	many := make([]string, placeMaxDocuments+1)
	for i := range many {
		many[i] = fmt.Sprintf("d%d", i)
	}
	for _, body := range []string{
		`{"op":"place","scope":"tenant"}`,
		`{"op":"place","scope":"tenant","document_ids":["a"],"query":"x"}`,
		`{"op":"place","scope":"tenant","document_ids":["a"],"min_share":0}`,
		`{"op":"place","scope":"tenant","document_ids":["a"],"min_share":1.5}`,
		`{"op":"place","scope":"tenant","document_ids":` + idsJSON(many...) + `}`,
	} {
		if _, r := docExec(t, d, f.reader, body); !r.IsError {
			t.Errorf("%s was answered: %s", body, r.Text)
		}
	}
}

// TestPlace_QueryPlacesTheTopHitsDocuments — with a query, the set is the documents
// of the search's top hits, each once, in rank order.
func TestPlace_QueryPlacesTheTopHitsDocuments(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := newCountCorpus(t, d, tenantDocCtx(base, nil), "tenant")
	x := c.doc("/kb/x", "rfc")
	c.chunk(x, 0)
	c.chunk(x, 1)
	y := c.doc("/kb/y", "rfc")
	c.chunk(y, 2)
	c.chunk(c.doc("/kb/z", "guide"), 5)
	out := c.exec(c.ctx, map[string]any{"op": "place", "scope": "tenant", "query": "alpha", "limit": 3})
	b, _ := json.Marshal(out)
	got := placeOf(t, string(b))
	if !reflect.DeepEqual(got.Documents, []string{x, y}) {
		t.Errorf("documents = %v, want [%s %s] (three hits, two documents)", got.Documents, x, y)
	}
	if got.Class == nil || got.Class.Name != "rfc" || got.Class.Share != 1 {
		t.Errorf("class = %+v, want rfc over both", got.Class)
	}
}

// TestOntologySchemes_WalkingOnPostgres — lineage's concept chain and place on the
// Postgres tier (named for the CI filter's TestOntologySchemes_).
func TestOntologySchemes_WalkingOnPostgres(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	assertLineageClimbsTheConceptChain(t, newMemberFixture(t, d, ctx))
	d2, ctx2 := pgDocumentOrSkip(t)
	assertPlaceNamesTheLowestNode(t, newMemberFixture(t, d2, ctx2))
}

// --- search facets with baseline ---

func facetRows(t *testing.T, out map[string]any, kind string) map[string]map[string]any {
	t.Helper()
	block, _ := out["facets"].(map[string]any)
	list, ok := block[kind].([]any)
	if !ok {
		t.Fatalf("facet %q missing from %v", kind, block)
	}
	got := map[string]map[string]any{}
	for _, e := range list {
		m := e.(map[string]any)
		got[m["value"].(string)] = m
	}
	return got
}

// TestDocumentSearchFacets_BaselineSharesOverTheReadableCorpus — with baseline each
// facet value carries its corpus count, its share of the counted documents and of
// the corpus, and the capped log-ratio; a reader's corpus is only what it may read.
func TestDocumentSearchFacets_BaselineSharesOverTheReadableCorpus(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		c := newCountCorpus(t, d, open, "tenant")
		c.chunk(c.doc("/a/x", "rfc"), 0)
		c.chunk(c.doc("/a/y", "rfc"), 1)
		c.chunk(c.doc("/b/z", "guide"), 2)
		c.chunk(c.doc("/b/w", "guide"), 3)
		c.chunk(c.doc("/a/h", "rfc"), 6)

		// A window of 2 counts x and y: rfc in 2 of 2 counted, 3 of 5 in the corpus.
		out, text := c.search(open, map[string]any{"facets": []string{"type", "folder"}, "facet_window": 2, "limit": 1, "baseline": true})
		if n := facetNumber(t, out, "corpus_documents"); n != 5 {
			t.Errorf("corpus_documents = %d, want 5: %s", n, text)
		}
		rfc := facetRows(t, out, "type")["rfc"]
		if rfc["corpus"] != float64(3) || rfc["share"] != float64(1) || rfc["corpus_share"] != 0.6 {
			t.Errorf("rfc = %v, want corpus 3, share 1, corpus_share 0.6", rfc)
		}
		if want := cappedLogRatio(2, 2, 3, 5); rfc["log_ratio"] != want || want <= 0 {
			t.Errorf("rfc log_ratio = %v, want %v (> 0)", rfc["log_ratio"], want)
		}
		if a := facetRows(t, out, "folder")["/a"]; a["corpus"] != float64(3) {
			t.Errorf("/a = %v, want corpus 3", a)
		}

		deny := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/a/h"}}))
		out, text = c.search(deny, map[string]any{"facets": []string{"type", "folder"}, "facet_window": 2, "limit": 1, "baseline": true})
		if n := facetNumber(t, out, "corpus_documents"); n != 4 {
			t.Errorf("reader denied /a/h: corpus_documents = %d, want 4: %s", n, text)
		}
		if rfc := facetRows(t, out, "type")["rfc"]; rfc["corpus"] != float64(2) {
			t.Errorf("reader denied /a/h: rfc corpus = %v, want 2 (the hidden one left out)", rfc["corpus"])
		}
		if a := facetRows(t, out, "folder")["/a"]; a["corpus"] != float64(2) {
			t.Errorf("reader denied /a/h: /a corpus = %v, want 2 (the hidden one left out)", a["corpus"])
		}
	})
}

// TestDocumentSearchFacets_BaselineAbsentLeavesTheAnswerUnchanged — without
// baseline the facets are byte-identical to before; baseline without facets is
// refused.
func TestDocumentSearchFacets_BaselineAbsentLeavesTheAnswerUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	c := newCountCorpus(t, d, open, "tenant")
	c.chunk(c.doc("/a/x", "rfc"), 0)
	_, plain := c.search(open, map[string]any{"facets": []string{"type", "folder"}})
	_, off := c.search(open, map[string]any{"facets": []string{"type", "folder"}, "baseline": false})
	if plain != off {
		t.Errorf("baseline:false differs:\n%s\n%s", off, plain)
	}
	for _, field := range []string{"corpus", "log_ratio", "corpus_documents"} {
		if strings.Contains(plain, `"`+field+`"`) {
			t.Errorf("a plain facet answer carries %s: %s", field, plain)
		}
	}
	if _, r := docExec(t, d, open, `{"op":"search","scope":"tenant","query":"alpha","baseline":true}`); !r.IsError {
		t.Errorf("baseline without facets was answered: %s", r.Text)
	}
}

// TestDocumentSearchFacets_BaselineCountsConceptsByTheFacetsRule — a concept's corpus
// count is the readable documents placed on it directly (its own statement, not a
// narrower one's), the rule its facet count follows; it covers the whole scope, not
// the search's filter.
func TestDocumentSearchFacets_BaselineCountsConceptsByTheFacetsRule(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	rm := f.concepts["topics/risk-management"]
	a := f.doc("tenant", "/kb/a")
	f.link("link_chunks", f.chunk("tenant", a), rm, "subject")
	f.link("link_chunks", f.chunk("tenant", f.doc("tenant", "/other/b")), rm, "subject")
	f.link("link_chunks", f.chunk("tenant", f.doc("tenant", "/other/c")), f.concepts["topics/risk-management/outsourcing"], "subject")

	_, text := f.search(f.reader, map[string]any{"scope": "tenant", "under_path": "/kb", "facets": []string{"concept"}, "baseline": true})
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil || !strings.Contains(text, `"facets"`) {
		t.Fatalf("search: %s", text)
	}
	row := facetRows(t, out, "concept")["topics/risk-management"]
	if row["count"] != float64(1) || row["corpus"] != float64(2) {
		t.Errorf("risk-management = %v, want count 1 under /kb and corpus 2 (its own two, not outsourcing's)", row)
	}

	// A reader denied /other/b: its placement is not in the corpus.
	deny := tools.UpdateRunPolicy(f.reader, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/other/b"}})
	})
	_, text = f.search(deny, map[string]any{"scope": "tenant", "under_path": "/kb", "facets": []string{"concept"}, "baseline": true})
	out = nil
	_ = json.Unmarshal([]byte(text), &out)
	if row := facetRows(t, out, "concept")["topics/risk-management"]; row["corpus"] != float64(1) {
		t.Errorf("reader denied /other/b: risk-management corpus = %v, want 1: %s", row["corpus"], text)
	}

	// A user scope counts by tag, at the deepest concept the tag lies under.
	f.chunk("user", f.doc("user", "/kb/u1"), "topics/risk-management")
	f.chunk("user", f.doc("user", "/other/u2"), "topics/risk-management/undeclared")
	f.chunk("user", f.doc("user", "/other/u3"), "topics/risk-management/outsourcing")
	_, text = f.search(f.reader, map[string]any{"scope": "user", "under_path": "/kb", "facets": []string{"concept"}, "baseline": true})
	out = nil
	_ = json.Unmarshal([]byte(text), &out)
	if row := facetRows(t, out, "concept")["topics/risk-management"]; row["count"] != float64(1) || row["corpus"] != float64(2) {
		t.Errorf("user scope: risk-management = %v, want count 1 and corpus 2 (u1 and u2, not u3 under outsourcing): %s", row, text)
	}
}
