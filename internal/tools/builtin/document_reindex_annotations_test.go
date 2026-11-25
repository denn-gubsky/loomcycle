package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The operator re-index pass over annotation index rows. The write path embeds a
// record's row once; an embed that failed then, or a later change of embed model,
// left the row with no usable vector for good until this pass existed.

type annRowRef struct {
	chunk, level string
}

// annReindexFixture is seedAnnSearch (three document records and one path record,
// tenant scope) with a counting embedder, plus where the index rows live.
func annReindexFixture(t *testing.T) (*Document, context.Context, *countingEmbedder, []annRowRef, func(annRowRef) (store.MemoryEmbedding, bool), func(annRowRef)) {
	t.Helper()
	d, op, _, _ := seedAnnSearch(t)
	key, mscope, err := d.resolveScope(op, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	scopeID := annotationMemScopeID(mscope, key)
	out, r := docExec(t, d, op, `{"op":"list_annotations","scope":"tenant"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	var refs []annRowRef
	for _, a := range out["annotations"].([]any) {
		m := a.(map[string]any)
		refs = append(refs, annRowRef{m["chunk_id"].(string), m["level"].(string)})
	}
	if len(refs) != 4 {
		t.Fatalf("fixture: %d annotation records, want 4", len(refs))
	}
	get := func(a annRowRef) (store.MemoryEmbedding, bool) {
		e, err := d.Store.MemoryEmbedGet(op, direntTenant(op), mscope, scopeID, annotationKey(a.chunk, a.level))
		return e, err == nil
	}
	drop := func(a annRowRef) {
		if err := d.Store.MemoryEmbedDelete(op, direntTenant(op), mscope, scopeID, annotationKey(a.chunk, a.level)); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range refs {
		if _, ok := get(a); !ok {
			t.Fatalf("fixture: record %v has no vector after its write", a)
		}
	}
	emb := &countingEmbedder{Embedder: d.Embedder}
	d.Embedder = emb
	return d, op, emb, refs, get, drop
}

// TestReindexScope_EmbedsAnAnnotationRowThatHasNoVector — a row whose embed failed at
// write time gets its vector from the pass, under the text the write path derives.
func TestReindexScope_EmbedsAnAnnotationRowThatHasNoVector(t *testing.T) {
	d, op, _, refs, get, drop := annReindexFixture(t)
	want, _ := get(refs[0])
	drop(refs[0])

	rep, err := d.ReindexScope(op, "tenant", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AnnotationsExamined != 4 || rep.AnnotationsReindexed != 1 || rep.AnnotationsUpToDate != 3 || rep.AnnotationsFailed != 0 || rep.More {
		t.Errorf("report: %+v", rep)
	}
	got, ok := get(refs[0])
	if !ok {
		t.Fatal("the row still has no vector after the re-index")
	}
	if got.EmbedText != want.EmbedText || got.Provider != "fake" || got.Model != "m1" {
		t.Errorf("re-embedded as %s/%s %q, want fake/m1 %q", got.Provider, got.Model, got.EmbedText, want.EmbedText)
	}
}

// TestReindexScope_ReembedsAnAnnotationRowOfAnotherModel — a vector under the right
// text but from another embed model is unusable to every reader that compares models.
func TestReindexScope_ReembedsAnAnnotationRowOfAnotherModel(t *testing.T) {
	d, op, emb, refs, get, _ := annReindexFixture(t)
	old, _ := get(refs[1])
	key, mscope, _ := d.resolveScope(op, "tenant")
	old.Provider, old.Model = "other", "m0"
	if err := d.Store.MemoryEmbedSet(op, direntTenant(op), mscope, annotationMemScopeID(mscope, key),
		annotationKey(refs[1].chunk, refs[1].level), old); err != nil {
		t.Fatal(err)
	}

	rep, err := d.ReindexScope(op, "tenant", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AnnotationsReindexed != 1 || emb.n.Load() != 1 {
		t.Errorf("reindexed %d with %d embed calls, want 1 and 1: %+v", rep.AnnotationsReindexed, emb.n.Load(), rep)
	}
	if got, _ := get(refs[1]); got.Provider != "fake" || got.Model != "m1" {
		t.Errorf("row still embedded by %s/%s", got.Provider, got.Model)
	}
}

// TestReindexScope_UpToDateAnnotationRowsCostNoEmbedCall — the pass is idempotent: a
// store whose rows are current is read and left alone.
func TestReindexScope_UpToDateAnnotationRowsCostNoEmbedCall(t *testing.T) {
	d, op, emb, _, _, _ := annReindexFixture(t)
	rep, err := d.ReindexScope(op, "tenant", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AnnotationsExamined != 4 || rep.AnnotationsUpToDate != 4 || rep.AnnotationsReindexed != 0 || emb.n.Load() != 0 {
		t.Errorf("a current store: %+v with %d embed calls", rep, emb.n.Load())
	}
}

// TestReindexScope_AnnotationDryRunCountsButWritesNothing.
func TestReindexScope_AnnotationDryRunCountsButWritesNothing(t *testing.T) {
	d, op, emb, refs, get, drop := annReindexFixture(t)
	drop(refs[2])
	rep, err := d.ReindexScope(op, "tenant", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.AnnotationsReindexed != 1 {
		t.Errorf("dry run: %+v", rep)
	}
	if _, ok := get(refs[2]); ok || emb.n.Load() != 0 {
		t.Errorf("the dry run wrote (vector present=%v, %d embed calls)", ok, emb.n.Load())
	}
}

// TestReindexScope_AnnotationPhasePagesByCursor — `limit` bounds the rows changed per
// call across the annotation phase too, and its cursor resumes there.
func TestReindexScope_AnnotationPhasePagesByCursor(t *testing.T) {
	d, op, _, refs, get, drop := annReindexFixture(t)
	for _, a := range refs {
		drop(a)
	}
	total, cursor := 0, ""
	for calls := 0; ; calls++ {
		if calls > 6 {
			t.Fatal("paging did not terminate")
		}
		rep, err := d.ReindexScope(op, "tenant", cursor, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		total += rep.AnnotationsReindexed
		if !rep.More {
			break
		}
		if !strings.HasPrefix(rep.NextCursor, reindexAnnotationCursor) {
			t.Fatalf("cursor %q does not resume in the annotation phase", rep.NextCursor)
		}
		cursor = rep.NextCursor
	}
	if total != 4 {
		t.Errorf("paging re-indexed %d annotation rows, want 4", total)
	}
	for _, a := range refs {
		if _, ok := get(a); !ok {
			t.Errorf("%v has no vector after paging", a)
		}
	}
}

// TestReindexScope_DeclaredNodeRecordKeepsNoRow — a node with no member holds nothing
// and has no index row: the pass never gives it one, and removes one left behind.
func TestReindexScope_DeclaredNodeRecordKeepsNoRow(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	d.Embedder = newFakeEmbedder("fake", "m1", "risk", "compliance", "filings", "vendor")
	op := k.f.operator
	comp := k.f.concepts["file-plan/compliance"]
	if rec := nodeWrite(t, d, op, "concept", comp, ""); rec["basis"] != "declared" {
		t.Fatalf("record = %v, want basis declared", rec)
	}
	key, mscope, err := d.ontologyTenantKey(op)
	if err != nil {
		t.Fatal(err)
	}
	scopeID, rowKey := annotationMemScopeID(mscope, key), annotationKey(comp, annotationLevelConcept)
	hasRow := func() bool {
		_, err := d.Store.MemoryGet(op, direntTenant(op), mscope, scopeID, rowKey)
		return err == nil
	}

	rep, err := d.ReindexScope(op, "tenant", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// The corpus's other records were indexed by its own fixture vectors, so the pass
	// may re-embed those; only the declared record's row is at issue here.
	if hasRow() || rep.AnnotationsExamined == 0 {
		t.Fatalf("a declared record got a row (or nothing was examined): %+v", rep)
	}

	// A row left from before the node lost its members.
	if err := d.Store.MemorySet(op, direntTenant(op), mscope, scopeID, rowKey, []byte(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	if err := d.Store.MemoryEmbedSet(op, direntTenant(op), mscope, scopeID, rowKey, store.MemoryEmbedding{
		Provider: "fake", Model: "m1", Dimension: 4, Vector: []float32{1, 0, 0, 0}, EmbedText: "compliance", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	dry, err := d.ReindexScope(op, "tenant", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.AnnotationsUnindexed != 1 || !hasRow() {
		t.Fatalf("dry run: unindexed=%d, row kept=%v; want 1 and true", dry.AnnotationsUnindexed, hasRow())
	}
	rep, err = d.ReindexScope(op, "tenant", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AnnotationsUnindexed != 1 || hasRow() {
		t.Errorf("leftover row of a declared record: unindexed=%d, row kept=%v", rep.AnnotationsUnindexed, hasRow())
	}
}
