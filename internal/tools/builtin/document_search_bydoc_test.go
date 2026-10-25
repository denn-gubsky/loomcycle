package builtin

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Filtered search by document id: a filter that names only documents is sent as
// document ids once every document row of the scope stores its document id, and
// as the chunk-id list otherwise. These tests pin both halves and every writer of
// the stored id.

// legLists reports, across every leg the spy saw, the chunk-id and document-id
// lists the store was sent.
func legLists(spy *legSpy) (chunkLists, docLists [][]string) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	for _, f := range spy.filters {
		chunkLists = append(chunkLists, append(append([]string{}, f.IDs...), f.ExcludeIDs...))
		docLists = append(docLists, append(append([]string{}, f.DocumentIDs...), f.ExcludeDocumentIDs...))
	}
	return chunkLists, docLists
}

// assertStoredDocumentIDs checks that every embedded document row in the fake
// stores exactly the document its chunk belongs to in SQL Memory.
func assertStoredDocumentIDs(t *testing.T, d *Document, ctx context.Context, vs *vectorStore) int {
	t.Helper()
	key := sidecarScope(t, d, ctx)
	vs.mu.Lock()
	rows := map[string]string{}
	for k, e := range vs.embeds {
		if i := strings.Index(k, "|doc."); i >= 0 {
			rows[k[i+1:]] = e.DocumentID
		}
	}
	vs.mu.Unlock()
	for rowKey, got := range rows {
		chunkID := ChunkIDFromBodyKey(rowKey)
		if chunkID == "" {
			chunkID, _ = memrank.UnitChunkID(rowKey)
		}
		res, err := d.query(ctx, key, `SELECT document_id FROM chunks WHERE id = ?`, chunkID)
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("chunk of %s: %v %v", rowKey, res, err)
		}
		if want := asStr(res.Rows[0][0]); got != want {
			t.Errorf("%s stores document %q, its chunk is in %q", rowKey, got, want)
		}
	}
	return len(rows)
}

func TestDocumentSearch_DocumentOnlyFilterSendsDocumentIDs(t *testing.T) {
	c, _, targets := normsCorpus(t)
	norms := c.docs["/qg/norms/ed1"]
	c.spy.reset()
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/qg/norms"})
	if !reflect.DeepEqual(setOf(ids), setOf(targets)) {
		t.Fatalf("hits = %v, want the five norm clauses", ids)
	}
	chunkLists, docLists := legLists(c.spy)
	if len(docLists) == 0 {
		t.Fatal("no leg ran")
	}
	for i := range docLists {
		if len(chunkLists[i]) != 0 || !reflect.DeepEqual(docLists[i], []string{norms}) {
			t.Errorf("leg %d was sent chunk ids %v and document ids %v, want only [%s]", i, chunkLists[i], docLists[i], norms)
		}
	}
	// The echo is the chunk-id path's: the same filter reports the same matches
	// whichever list travels.
	c.vs.noDocumentIDs = true
	_, chunkEcho := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/qg/norms"})
	for _, k := range []string{"matched_chunks", "matched_documents"} {
		if echoInt(echo, k) != echoInt(chunkEcho, k) {
			t.Errorf("%s = %d by document, %d by chunk", k, echoInt(echo, k), echoInt(chunkEcho, k))
		}
	}
}

func TestDocumentSearch_ExclusionOnlyFilterSendsADocumentDenylist(t *testing.T) {
	c, decoys, _ := normsCorpus(t)
	c.spy.reset()
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10,
		"exclude_under_paths": []string{"/board"}})
	for _, id := range ids {
		if setOf(decoys)[id] {
			t.Fatalf("an excluded board chunk came back: %v", ids)
		}
	}
	want := []string{c.docs["/board/a"], c.docs["/board/b"], c.docs["/board/c"]}
	sort.Strings(want)
	c.spy.mu.Lock()
	for i, f := range c.spy.filters {
		if len(f.IDs)+len(f.ExcludeIDs)+len(f.DocumentIDs) != 0 || !reflect.DeepEqual(f.ExcludeDocumentIDs, want) {
			t.Errorf("leg %d filter = %+v, want only ExcludeDocumentIDs %v", i, f, want)
		}
	}
	c.spy.mu.Unlock()
	if echoInt(echo, "matched_documents") != 1 {
		t.Errorf("echo = %v, want the one norms document matched", echo)
	}
}

func TestDocumentSearch_ChunkPredicateKeepsTheChunkIDList(t *testing.T) {
	c, _, _ := normsCorpus(t)
	c.spy.reset()
	c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/qg/norms", "status": "in_force"})
	chunkLists, docLists := legLists(c.spy)
	for i := range docLists {
		if len(docLists[i]) != 0 {
			t.Errorf("leg %d was sent document ids %v for a filter that names a status", i, docLists[i])
		}
	}
	_ = chunkLists
}

// TestDocumentSearch_RowWithoutDocumentIDFallsBackUntilReindexed — a row embedded
// before the column existed stores no document id. The search must then use the
// chunk-id list (a document-id list would silently drop that row), and the
// re-index pass fills the id without re-embedding, after which the search uses
// document ids again.
func TestDocumentSearch_RowWithoutDocumentIDFallsBackUntilReindexed(t *testing.T) {
	c, _, targets := normsCorpus(t)
	key := sidecarScope(t, c.d, c.ctx)
	if _, err := c.vs.MemoryEmbedSetDocumentID(c.ctx, "", store.MemoryScopeUser, key.ScopeID, chunkBodyKeyPrefix, targets[:1], ""); err != nil {
		t.Fatal(err)
	}
	search := func() ([]string, [][]string, [][]string) {
		c.spy.reset()
		ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/qg/norms"})
		cl, dl := legLists(c.spy)
		return ids, cl, dl
	}
	ids, chunkLists, docLists := search()
	if !setOf(ids)[targets[0]] {
		t.Fatalf("the row without a document id dropped out of the search: %v", ids)
	}
	for i := range docLists {
		if len(docLists[i]) != 0 || len(chunkLists[i]) == 0 {
			t.Fatalf("leg %d: chunk ids %v, document ids %v — want the chunk-id fallback", i, chunkLists[i], docLists[i])
		}
	}
	embedsBefore := c.emb.n.Load()
	rep, err := c.d.ReindexScope(c.ctx, "user", "", 200, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Attributed != 1 || rep.Reindexed != 0 {
		t.Fatalf("reindex report = %+v, want 1 attributed and nothing re-embedded", rep)
	}
	if c.emb.n.Load() != embedsBefore {
		t.Error("filling a document id made an embedding call")
	}
	ids, chunkLists, docLists = search()
	if !setOf(ids)[targets[0]] {
		t.Fatalf("after the re-index the row is missing: %v", ids)
	}
	for i := range docLists {
		if len(chunkLists[i]) != 0 || len(docLists[i]) != 1 {
			t.Fatalf("leg %d after the re-index: chunk ids %v, document ids %v — want document ids", i, chunkLists[i], docLists[i])
		}
	}
}

func TestDocumentSearch_ACLLimitedCallerSendsOnlyReadableDocumentIDs(t *testing.T) {
	d, spy, _, ctx, s := aclCorpus(t)
	spy.reset()
	if _, res := docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting","under_path":"/kb"}`); res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	_, docLists := legLists(spy)
	if len(docLists) == 0 {
		t.Fatal("no leg ran")
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	for i, f := range spy.filters {
		if len(f.ExcludeDocumentIDs) != 0 || len(f.IDs)+len(f.ExcludeIDs) != 0 {
			t.Errorf("leg %d filter = %+v, want an allowlist of document ids only", i, f)
		}
		for _, id := range f.DocumentIDs {
			if id == s.secretDoc {
				t.Errorf("leg %d was sent the read-denied document %s", i, id)
			}
		}
		if !reflect.DeepEqual(f.DocumentIDs, []string{s.publicDoc}) {
			t.Errorf("leg %d document ids = %v, want only the readable %s", i, f.DocumentIDs, s.publicDoc)
		}
	}
}

// TestDocumentEmbedWriters_StoreTheChunksDocumentID — the writer census, end to
// end: create_chunk, update_chunk, import_md and derived units each store the
// document their chunk belongs to.
func TestDocumentEmbedWriters_StoreTheChunksDocumentID(t *testing.T) {
	c, _, targets := normsCorpus(t)
	c.exec(map[string]any{"op": "update_chunk", "id": targets[0], "body": "norm clause amended", "revision": 1})
	c.exec(map[string]any{"op": "import_md", "path": "/qg/imported", "markdown": "# Imported\n\n## One\n\nnorm clause one\n\n## Two\n\nnorm clause two\n"})
	if _, err := c.d.ReplaceUnits(c.ctx, "user", targets[1], []DerivedUnit{{Kind: memrank.UnitClaim, Text: "a norm clause claim"}}, UnitSource{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if n := assertStoredDocumentIDs(t, c.d, c.ctx, c.vs); n < 128 {
		t.Fatalf("only %d document rows checked — the fixture did not embed what it should", n)
	}
}

// TestHomeFactsUnderSubjects_MovesTheStoredDocumentID — homing re-files a subject
// and its facts into another document, the one writer that changes a chunk's
// document. Their embeddings must name the new document afterwards; a stale one
// would keep them in the old document's filtered searches.
func TestHomeFactsUnderSubjects_MovesTheStoredDocumentID(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	vs := newVectorStore(d.Store)
	d.Store = vs
	d.Embedder = newFakeEmbedder("fake", "m1", "denn", "go", "releases")
	shared, subj, fact, _ := seedOldShape(t, d, ctx)
	key := sidecarScope(t, d, ctx)
	if got, _ := vs.MemoryEmbedGet(ctx, "", store.MemoryScopeUser, key.ScopeID, chunkBodyKey(fact)); got.DocumentID != shared {
		t.Fatalf("precondition: the fact's row stores %q, want the shared document %q", got.DocumentID, shared)
	}
	if _, err := d.HomeFactsUnderSubjects(ctx, "tnt", store.MemoryScopeUser, "u1", false); err != nil {
		t.Fatalf("home: %v", err)
	}
	shape := homedShape(t, d, ctx, key)
	if shape[fact][0] == shared {
		t.Fatalf("precondition: the fact did not move")
	}
	assertStoredDocumentIDs(t, d, ctx, vs)
	_ = subj
}

// TestDocumentSearchByDocument_CoreBothTiers runs the document-id path's SQL Memory
// statements (the per-document chunk counts, the scope's document listing) on both
// tiers: an include filter and an exclusion-only filter each send document ids and
// echo the counts.
func TestDocumentSearchByDocument_CoreBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertSearchByDocumentCore(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertSearchByDocumentCore(t, d, ctx)
	})
}

func assertSearchByDocumentCore(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	vs := newVectorStore(d.Store)
	spy := &legSpy{ftsStore: &ftsStore{vs}}
	d.Store = spy
	d.Embedder = newFakeEmbedder("fake", "m1", "norm", "clause")
	c := &corpus{t: t, d: d, vs: vs, spy: spy, ctx: ctx, docs: map[string]string{}, byID: map[string][]string{}}
	board := c.doc("/board/a")
	for i := 0; i < 6; i++ {
		c.chunk(board, "norm clause", nil)
	}
	norms := c.doc("/qg/norms/ed1")
	target := c.chunk(norms, "norm clause target", nil)

	spy.reset()
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/qg/norms"})
	if !setOf(ids)[target] || echoInt(echo, "matched_documents") != 1 || echoInt(echo, "matched_chunks") != 2 {
		t.Fatalf("include: hits %v echo %v, want the target; 1 document, 2 chunks (root + target)", ids, echo)
	}
	_, docLists := legLists(spy)
	for i, l := range docLists {
		if !reflect.DeepEqual(l, []string{norms}) {
			t.Errorf("include leg %d document ids = %v, want [%s]", i, l, norms)
		}
	}
	spy.reset()
	_, echo = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "exclude_under_path": "/board"})
	if echoInt(echo, "matched_documents") != 1 || echoInt(echo, "matched_chunks") != 2 {
		t.Fatalf("exclusion: echo %v, want 1 document, 2 chunks", echo)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	for i, f := range spy.filters {
		if !reflect.DeepEqual(f.ExcludeDocumentIDs, []string{board}) {
			t.Errorf("exclusion leg %d = %+v, want ExcludeDocumentIDs [%s]", i, f, board)
		}
	}
}
