package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// statsCorpus is a concept fixture whose documents carry document-level
// annotations with vectors the test chose:
//
//	/kb/a1  Document   about outsourcing AND Risk Management (reaches Risk Management twice)
//	/kb/a2  Document   about outsourcing
//	/kb/a3  Document   about outsourcing
//	/kb/b1  Document   about Risk Management
//	/kb/r1..r3  Report about reporting
//	/kb/law1    untyped about Law
type statsCorpus struct {
	f                 *conceptFixture
	d                 *Document
	rm, out, rep, law string // concept ids
	a1, a2, a3, b1    string
	r1, r2, r3, law1  string
	vecs              map[string][]float32 // document → its annotation vector
	briefs            map[string]string    // document → brief + "\n" + abstract, as nodestats reads them
	docs              map[string]string    // document → root chunk
}

func newStatsCorpus(t *testing.T, d *Document, base context.Context) *statsCorpus {
	t.Helper()
	f := newConceptFixture(t, d, base)
	k := &statsCorpus{f: f, d: d, vecs: map[string][]float32{}, briefs: map[string]string{}, docs: map[string]string{},
		rm: f.concepts["topics/risk-management"], out: f.concepts["topics/risk-management/outsourcing"],
		rep: f.concepts["topics/reporting"], law: f.concepts["rank/law"]}
	if k.rm == "" || k.out == "" || k.rep == "" || k.law == "" {
		t.Fatalf("concepts = %v", f.concepts)
	}
	mk := func(path, typ, brief string, vec []float32, concepts ...string) string {
		t.Helper()
		v := map[string]any{"op": "create_document", "scope": "tenant", "title": "Doc " + path, "path": path}
		if typ != "" {
			v["type"] = typ
		}
		out := f.c.exec(v)
		doc := out["document_id"].(string)
		k.docs[doc] = out["root_chunk_id"].(string)
		for _, c := range concepts {
			f.link("link_chunks", f.chunk("tenant", doc), c, "subject")
		}
		k.annotate(t, doc, brief, vec)
		return doc
	}
	k.a1 = mk("/kb/a1", "Document", "outsourcing vendor contracts", []float32{1, 0.1, 0, 0}, k.out, k.rm)
	k.a2 = mk("/kb/a2", "Document", "outsourcing vendor oversight", []float32{0.9, 0.2, 0, 0}, k.out)
	k.a3 = mk("/kb/a3", "Document", "outsourcing exit plans", []float32{0.8, 0, 0.3, 0}, k.out)
	k.b1 = mk("/kb/b1", "Document", "operational risk appetite", []float32{0.5, 0.5, 0, 0}, k.rm)
	k.r1 = mk("/kb/r1", "Report", "quarterly reporting filings", []float32{0, 0, 1, 0.1}, k.rep)
	k.r2 = mk("/kb/r2", "Report", "annual reporting filings", []float32{0, 0.1, 0.9, 0}, k.rep)
	k.r3 = mk("/kb/r3", "Report", "regulatory reporting templates", []float32{0.1, 0, 0.8, 0.2}, k.rep)
	k.law1 = mk("/kb/law1", "", "statute text", []float32{0, 0, 0, 1}, k.law)
	return k
}

// annotate writes doc's document annotation and then puts vec in place of the
// vector the fake embedder made for it.
func (k *statsCorpus) annotate(t *testing.T, doc, brief string, vec []float32) {
	t.Helper()
	op := k.f.operator
	in := annInputs(t, k.d, op, `"document_id":"`+doc+`"`)
	abstract := "About " + brief + "."
	if _, r := docExec(t, k.d, op, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","document_id":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q}`,
		doc, brief, abstract, in["inputs_sha256"])); r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	k.setVector(t, doc, vec, "m1")
	k.briefs[doc] = brief + "\n" + abstract
}

func (k *statsCorpus) setVector(t *testing.T, doc string, vec []float32, model string) {
	t.Helper()
	key, mscope, err := k.d.ontologyTenantKey(k.f.operator)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.f.c.vs.MemoryEmbedSet(k.f.operator, "", mscope, annotationMemScopeID(mscope, key),
		annotationKey(k.docs[doc], annotationLevelDocument),
		store.MemoryEmbedding{Provider: "fake", Model: model, Dimension: len(vec), Vector: vec, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	k.vecs[doc] = vec
}

// nodeStatsRun runs node_stats as ctx and decodes it.
func nodeStatsRun(t *testing.T, d *Document, ctx context.Context, body string) NodeStatsReport {
	t.Helper()
	_, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out NodeStatsReport
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func (a NodeStatsReport) node(t *testing.T, kind, id string) nodeStatsNode {
	t.Helper()
	for _, n := range a.Nodes {
		if n.Kind == kind && n.NodeID == id {
			return n
		}
	}
	t.Fatalf("no %s node %s in %+v", kind, id, a.Nodes)
	return nodeStatsNode{}
}

// storedRows reads node_stats directly, centroids aside.
func storedRows(t *testing.T, d *Document, ctx context.Context) map[string]storedNodeStats {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]storedNodeStats{}
	if err := d.loadStoredNodeStats(ctx, key, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func describeStats(t *testing.T, d *Document, ctx context.Context) ontologyDescription {
	t.Helper()
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology","with_stats":true}`)
	if res.IsError {
		t.Fatalf("describe_ontology with_stats: %s", res.Text)
	}
	var out ontologyDescription
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func conceptStats(desc ontologyDescription, id string) (*nodeStatsView, *int) {
	for _, s := range desc.Schemes {
		for _, c := range s.Concepts {
			if c.ChunkID == id {
				return c.Stats, c.Members
			}
		}
	}
	return nil, nil
}

const computeAll = `{"op":"node_stats","mode":"compute"}`

// TestNodeStats_RollUpCountsADocumentOncePerNode — a broader concept's members are
// its own and every narrower concept's, a document once however many routes reach
// it; a class's members are its documents. Both SQL Memory tiers.
func TestNodeStats_RollUpCountsADocumentOncePerNode(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		ans := nodeStatsRun(t, d, k.f.operator, computeAll)
		for id, want := range map[string]int{k.rm: 4, k.out: 3, k.rep: 3, k.law: 1} {
			if n := ans.node(t, nodeKindConcept, id); n.Members != want {
				t.Errorf("concept %s %s: members = %d, want %d", id, n.Name, n.Members, want)
			}
		}
		if n := ans.node(t, nodeKindClass, "Document"); n.Members != 4 {
			t.Errorf("Document: members = %d, want 4 (the documents typed Document)", n.Members)
		}
		rows := storedRows(t, d, k.f.operator)
		if r := rows[nodeStatsKey(nodeKindConcept, k.rm)]; r.members != 4 || r.withVector != 4 {
			t.Errorf("stored Risk Management = %+v, want members 4 with vector 4 (a1 once)", r)
		}
		if ans.Computed == 0 || ans.Missing != ans.Computed {
			t.Errorf("first compute: computed %d, missing %d — every node is new", ans.Computed, ans.Missing)
		}
	})
}

// TestNodeStats_UnchangedNodeIsNotRecomputed — a second compute with nothing changed
// writes no row and reports every node fresh.
func TestNodeStats_UnchangedNodeIsNotRecomputed(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	nodeStatsRun(t, d, k.f.operator, computeAll)
	before := storedRows(t, d, k.f.operator)
	again := nodeStatsRun(t, d, k.f.operator, computeAll)
	if again.Computed != 0 || again.Stale != 0 || again.Missing != 0 || again.Fresh != len(again.Nodes) {
		t.Errorf("second compute = computed %d stale %d missing %d fresh %d of %d; want all fresh, none computed",
			again.Computed, again.Stale, again.Missing, again.Fresh, len(again.Nodes))
	}
	if after := storedRows(t, d, k.f.operator); !reflect.DeepEqual(before, after) {
		t.Errorf("rows changed on a compute with nothing stale")
	}
	// Not vacuous: a changed member annotation does mark its nodes stale.
	k.annotate(t, k.r1, "monthly reporting filings", k.vecs[k.r1])
	rep := nodeStatsRun(t, d, k.f.operator, `{"op":"node_stats"}`)
	if n := rep.node(t, nodeKindConcept, k.rep); n.Status != "stale" {
		t.Errorf("reporting after r1's annotation changed: %s, want stale", n.Status)
	}
	if n := rep.node(t, nodeKindConcept, k.out); n.Status != "fresh" {
		t.Errorf("outsourcing after r1's annotation changed: %s, want fresh", n.Status)
	}
}

// TestNodeStats_OntologyEditElsewhereMarksNothingStale — editing one concept's
// meaning marks that concept stale and no other node.
func TestNodeStats_OntologyEditElsewhereMarksNothingStale(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	nodeStatsRun(t, d, k.f.operator, computeAll)
	g := k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": k.law})
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": k.law,
		"body": "Statutes and regulations in force.", "revision": g["revision"]})
	if read, _ := d.tenantOntologyRead(k.f.operator); !strings.Contains(fmt.Sprint(read.Schemes), "Statutes and regulations") {
		t.Fatalf("precondition: the edit did not reach the ontology read: %+v", read.Schemes)
	}
	rep := nodeStatsRun(t, d, k.f.operator, `{"op":"node_stats"}`)
	for _, n := range rep.Nodes {
		want := "fresh"
		if n.NodeID == k.law {
			want = "stale"
		}
		if n.Status != want {
			t.Errorf("%s %s: %s (%s), want %s", n.Kind, n.Name, n.Status, n.Reason, want)
		}
	}
}

// TestNodeStats_EmbedModelChangeMarksStale — a row computed under one embed model is
// stale under another, and a vector of the old model is no vector.
func TestNodeStats_EmbedModelChangeMarksStale(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	nodeStatsRun(t, d, k.f.operator, computeAll)
	d.Embedder = newFakeEmbedder("fake", "m2", "risk", "note")
	rep := nodeStatsRun(t, d, k.f.operator, `{"op":"node_stats"}`)
	if rep.Fresh != 0 || rep.Stale != len(rep.Nodes) {
		t.Errorf("after the model change: fresh %d stale %d of %d; want every node stale", rep.Fresh, rep.Stale, len(rep.Nodes))
	}
	if n := rep.node(t, nodeKindConcept, k.rm); n.Reason != "embed model changed" {
		t.Errorf("reason = %q, want embed model changed", n.Reason)
	}
	ans := nodeStatsRun(t, d, k.f.operator, computeAll)
	if n := ans.node(t, nodeKindConcept, k.rm); n.MembersWithVector == nil || *n.MembersWithVector != 0 {
		t.Errorf("members_with_vector under m2 = %v, want 0: every vector is m1's", n.MembersWithVector)
	}
}

// TestNodeStats_ReaderLackingAMemberGetsNoStatistics — with_stats withholds a
// node's statistics from a reader who may not read one of its members, and shows
// the members it may read; a node all of whose members it reads keeps its stats.
func TestNodeStats_ReaderLackingAMemberGetsNoStatistics(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		nodeStatsRun(t, d, k.f.operator, computeAll)

		full := describeStats(t, d, k.f.reader)
		if s, _ := conceptStats(full, k.rm); s == nil || s.Withheld != "" || s.Spread == nil || len(s.Exemplars) != 3 {
			t.Fatalf("precondition: a full reader sees Risk Management's statistics: %+v", s)
		}
		denyA2 := tools.UpdateRunPolicy(k.f.reader, func(rp *tools.RunPolicy) {
			rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/a2"}})
		})
		desc := describeStats(t, d, denyA2)
		for _, id := range []string{k.rm, k.out} {
			s, members := conceptStats(desc, id)
			if s == nil || s.Withheld == "" || s.Spread != nil || s.Exemplars != nil || s.Terms != nil || s.Members != nil {
				t.Errorf("concept %s: stats = %+v, want withheld and no figure", id, s)
			}
			if members == nil {
				t.Errorf("concept %s: no visible member count beside withheld stats", id)
			}
		}
		if _, m := conceptStats(desc, k.rm); m == nil || *m != 3 {
			t.Errorf("Risk Management visible members = %v, want 3", m)
		}
		if s, _ := conceptStats(desc, k.rep); s == nil || s.Withheld != "" || s.Spread == nil {
			t.Errorf("reporting (every member readable): stats = %+v, want shown", s)
		}

		// A row computed from another member set than the one the reader is checked
		// against is withheld from a limited reader until it is recomputed: a member
		// it was computed from may be one the reader cannot see. A reader with no
		// limit still sees it, marked stale.
		k.f.link("link_chunks", k.f.chunk("tenant", k.r1), k.law, "subject")
		desc = describeStats(t, d, denyA2)
		if s, _ := conceptStats(desc, k.law); s == nil || !strings.Contains(s.Withheld, "members changed") {
			t.Errorf("Law after a new member, limited reader: stats = %+v, want withheld (members changed)", s)
		}
		if s, _ := conceptStats(describeStats(t, d, k.f.reader), k.law); s == nil || s.Withheld != "" || !s.Stale {
			t.Errorf("Law after a new member, full reader: stats = %+v, want shown and stale", s)
		}
	})
}

// TestNodeStats_StoreWithoutVectorsReportsCountsOnly — on a store that keeps no
// vectors the pass says so, stores the counts, and computes nothing else.
func TestNodeStats_StoreWithoutVectorsReportsCountsOnly(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	d.Store = k.f.c.vs.Store // the plain sqlite store: SupportsVectors is false
	if d.Store.SupportsVectors() {
		t.Fatal("precondition: the sqlite test store keeps no vectors")
	}
	ans := nodeStatsRun(t, d, k.f.operator, computeAll)
	if ans.Vectors || !strings.Contains(ans.Note, "keeps no vectors") || ans.EmbedModel != "" {
		t.Errorf("answer = vectors %v note %q model %q; want no vectors and why", ans.Vectors, ans.Note, ans.EmbedModel)
	}
	r := storedRows(t, d, k.f.operator)[nodeStatsKey(nodeKindConcept, k.rm)]
	if r.members != 4 || r.withVector != 0 || r.spread != nil || r.exemplars != nil || r.siblings != nil || r.terms != nil {
		t.Errorf("stored row = %+v, want members 4 and no statistic", r)
	}
	s, _ := conceptStats(describeStats(t, d, k.f.reader), k.rm)
	if s == nil || s.Members == nil || *s.Members != 4 || s.Spread != nil || s.Exemplars != nil {
		t.Errorf("with_stats = %+v, want counts only", s)
	}
}

// TestNodeStats_ReportWritesNothing — mode report (and an omitted mode) stores no
// row and changes none.
func TestNodeStats_ReportWritesNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	for _, body := range []string{`{"op":"node_stats"}`, `{"op":"node_stats","mode":"report"}`} {
		ans := nodeStatsRun(t, d, k.f.operator, body)
		if ans.Missing == 0 || ans.Computed != 0 {
			t.Errorf("%s: missing %d computed %d; want every node missing, none computed", body, ans.Missing, ans.Computed)
		}
	}
	if rows := storedRows(t, d, k.f.operator); len(rows) != 0 {
		t.Fatalf("report stored %d rows", len(rows))
	}
	nodeStatsRun(t, d, k.f.operator, computeAll)
	before := storedRows(t, d, k.f.operator)
	k.annotate(t, k.a1, "outsourcing vendor contracts revised", k.vecs[k.a1])
	if rep := nodeStatsRun(t, d, k.f.operator, `{"op":"node_stats"}`); rep.Stale == 0 {
		t.Fatal("precondition: a1's new annotation marks nodes stale")
	}
	if after := storedRows(t, d, k.f.operator); !reflect.DeepEqual(before, after) {
		t.Error("a report changed stored rows")
	}
}

// TestNodeStats_StoredStatisticsMatchNodestats — what the pass stores is what
// nodestats computes on the same vectors and texts: spread, exemplars, siblings
// and terms of Risk Management, whose siblings are reporting (topics' top level).
func TestNodeStats_StoredStatisticsMatchNodestats(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		nodeStatsRun(t, d, k.f.operator, computeAll)
		row := storedRows(t, d, k.f.operator)[nodeStatsKey(nodeKindConcept, k.rm)]

		members := func(docs ...string) []nodestats.Member {
			var ms []nodestats.Member
			for _, doc := range sortedSet(setOf(docs)) {
				ms = append(ms, nodestats.Member{ID: doc, Vec: k.vecs[doc]})
			}
			return ms
		}
		cent := func(ms []nodestats.Member) nodestats.Centroid {
			vs := make([]nodestats.Vector, len(ms))
			for i, m := range ms {
				vs[i] = m.Vec
			}
			c, err := nodestats.CentroidOf(vs)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		texts := func(docs ...string) []string {
			var out []string
			for _, doc := range sortedSet(setOf(docs)) {
				b, a, _ := strings.Cut(k.briefs[doc], "\n")
				out = append(out, b, a)
			}
			return out
		}
		rmMembers := members(k.a1, k.a2, k.a3, k.b1)
		c := cent(rmMembers)
		if row.spread == nil || math.Abs(*row.spread-c.Spread()) > 1e-12 {
			t.Errorf("spread = %v, want %v", row.spread, c.Spread())
		}
		ex, _ := nodestats.Exemplars(c, rmMembers, nodestats.KExemplars)
		var wantEx []string
		for _, e := range ex {
			wantEx = append(wantEx, e.ID)
		}
		var gotEx []string
		for _, e := range row.exemplars {
			gotEx = append(gotEx, e.ID)
		}
		if !reflect.DeepEqual(gotEx, wantEx) {
			t.Errorf("exemplars = %v, want %v", gotEx, wantEx)
		}
		near, _ := nodestats.NearestSiblings(nodestats.NodeCentroid{ID: k.rm, Centroid: c},
			[]nodestats.NodeCentroid{{ID: k.rep, Centroid: cent(members(k.r1, k.r2, k.r3))}}, nodestats.KSiblings)
		if len(row.siblings) != 1 || row.siblings[0].ID != near[0].ID || math.Abs(row.siblings[0].Score-near[0].Score) > 1e-12 {
			t.Errorf("siblings = %+v, want %+v", row.siblings, near)
		}
		terms, _ := nodestats.DistinctiveTerms(texts(k.a1, k.a2, k.a3, k.b1), [][]string{texts(k.r1, k.r2, k.r3)}, nodestats.TopTerms)
		var wantTerms []string
		for _, tm := range terms {
			wantTerms = append(wantTerms, tm.Term)
		}
		if len(wantTerms) == 0 || !reflect.DeepEqual(row.terms, wantTerms) {
			t.Errorf("terms = %v, want %v", row.terms, wantTerms)
		}
	})
}

// TestNodeStats_WhoMayRunIt — an operator and a run granted statement_methods may
// run the pass; a plain run may not, nor a granted run whose rules hide a document,
// nor one without the tenant scope.
func TestNodeStats_WhoMayRunIt(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	run := func(sm []string, eff *authz.Effective, scopes ...string) context.Context {
		return tools.WithRunID(tools.WithRunPolicy(base, tools.RunPolicy{
			Tools:            []string{"*"},
			Memory:           tools.MemoryPolicyValue{AllowedScopes: scopes},
			SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: scopes},
			StatementMethods: sm, ACL: eff,
		}), "run-stats")
	}
	hide := aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/a2"}})
	for name, c := range map[string]struct {
		ctx context.Context
		ok  bool
	}{
		"operator":            {k.f.operator, true},
		"granted run":         {run([]string{"rule"}, nil, "tenant"), true},
		"plain run":           {run(nil, nil, "tenant"), false},
		"granted, hidden doc": {run([]string{"rule"}, hide, "tenant"), false},
		"granted, user scope": {run([]string{"rule"}, nil, "user"), false},
		"annotate grant only": {run([]string{annotationGrant}, nil, "tenant"), false},
	} {
		_, res := docExec(t, d, c.ctx, `{"op":"node_stats","mode":"compute"}`)
		if res.IsError == c.ok {
			t.Errorf("%s: error=%v, want allowed=%v: %s", name, res.IsError, c.ok, res.Text)
		}
		if !c.ok && !strings.Contains(res.Text, "permission") {
			t.Errorf("%s: refusal does not say permission: %s", name, res.Text)
		}
	}
}

// TestDescribeOntology_WithoutStatsIsUnchanged — without with_stats the answer is
// byte-identical whether or not rows are stored; with it, the answer is the
// with_counts answer plus the stats fields alone.
func TestDescribeOntology_WithoutStatsIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	text := func(input string) string {
		t.Helper()
		_, res := docExec(t, d, k.f.reader, input)
		if res.IsError {
			t.Fatalf("%s: %s", input, res.Text)
		}
		return res.Text
	}
	plainBefore := text(`{"op":"describe_ontology"}`)
	nodeStatsRun(t, d, k.f.operator, computeAll)
	plain := text(`{"op":"describe_ontology"}`)
	if plain != plainBefore {
		t.Errorf("stored statistics changed the plain answer:\n%s\n%s", plainBefore, plain)
	}
	if off := text(`{"op":"describe_ontology","with_stats":false}`); off != plain {
		t.Errorf("with_stats:false differs from no argument")
	}
	if strings.Contains(plain, `"stats"`) {
		t.Errorf("without with_stats the answer carries stats: %s", plain)
	}
	var counted, stats map[string]any
	if err := json.Unmarshal([]byte(text(`{"op":"describe_ontology","with_counts":true}`)), &counted); err != nil {
		t.Fatal(err)
	}
	withStats := text(`{"op":"describe_ontology","with_stats":true}`)
	if !strings.Contains(withStats, `"stats"`) {
		t.Fatalf("with_stats carries no stats: %s", withStats)
	}
	if err := json.Unmarshal([]byte(withStats), &stats); err != nil {
		t.Fatal(err)
	}
	stripStatsFields(stats)
	if !reflect.DeepEqual(counted, stats) {
		t.Errorf("with_stats changed more than the stats fields:\n counts %v\n stats  %v", counted, stats)
	}
}

func stripStatsFields(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "stats")
		delete(x, "series")
		for _, e := range x {
			stripStatsFields(e)
		}
	case []any:
		for _, e := range x {
			stripStatsFields(e)
		}
	}
}
