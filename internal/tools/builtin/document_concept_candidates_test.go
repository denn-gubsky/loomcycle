package builtin

import (
	"fmt"
	"strings"
	"testing"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// candidateFixture is the big-tree classify fixture (29 concepts, more than one
// request names) on a store with vectors, with an embedder that puts Alpha3's
// definition along x, Gamma's near it, Alpha and its other children at 45 degrees,
// and every other definition along w.
type candidateFixture struct {
	*classifyFixture
	vs  *vectorStore
	emb *defEmbedder
}

func newCandidateFixture(t *testing.T) *candidateFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	vs := newVectorStore(d.Store)
	d.Store = &ftsStore{vs}
	emb := newDefEmbedder(
		defRule{word: "alpha3", vec: []float32{1, 0, 0, 0}},
		defRule{word: "gamma", vec: []float32{0.9, 0.1, 0, 0}},
		defRule{word: "alpha", vec: []float32{0.5, 0.5, 0, 0}},
	)
	d.Embedder = emb
	ctx := operatorCtx(installTenantOntology(t, d, base, bigTreeOntologyMD(), memrank.OntologyConfirmedStatus))
	return &candidateFixture{classifyFixture: &classifyFixture{t: t, d: d, ctx: ctx}, vs: vs, emb: emb}
}

// annotatedDoc makes a hand-typed tenant document (so classify asks the concepts
// alone) whose document annotation vector is vec.
func (f *candidateFixture) annotatedDoc(path string, vec []float32) string {
	f.t.Helper()
	docID, root := f.doc("tenant", "A note "+path, path, "Order", [2]string{"Part", "text"})
	if vec == nil {
		return docID
	}
	in := annInputs(f.t, f.d, f.ctx, `"document_id":"`+docID+`"`)
	if _, r := docExec(f.t, f.d, f.ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","document_id":%q,"brief":"a note","abstract":"About a note.","inputs_sha256":%q}`,
		docID, in["inputs_sha256"])); r.IsError {
		f.t.Fatalf("set_annotation: %s", r.Text)
	}
	key, mscope, err := f.d.ontologyTenantKey(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.vs.MemoryEmbedSet(f.ctx, "", mscope, annotationMemScopeID(mscope, key), annotationKey(root, annotationLevelDocument),
		store.MemoryEmbedding{Provider: "fake", Model: "m1", Dimension: len(vec), Vector: vec, CreatedAt: time.Now()}); err != nil {
		f.t.Fatal(err)
	}
	return docID
}

func (f *candidateFixture) computeDefinitions() {
	f.t.Helper()
	ans := nodeStatsRun(f.t, f.d, f.ctx, computeAll)
	if ans.Definitions == nil || ans.Definitions.Embedded == 0 {
		f.t.Fatalf("precondition: compute stored no definition: %+v", ans.Definitions)
	}
}

// TestClassify_CandidatesNarrowWhatTheModelIsAsked — with definition vectors stored
// and the document's annotation vector at hand, the 29-concept vocabulary is asked in
// ONE request of the nearest concepts, nearest first, instead of level by level; a
// concept said yes with a narrower one also said yes is left out.
func TestClassify_CandidatesNarrowWhatTheModelIsAsked(t *testing.T) {
	f := newCandidateFixture(t)
	f.computeDefinitions()
	stub := &treeStub{yes: map[string]bool{"Alpha": true, "Alpha3": true}}
	f.d.Classifier = stub
	docID := f.annotatedDoc("/kb/n/1", []float32{1, 0, 0, 0})
	a := f.classify(f.ctx, "tenant", docID, nil)

	names := stub.askedNames()
	if len(names) != 1 || len(names[0]) > memrank.ClassifyMaxConcepts || names[0][0] != "Alpha3" || names[0][1] != "Gamma" {
		t.Fatalf("asked %v; want one request, Alpha3 then Gamma first", names)
	}
	step := modelStep(a)
	if step == nil || len(step.Nearest) != len(names[0]) || step.Nearest[0].Concept != "Alpha3" || step.Nearest[0].Definition == nil || *step.Nearest[0].Definition < 0.999 {
		t.Fatalf("model step = %+v", step)
	}
	if !strings.Contains(step.Reason, "nearest the document's annotation") {
		t.Errorf("reason = %q", step.Reason)
	}
	if got := conceptNames(a.Concepts); strings.Join(got, ",") != "Alpha3" {
		t.Errorf("concepts = %v; want Alpha3 alone (Alpha is broader)", got)
	}
}

// TestClassify_CandidatesFallBackToTheTreeWalk — the tree is walked as before when
// the store keeps no vectors, when no definition has been stored yet, and when the
// document has no annotation vector.
func TestClassify_CandidatesFallBackToTheTreeWalk(t *testing.T) {
	walked := func(t *testing.T, f *candidateFixture, docID string) {
		t.Helper()
		stub := &treeStub{yes: map[string]bool{"Alpha": true, "Alpha3": true, "Gamma": true}}
		f.d.Classifier = stub
		a := f.classify(f.ctx, "tenant", docID, nil)
		names := stub.askedNames()
		if len(names) != 2 || strings.Join(names[0], ",") != "Alpha,Beta,Gamma" {
			t.Fatalf("asked %v; want the tree walk", names)
		}
		if step := modelStep(a); step == nil || step.Nearest != nil || !strings.Contains(step.Reason, "down the concept tree") {
			t.Errorf("model step = %+v; want the walk's", step)
		}
	}
	t.Run("no definitions stored", func(t *testing.T) {
		f := newCandidateFixture(t)
		walked(t, f, f.annotatedDoc("/kb/n/1", []float32{1, 0, 0, 0}))
	})
	t.Run("no annotation vector", func(t *testing.T) {
		f := newCandidateFixture(t)
		f.computeDefinitions()
		walked(t, f, f.annotatedDoc("/kb/n/1", nil))
	})
	t.Run("store without vectors", func(t *testing.T) {
		f := newCandidateFixture(t)
		f.computeDefinitions()
		docID := f.annotatedDoc("/kb/n/1", []float32{1, 0, 0, 0})
		f.vs.enabled = false
		walked(t, f, docID)
	})
}

// TestClassify_CandidatesIncludeTheNearestCentroids — a concept whose definition is
// far but whose members' centroid is near the document is asked too: definitions
// and centroids are taken in turn.
func TestClassify_CandidatesIncludeTheNearestCentroids(t *testing.T) {
	f := newCandidateFixture(t)
	read, _ := f.d.tenantOntologyRead(f.ctx)
	beta7 := ""
	for _, c := range inForceConcepts(read) {
		if c.Name == "Beta7" {
			beta7 = c.ChunkID
		}
	}
	// Three members of Beta7 pointing along z: its centroid, not its definition (w),
	// is what is near a document along z.
	for i := 1; i <= 3; i++ {
		m := f.annotatedDoc(fmt.Sprintf("/kb/m/%d", i), []float32{0, 0, 1, 0})
		chunk := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": m, "title": "Body", "body": "text"})["id"].(string)
		f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": chunk, "to_id": beta7, "kind": "subject"})
	}
	f.computeDefinitions()
	stub := &treeStub{yes: map[string]bool{"Beta7": true}}
	f.d.Classifier = stub
	a := f.classify(f.ctx, "tenant", f.annotatedDoc("/kb/n/2", []float32{0, 0, 1, 0}), nil)
	// Every definition is orthogonal to z, so the definitions tie. By centroid,
	// Beta7 ties with its parent Beta, whose centroid counts its descendants'
	// members; the two lists are taken in turn, so both are among the first four.
	step := modelStep(a)
	found := false
	for i, c := range step.Nearest {
		if c.Concept == "Beta7" && i < 4 && c.Centroid != nil && *c.Centroid > 0.999 {
			found = true
		}
	}
	if !found {
		t.Fatalf("nearest = %+v; want Beta7 among the first four, by its centroid", step.Nearest)
	}
	if got := conceptNames(a.Concepts); strings.Join(got, ",") != "Beta7" {
		t.Errorf("concepts = %v; want Beta7", got)
	}
}
