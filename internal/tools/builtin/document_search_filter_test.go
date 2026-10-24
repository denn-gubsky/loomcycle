package builtin

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// hexIDRe matches the 32-hex ids documents and chunks are minted with.
var hexIDRe = regexp.MustCompile(`[0-9a-f]{32}`)

// normaliseIDs replaces every id with its order of first appearance, so a
// response from a fresh fixture can be compared byte for byte with a recorded one.
func normaliseIDs(s string) string {
	seen := map[string]string{}
	return hexIDRe.ReplaceAllStringFunc(s, func(id string) string {
		if n, ok := seen[id]; ok {
			return n
		}
		n := "<id" + string(rune('A'+len(seen))) + ">"
		seen[id] = n
		return n
	})
}

// goldenFixture is three chunks whose vector and word scores are all distinct,
// so the order — and therefore the response bytes — is fully determined.
func goldenFixture(t *testing.T) ([]string, func(string) string) {
	t.Helper()
	d, vs, ctx := mermaidDocFixture(t, "alpha", "beta", "gamma")
	d.Store = &ftsStore{vs}
	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Golden","path":"/gold/doc"}`))
	docID := resultField(res, "document_id")
	var ids []string
	for _, c := range []struct{ title, body string }{
		{"One", "alpha beta beta"},
		{"Two", "alpha beta gamma"},
		{"Three", "alpha gamma"},
	} {
		b, _ := json.Marshal(map[string]any{"op": "create_chunk", "document_id": docID, "title": c.title, "body": c.body})
		r, err := d.Execute(ctx, b)
		if err != nil || r.IsError {
			t.Fatalf("create_chunk %q: %v %s", c.title, err, r.Text)
		}
		ids = append(ids, resultField(r, "id"))
	}
	return ids, func(input string) string {
		t.Helper()
		out, err := d.Execute(ctx, json.RawMessage(input))
		if err != nil || out.IsError {
			t.Fatalf("%s: %v %s", input, err, out.Text)
		}
		return normaliseIDs(out.Text)
	}
}

// goldenNoFilterSearch was recorded from `main` at acfb1d88, before search took
// any filter. A filter-free call must keep producing exactly these bytes.
const goldenNoFilterSearch = `{"chunks":[{"chunk_id":"<idA>","document_id":"<idB>","rank_score":0.03333333333333333,"score":1.0000000000000002,"title":"One"},{"chunk_id":"<idC>","document_id":"<idB>","rank_score":0.03278688524590164,"score":0.8164965809277261,"title":"Two"},{"chunk_id":"<idD>","document_id":"<idB>","rank_score":0.016129032258064516,"score":0.5000000000000001,"title":"Three"}]}`

// TestDocumentSearch_NoFilterIsByteIdentical — a search that names no filter is
// the search it was before filters existed: same pool, same order, same bytes,
// and no `filter` key.
func TestDocumentSearch_NoFilterIsByteIdentical(t *testing.T) {
	_, search := goldenFixture(t)
	got := search(`{"op":"search","query":"alpha beta","limit":5}`)
	if got != goldenNoFilterSearch {
		t.Errorf("filter-free search changed:\n got %s\nwant %s", got, goldenNoFilterSearch)
	}
}

// goldenNoFilterRelated: the same recording for op=related from the chunk "One".
const goldenNoFilterRelated = `{"related":[{"chunk_id":"<idA>","document_id":"<idB>","score":0.8164965809277261,"title":"Two"},{"chunk_id":"<idC>","document_id":"<idB>","score":0.5000000000000001,"title":"Three"}]}`

func TestDocumentRelated_NoFilterIsByteIdentical(t *testing.T) {
	ids, exec := goldenFixture(t)
	got := exec(`{"op":"related","id":"` + ids[0] + `","limit":5}`)
	if got != goldenNoFilterRelated {
		t.Errorf("filter-free related changed:\n got %s\nwant %s", got, goldenNoFilterRelated)
	}
}

// ---- the corpus fixture -----------------------------------------------------

// legSpy records what each store leg was asked and what it returned, so a test can
// assert on ONE leg rather than on the fused result both legs feed.
type legSpy struct {
	*ftsStore
	mu      sync.Mutex
	filters []store.MemorySearchFilter
	ftsKeys [][]string
}

func (s *legSpy) MemoryEmbedSearch(ctx context.Context, tenant string, scope store.MemoryScope, scopeID string,
	f store.MemorySearchFilter, q []float32, topK int) ([]store.MemorySearchEntry, error) {
	s.mu.Lock()
	s.filters = append(s.filters, f)
	s.mu.Unlock()
	return s.ftsStore.MemoryEmbedSearch(ctx, tenant, scope, scopeID, f, q, topK)
}

func (s *legSpy) MemoryFullTextSearch(ctx context.Context, tenant string, scope store.MemoryScope, scopeID string,
	f store.MemorySearchFilter, q string, limit int) ([]store.MemorySearchEntry, error) {
	rows, err := s.ftsStore.MemoryFullTextSearch(ctx, tenant, scope, scopeID, f, q, limit)
	var ks []string
	for _, r := range rows {
		ks = append(ks, r.Key)
	}
	s.mu.Lock()
	s.filters = append(s.filters, f)
	s.ftsKeys = append(s.ftsKeys, ks)
	s.mu.Unlock()
	return rows, err
}

func (s *legSpy) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filters, s.ftsKeys = nil, nil
}

type corpus struct {
	t    *testing.T
	d    *Document
	vs   *vectorStore
	spy  *legSpy
	emb  *countingEmbedder
	ctx  context.Context
	docs map[string]string   // path → document id
	byID map[string][]string // document id → its content chunk ids
}

func newCorpus(t *testing.T, vocab ...string) *corpus {
	t.Helper()
	d, vs, ctx := mermaidDocFixture(t, vocab...)
	spy := &legSpy{ftsStore: &ftsStore{vs}}
	d.Store = spy
	emb := &countingEmbedder{Embedder: d.Embedder}
	d.Embedder = emb
	return &corpus{t: t, d: d, vs: vs, spy: spy, emb: emb, ctx: ctx, docs: map[string]string{}, byID: map[string][]string{}}
}

func (c *corpus) exec(v map[string]any) map[string]any {
	c.t.Helper()
	b, _ := json.Marshal(v)
	r, err := c.d.Execute(c.ctx, b)
	if err != nil || r.IsError {
		c.t.Fatalf("%v: %v %s", v["op"], err, r.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(r.Text), &out)
	return out
}

// doc creates a document at path (its title carries no query word).
func (c *corpus) doc(path string) string {
	c.t.Helper()
	out := c.exec(map[string]any{"op": "create_document", "title": "Doc", "path": path})
	id := out["document_id"].(string)
	c.docs[path] = id
	return id
}

// chunk adds a content chunk; extra carries type/status/tags.
func (c *corpus) chunk(docID, body string, extra map[string]any) string {
	c.t.Helper()
	v := map[string]any{"op": "create_chunk", "document_id": docID, "title": "Part", "body": body}
	for k, x := range extra {
		v[k] = x
	}
	id := c.exec(v)["id"].(string)
	c.byID[docID] = append(c.byID[docID], id)
	return id
}

// call runs op=search / op=related and returns the hit ids in order and the echo.
func (c *corpus) call(v map[string]any) ([]string, map[string]any) {
	c.t.Helper()
	out := c.exec(v)
	key := "chunks"
	if v["op"] == "related" {
		key = "related"
	}
	var ids []string
	for _, h := range out[key].([]any) {
		ids = append(ids, h.(map[string]any)["chunk_id"].(string))
	}
	echo, _ := out["filter"].(map[string]any)
	return ids, echo
}

func setOf(ids ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, l := range ids {
		for _, id := range l {
			m[id] = true
		}
	}
	return m
}

func echoInt(echo map[string]any, k string) int {
	n, _ := echo[k].(float64)
	return int(n)
}

// normsCorpus is the shape the client described: a few norm clauses under
// /qg/norms inside a scope dominated by board files that match the query BETTER
// on both legs — an identical vector, and the query's words twice over. 120
// decoys over three documents, so each leg's 51-row cap fills with them.
func normsCorpus(t *testing.T) (*corpus, []string, []string) {
	t.Helper()
	c := newCorpus(t, "norm", "clause", "decision")
	var decoys []string
	for _, p := range []string{"/board/a", "/board/b", "/board/c"} {
		doc := c.doc(p)
		for i := 0; i < 40; i++ {
			decoys = append(decoys, c.chunk(doc, "norm clause norm clause boardfile", nil))
		}
	}
	norms := c.doc("/qg/norms/ed1")
	var targets []string
	for i := 0; i < 5; i++ {
		targets = append(targets, c.chunk(norms, "norm clause decision normtext", nil))
	}
	return c, decoys, targets
}

// ---- P1 tests ---------------------------------------------------------------

func TestDocumentSearch_UnderPathPrefiltersBeforeTheLegLimit(t *testing.T) {
	c, _, targets := normsCorpus(t)
	want := setOf(targets)

	// Precondition: unfiltered, the decoys fill the top ten.
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10})
	for _, id := range ids {
		if want[id] {
			t.Fatalf("precondition: a target reached the unfiltered top ten")
		}
	}
	if echo != nil {
		t.Errorf("a filter-free search carries a filter echo: %v", echo)
	}

	ids, echo = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/qg/norms"})
	if len(ids) != len(targets) {
		t.Fatalf("under_path search returned %d hits, want the %d norm clauses: %v", len(ids), len(targets), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("under_path search returned %s, outside /qg/norms", id)
		}
	}
	// The document's root chunk is a chunk of it too (with no body, so never a hit).
	if echoInt(echo, "matched_documents") != 1 || echoInt(echo, "matched_chunks") != len(targets)+1 || echo["inverted"] != false {
		t.Errorf("echo = %v, want 1 document, %d chunks, not inverted", echo, len(targets)+1)
	}
	applied, _ := echo["applied"].(map[string]any)
	if paths, _ := applied["under_paths"].([]any); len(paths) != 1 || paths[0] != "/qg/norms" {
		t.Errorf("applied = %v, want under_paths [/qg/norms]", applied)
	}
}

func TestDocumentSearch_FullTextLegHonoursTheFilter(t *testing.T) {
	// No query word is in the embedder's vocabulary, so only the word leg can rank.
	c := newCorpus(t, "unrelated")
	minutes := c.doc("/qg/decisions/minutes")
	for i := 0; i < 60; i++ {
		c.chunk(minutes, "protocol 86 86 decided", nil)
	}
	target := c.chunk(c.doc("/qg/decisions/protocols"), "protocol 86 decided", nil)

	c.spy.reset()
	c.call(map[string]any{"op": "search", "query": "protocol 86", "limit": 5})
	for _, ks := range c.spy.ftsKeys {
		for _, k := range ks {
			if k == chunkBodyKey(target) {
				t.Fatal("precondition: the target reached the unfiltered word leg")
			}
		}
	}

	c.spy.reset()
	ids, _ := c.call(map[string]any{"op": "search", "query": "protocol 86", "limit": 5, "under_path": "/qg/decisions/protocols"})
	if len(ids) == 0 || ids[0] != target {
		t.Errorf("filtered search = %v, want the protocol %s first", ids, target)
	}
	saw := false
	for _, ks := range c.spy.ftsKeys {
		for _, k := range ks {
			if k == chunkBodyKey(target) {
				saw = true
			} else if strings.HasPrefix(k, chunkBodyKeyPrefix) {
				t.Errorf("the word leg returned %s, outside the filter", k)
			}
		}
	}
	if !saw {
		t.Error("the word leg never returned the target")
	}
}

func TestDocumentSearch_UnitLegHonoursTheFilter(t *testing.T) {
	c := newCorpus(t, "norm", "clause", "zebra")
	target := c.chunk(c.doc("/qg/norms/ed1"), "norm clause", nil)
	decoy := c.chunk(c.doc("/board/x"), "board minutes", nil)
	// The decoy is reachable ONLY through a derived unit that says what its body
	// does not.
	ukey := memrank.UnitKey(decoy, memrank.UnitClaim, 0)
	uv, _ := json.Marshal(memrank.UnitValue{ChunkID: decoy, Kind: memrank.UnitClaim, Text: "zebra"})
	if err := c.vs.Store.MemorySet(c.ctx, "tnt", store.MemoryScopeUser, "u1", ukey, uv, 0); err != nil {
		t.Fatal(err)
	}
	vec, _ := c.emb.Embedder.Embed(c.ctx, []string{"zebra"})
	if err := c.vs.MemoryEmbedSet(c.ctx, "tnt", store.MemoryScopeUser, "u1", ukey, store.MemoryEmbedding{
		Provider: "fake", Model: "m1", Dimension: len(vec[0]), Vector: vec[0], EmbedText: "zebra",
	}); err != nil {
		t.Fatal(err)
	}

	ids, _ := c.call(map[string]any{"op": "search", "query": "zebra norm", "limit": 5})
	if !setOf(ids)[decoy] {
		t.Fatalf("precondition: the decoy is found through its unit unfiltered (%v)", ids)
	}
	ids, _ = c.call(map[string]any{"op": "search", "query": "zebra norm", "limit": 5, "under_path": "/qg/norms"})
	if setOf(ids)[decoy] || !setOf(ids)[target] {
		t.Errorf("filtered search = %v, want the target and never the decoy found through its unit", ids)
	}
}

func TestDocumentRelated_UnderPathPrefilters(t *testing.T) {
	c, decoys, targets := normsCorpus(t)
	ids, _ := c.call(map[string]any{"op": "related", "id": decoys[0], "limit": 5})
	if setOf(ids)[targets[0]] || len(ids) != 5 {
		t.Fatalf("precondition: a decoy's neighbours are decoys (%v)", ids)
	}
	ids, echo := c.call(map[string]any{"op": "related", "id": decoys[0], "limit": 10, "under_path": "/qg/norms"})
	if len(ids) != len(targets) {
		t.Fatalf("related under /qg/norms = %v, want the %d norm clauses", ids, len(targets))
	}
	for _, id := range ids {
		if !setOf(targets)[id] {
			t.Errorf("related returned %s, outside /qg/norms", id)
		}
	}
	if echoInt(echo, "matched_documents") != 1 {
		t.Errorf("related echo = %v", echo)
	}
}

func TestDocumentRelated_ExcludeDocumentIDsDropsTheOwnDocument(t *testing.T) {
	c, _, targets := normsCorpus(t)
	own := c.docs["/qg/norms/ed1"]
	other := c.doc("/qg/norms/ed2")
	elsewhere := c.chunk(other, "norm clause decision", nil)

	ids, _ := c.call(map[string]any{"op": "related", "id": targets[0], "limit": 10, "under_path": "/qg/norms"})
	if !setOf(ids)[targets[1]] {
		t.Fatalf("precondition: the passport's own siblings are its neighbours (%v)", ids)
	}
	ids, _ = c.call(map[string]any{"op": "related", "id": targets[0], "limit": 10, "under_path": "/qg/norms",
		"exclude_document_ids": []string{own}})
	if len(ids) != 1 || ids[0] != elsewhere {
		t.Errorf("related excluding its own document = %v, want only %s", ids, elsewhere)
	}
}

func TestDocumentSearch_ExcludeUnderPathDropsTheTopHit(t *testing.T) {
	c, decoys, targets := normsCorpus(t)
	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 5})
	if len(ids) == 0 || !setOf(decoys)[ids[0]] {
		t.Fatalf("precondition: a board file is the top hit (%v)", ids)
	}
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 5, "exclude_under_path": "/board"})
	if len(ids) != len(targets) || setOf(decoys)[ids[0]] {
		t.Errorf("excluding /board = %v, want only the %d norm clauses", ids, len(targets))
	}
	// An exclusion is a denylist by nature, never an inverted allowlist.
	if echo["inverted"] != false {
		t.Errorf("echo = %v, want inverted false", echo)
	}
	last := c.spy.filters[len(c.spy.filters)-1]
	if len(last.IDs) != 0 || len(last.ExcludeIDs) != 123 {
		t.Errorf("the store was sent %d allowed / %d excluded ids, want 0 / 123 (120 board chunks + 3 roots)",
			len(last.IDs), len(last.ExcludeIDs))
	}
}

func TestDocumentSearch_ExcludeDocumentIDsDropsTheTopHit(t *testing.T) {
	c, _, _ := normsCorpus(t)
	top, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 1})
	var topDoc string
	for doc, chunks := range c.byID {
		if setOf(chunks)[top[0]] {
			topDoc = doc
		}
	}
	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "exclude_document_ids": []string{topDoc}})
	for _, id := range ids {
		if setOf(c.byID[topDoc])[id] {
			t.Errorf("excluded document %s still returned chunk %s", topDoc, id)
		}
	}
	if len(ids) == 0 {
		t.Error("excluding one document emptied the search")
	}
}

func TestDocumentSearch_TypeFilterExpandsSubtypesAndReportsIt(t *testing.T) {
	d, ctx := ontologyRetrievalFixture(t, true)
	vs := newVectorStore(d.Store)
	d.Store = &ftsStore{vs}
	d.Embedder = newFakeEmbedder("fake", "m1", "norm", "clause")
	c := &corpus{t: t, d: d, vs: vs, ctx: ctx, docs: map[string]string{}, byID: map[string][]string{}}
	doc := c.doc("/typed/log")
	incident := c.chunk(doc, "norm clause", map[string]any{"type": "incident"})
	outage := c.chunk(doc, "norm clause", map[string]any{"type": "outage"})
	person := c.chunk(doc, "norm clause", map[string]any{"type": "person"})

	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "type": "event"})
	got := setOf(ids)
	if !got[incident] || !got[outage] || got[person] {
		t.Errorf("type=event = %v, want the incident and the outage, never the person", ids)
	}
	exp, _ := echo["type_expanded_to"].([]any)
	if len(exp) != 3 || exp[0] != "event" {
		t.Errorf("type_expanded_to = %v, want [event incident outage]", exp)
	}
}

// facetCorpus: one document with chunks that differ only in status and tags.
func facetCorpus(t *testing.T) (*corpus, map[string]string) {
	t.Helper()
	c := newCorpus(t, "norm", "clause")
	doc := c.doc("/qg/norms/ed1")
	return c, map[string]string{
		"nbu":      c.chunk(doc, "norm clause", map[string]any{"status": "in_force", "tags": []string{"regulator/nbu"}}),
		"ecb":      c.chunk(doc, "norm clause", map[string]any{"status": "in_force", "tags": []string{"regulator/ecb"}}),
		"repealed": c.chunk(doc, "norm clause", map[string]any{"status": "repealed", "tags": []string{"regulator"}}),
		"other":    c.chunk(doc, "norm clause", map[string]any{"status": "draft", "tags": []string{"regulatory"}}),
	}
}

func assertHits(t *testing.T, what string, ids []string, want ...string) {
	t.Helper()
	if len(ids) != len(want) {
		t.Errorf("%s = %v, want %v", what, ids, want)
		return
	}
	got := setOf(ids)
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s = %v, missing %s", what, ids, w)
		}
	}
}

func TestDocumentSearch_StatusFilter(t *testing.T) {
	c, ch := facetCorpus(t)
	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "status": "in_force"})
	assertHits(t, "status=in_force", ids, ch["nbu"], ch["ecb"])
	ids, _ = c.call(map[string]any{"op": "search", "query": "norm clause", "statuses": []string{"repealed", "draft"}})
	assertHits(t, "statuses=[repealed draft]", ids, ch["repealed"], ch["other"])
}

func TestDocumentSearch_TagFilter(t *testing.T) {
	c, ch := facetCorpus(t)
	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "tag": "regulator/nbu"})
	assertHits(t, "tag=regulator/nbu", ids, ch["nbu"])
}

func TestDocumentSearch_TagPrefixFilter(t *testing.T) {
	c, ch := facetCorpus(t)
	// The tag itself and anything nested under it — not a tag that merely starts
	// with the same letters.
	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "tag_prefix": "regulator"})
	assertHits(t, "tag_prefix=regulator", ids, ch["nbu"], ch["ecb"], ch["repealed"])
}

func TestDocumentSearch_SingularAndPluralAreOneFilter(t *testing.T) {
	c, decoys, targets := normsCorpus(t)
	boardA := setOf(c.byID[c.docs["/board/a"]])
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50,
		"under_path": "/qg/norms", "under_paths": []string{"/board/a"}})
	got := setOf(ids)
	for _, id := range targets {
		if !got[id] {
			t.Errorf("the singular path's chunk %s is missing", id)
		}
	}
	fromA := 0
	for _, id := range ids {
		if boardA[id] {
			fromA++
		} else if setOf(decoys)[id] {
			t.Errorf("returned %s, from neither path", id)
		}
	}
	if fromA == 0 {
		t.Error("the plural path contributed nothing")
	}
	applied, _ := echo["applied"].(map[string]any)
	if paths, _ := applied["under_paths"].([]any); len(paths) != 2 {
		t.Errorf("applied under_paths = %v, want both", applied["under_paths"])
	}
}

func TestDocumentSearch_AsOfOverTheSidecar(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	doc := c.doc("/qg/facts")
	fact := func(key string, validAt int64, invalidAt *int64) string {
		v := map[string]any{"op": "upsert_chunk", "document_id": doc, "title": key, "natural_key": key,
			"body": "norm clause", "valid_at": validAt}
		if invalidAt != nil {
			v["invalid_at"] = *invalidAt
		}
		return c.exec(v)["id"].(string)
	}
	t2, t3 := int64(2000), int64(3000)
	fact("fact:early", 1000, &t2)
	middle := fact("fact:middle", 2000, &t3)
	fact("fact:late", 3000, nil)

	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "as_of": 2500})
	assertHits(t, "as_of=2500", ids, middle)
}

// recordingReranker keeps every prompt it is shown.
type recordingReranker struct {
	mu      sync.Mutex
	prompts []string
}

func (r *recordingReranker) Complete(_ context.Context, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
	return "[1]", nil
}

func TestDocumentSearch_RerankPoolHoldsOnlyFilteredChunks(t *testing.T) {
	c, _, _ := normsCorpus(t)
	b := inprocess.New(c.d.Store, c.d.Embedder)
	rr := &recordingReranker{}
	b.SetReranker(rr)
	c.d.Backend = b
	c.ctx = tools.UpdateRunPolicy(c.ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Rerank: rerankOn()}
	})

	c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 5})
	if len(rr.prompts) != 1 || !strings.Contains(rr.prompts[0], "boardfile") {
		t.Fatalf("precondition: the unfiltered rerank is shown board files (%d prompts)", len(rr.prompts))
	}
	c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 5, "under_path": "/qg/norms"})
	if len(rr.prompts) != 2 {
		t.Fatalf("the filtered search was not reranked (%d prompts)", len(rr.prompts))
	}
	if p := rr.prompts[1]; strings.Contains(p, "boardfile") || !strings.Contains(p, "normtext") {
		t.Errorf("the filtered rerank was shown a chunk outside the filter:\n%s", p)
	}
}

func TestDocumentSearch_FilterFreeInputNeverResolves(t *testing.T) {
	for _, in := range []string{
		`{"op":"search","query":"q"}`,
		`{"op":"search","query":"q","limit":5,"scope":"tenant"}`,
		`{"op":"search","query":"q","under_path":"","under_paths":[],"types":[],"type":""}`,
		`{"op":"related","id":"x"}`,
	} {
		var di docInput
		_ = json.Unmarshal([]byte(in), &di)
		if hasSearchFilter(di) {
			t.Errorf("%s counts as a filter — the filter-free path must stay the old one", in)
		}
	}
	for _, in := range []string{
		`{"under_path":"/a"}`, `{"under_paths":["/a"]}`, `{"exclude_under_path":"/a"}`, `{"exclude_under_paths":["/a"]}`,
		`{"document_id":"d"}`, `{"document_ids":["d"]}`, `{"exclude_document_ids":["d"]}`,
		`{"type":"t"}`, `{"types":["t"]}`, `{"status":"s"}`, `{"statuses":["s"]}`,
		`{"tag":"x"}`, `{"tag_prefix":"x"}`, `{"as_of":1}`,
	} {
		var di docInput
		_ = json.Unmarshal([]byte(in), &di)
		if !hasSearchFilter(di) {
			t.Errorf("%s is not recognised as a filter", in)
		}
	}
}

func TestDocumentSearch_PathInAnotherTenantMatchesNothing(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	a := c.chunk(c.doc("/qg/norms/ed1"), "norm clause", nil)
	bctx := tools.WithRunIdentity(c.ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "other"})
	cb := &corpus{t: t, d: c.d, vs: c.vs, spy: c.spy, emb: c.emb, ctx: bctx, docs: map[string]string{}, byID: map[string][]string{}}
	b := cb.chunk(cb.doc("/qg/norms/ed1"), "norm clause", nil)

	ids, echo := cb.call(map[string]any{"op": "search", "query": "norm clause", "under_path": "/qg/norms"})
	if setOf(ids)[a] || !setOf(ids)[b] {
		t.Errorf("tenant B's search = %v, want only B's chunk %s", ids, b)
	}
	if echoInt(echo, "matched_documents") != 1 {
		t.Errorf("tenant B's echo = %v, want B's one document", echo)
	}
	// A path that exists only in the other tenant resolves to nothing here.
	cb.doc("/only/b")
	ids, echo = c.call(map[string]any{"op": "search", "query": "norm clause", "under_path": "/only"})
	if len(ids) != 0 || echoInt(echo, "matched_documents") != 0 {
		t.Errorf("tenant A reached tenant B's /only path: %v %v", ids, echo)
	}
}

func TestDocumentSearch_UnresolvedPathReportsZeroMatchedDocuments(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	c.chunk(c.doc("/qg/norms/ed1"), "norm clause", nil)
	before := c.emb.n.Load()
	out := c.exec(map[string]any{"op": "search", "query": "norm clause", "under_path": "/qg/nope"})
	if chunks, _ := out["chunks"].([]any); chunks == nil || len(chunks) != 0 {
		t.Errorf("chunks = %v, want an empty list", out["chunks"])
	}
	echo, _ := out["filter"].(map[string]any)
	if echo == nil || echoInt(echo, "matched_documents") != 0 || echoInt(echo, "matched_chunks") != 0 {
		t.Errorf("filter echo = %v, want matched_documents 0, matched_chunks 0", echo)
	}
	if n := c.emb.n.Load() - before; n != 0 {
		t.Errorf("an unresolvable filter embedded the query (%d calls)", n)
	}
}

func TestDocumentSearch_WideFilterInvertsToADenylist(t *testing.T) {
	c, decoys, _ := normsCorpus(t)
	c.spy.reset()
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 10, "under_path": "/board"})
	for _, id := range ids {
		if !setOf(decoys)[id] {
			t.Errorf("returned %s, outside /board", id)
		}
	}
	// 123 of the scope's 129 chunks: the complement (the six under /qg/norms) is
	// what travels.
	if echo["inverted"] != true || echoInt(echo, "matched_chunks") != 123 || echoInt(echo, "matched_documents") != 3 {
		t.Errorf("echo = %v, want inverted, 123 chunks in 3 documents", echo)
	}
	for _, f := range c.spy.filters {
		if len(f.IDs) != 0 || len(f.ExcludeIDs) != 6 {
			t.Errorf("a leg was sent %d allowed / %d excluded ids, want 0 / 6", len(f.IDs), len(f.ExcludeIDs))
		}
	}
}

func TestDocumentSearch_FilterOverTheCapIsRefused(t *testing.T) {
	c, _, _ := normsCorpus(t)
	c.d.Cfg = &config.Config{}
	c.d.Cfg.Env.DocumentSearchFilterMaxIDs = 5
	before := c.emb.n.Load()
	b, _ := json.Marshal(map[string]any{"op": "search", "query": "norm clause", "under_path": "/qg/norms"})
	res, err := c.d.Execute(c.ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryBusiness ||
		!strings.Contains(res.Text, "filter_too_wide") || !strings.Contains(res.Text, "matched_chunks=6") ||
		!strings.Contains(res.Text, "scope_chunks=129") || !strings.Contains(res.Text, "max_ids=5") {
		t.Errorf("over the cap = %+v %s, want a filter_too_wide business refusal with the counts", res.Error, res.Text)
	}
	if c.emb.n.Load() != before {
		t.Error("a refused filter still embedded the query")
	}
	// Under the cap the same call runs.
	c.d.Cfg.Env.DocumentSearchFilterMaxIDs = 6
	if ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "under_path": "/qg/norms"}); len(ids) != 5 {
		t.Errorf("at the cap the search returned %v", ids)
	}
}

func TestDocumentSearch_FilterRefusesAMalformedPathAndANonPositiveAsOf(t *testing.T) {
	c := newCorpus(t, "norm")
	for _, in := range []string{
		`{"op":"search","query":"norm","under_path":"relative/path"}`,
		`{"op":"search","query":"norm","exclude_under_paths":["/a//../b"]}`,
		`{"op":"search","query":"norm","as_of":0}`,
		`{"op":"related","id":"` + strings.Repeat("a", 32) + `","as_of":-5}`,
	} {
		res, err := c.d.Execute(c.ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryValidation {
			t.Errorf("%s = %+v %s, want a validation error", in, res.Error, res.Text)
		}
	}
}

func TestDocumentSearch_ACLStillAppliesOnTopOfTheFilter(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	s := seedACLDocs(t, d, tenantDocCtx(base, nil))
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

	_, res := docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting","under_path":"/kb"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	if strings.Contains(res.Text, s.secretDoc) || strings.Contains(res.Text, s.secretChunk) {
		t.Errorf("the read-denied document inside the filtered subtree was returned: %s", res.Text)
	}
	if !strings.Contains(res.Text, s.publicChunk) {
		t.Errorf("the readable document was dropped: %s", res.Text)
	}
	// The echo counts only what the caller may read, so it cannot be used to
	// learn what a denied subtree holds.
	var out struct {
		Filter map[string]any `json:"filter"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	if echoInt(out.Filter, "matched_documents") != 1 {
		t.Errorf("echo = %v, want only the readable document counted", out.Filter)
	}
	_, res = docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting","under_path":"/kb/secret"}`)
	if res.IsError || !strings.Contains(res.Text, `"matched_documents":0`) {
		t.Errorf("a filter aimed at the denied subtree = %s, want matched_documents 0", res.Text)
	}
}

// TestDocumentSearchFilter_CoreBothTiers runs the resolver's SQL on both SQL
// Memory tiers: the IN batches, the tag subqueries, the sidecar join and the
// keyset paging are portable `?` SQL that the postgres tier rebinds.
func TestDocumentSearchFilter_CoreBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertSearchFilterCore(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertSearchFilterCore(t, d, ctx)
	})
}

func assertSearchFilterCore(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	vs := newVectorStore(d.Store)
	d.Store = &ftsStore{vs}
	d.Embedder = newFakeEmbedder("fake", "m1", "norm", "clause")
	c := &corpus{t: t, d: d, vs: vs, ctx: ctx, docs: map[string]string{}, byID: map[string][]string{}}

	board := c.doc("/board/a")
	var decoys []string
	for i := 0; i < 12; i++ {
		decoys = append(decoys, c.chunk(board, "norm clause", nil))
	}
	norms := c.doc("/qg/norms/ed1")
	inForce := c.chunk(norms, "norm clause", map[string]any{"type": "norm.clause", "status": "in_force", "tags": []string{"regulator/nbu"}})
	repealed := c.chunk(norms, "norm clause", map[string]any{"type": "norm.clause", "status": "repealed", "tags": []string{"regulator/ecb"}})
	t2 := int64(2000)
	early := c.exec(map[string]any{"op": "upsert_chunk", "document_id": norms, "title": "early", "natural_key": "fact:early",
		"body": "norm clause", "valid_at": int64(1000), "invalid_at": t2})["id"].(string)
	late := c.exec(map[string]any{"op": "upsert_chunk", "document_id": norms, "title": "late", "natural_key": "fact:late",
		"body": "norm clause", "valid_at": t2})["id"].(string)

	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50,
		"under_path": "/qg/norms", "type": "norm.clause", "status": "in_force"})
	assertHits(t, "under_path+type+status", ids, inForce)
	if echoInt(echo, "matched_documents") != 1 || echoInt(echo, "matched_chunks") != 1 {
		t.Errorf("echo = %v", echo)
	}
	ids, _ = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "tag_prefix": "regulator"})
	assertHits(t, "tag_prefix", ids, inForce, repealed)
	ids, _ = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "under_path": "/qg/norms", "as_of": 1500})
	// as_of passes every chunk with no sidecar interval, and of the dated facts
	// only the one true at 1500.
	got := setOf(ids)
	if !got[early] || got[late] || !got[inForce] || !got[repealed] {
		t.Errorf("as_of=1500 under /qg/norms = %v, want early + the two undated clauses, never late", ids)
	}
	// Wide: /board is most of the scope, so its complement travels.
	ids, echo = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "under_path": "/board"})
	if echo["inverted"] != true || len(ids) != len(decoys) {
		t.Errorf("wide filter = %d hits, echo %v; want the %d board chunks, inverted", len(ids), echo, len(decoys))
	}
	// Exclusion only.
	ids, _ = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "exclude_under_path": "/board"})
	for _, id := range ids {
		if setOf(decoys)[id] {
			t.Errorf("exclude_under_path=/board returned board chunk %s", id)
		}
	}
}

// TestDocumentSearchFilter_PagesPastTheRowCap — the operator's row cap bounds
// one SQL Memory read, not the filter: a resolver that took one capped read as
// the whole set would silently drop the rest of the corpus from the search.
func TestDocumentSearchFilter_PagesPastTheRowCap(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), MaxRows: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	c.d.SqlMem = mgr
	doc := c.doc("/qg/norms/ed1")
	var want []string
	for i := 0; i < 10; i++ {
		want = append(want, c.chunk(doc, "norm clause", map[string]any{"status": "in_force"}))
	}
	other := c.doc("/board/a")
	for i := 0; i < 12; i++ {
		c.chunk(other, "norm clause", nil)
	}
	c.spy.reset()
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "status": "in_force"})
	assertHits(t, "status=in_force past the row cap", ids, want...)
	if echoInt(echo, "matched_chunks") != 10 {
		t.Errorf("echo = %v, want all 10 matched despite a 4-row cap", echo)
	}
	if f := c.spy.filters[0]; len(f.IDs) != 10 {
		t.Errorf("the store was sent %d ids, want 10", len(f.IDs))
	}
}

// aclCorpus: /kb/secret/plan (read-denied to the limited caller) and
// /kb/public/notes in the tenant scope, plus a readable /other/doc with ten
// chunks, so /kb is a minority of the scope and an unnarrowed list would be an
// allowlist whose size the denied document moves.
func aclCorpus(t *testing.T) (d *Document, spy *legSpy, open, limited context.Context, s aclDocs) {
	t.Helper()
	d, vs, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	spy = &legSpy{ftsStore: &ftsStore{vs}}
	d.Store = spy
	open = tenantDocCtx(base, nil)
	s = seedACLDocs(t, d, open)
	out, r := docExec(t, d, open, `{"op":"create_document","scope":"tenant","title":"Other","path":"/other/doc"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	for i := 0; i < 10; i++ {
		if _, r := docExec(t, d, open, `{"op":"create_chunk","scope":"tenant","document_id":"`+out["document_id"].(string)+`","title":"o","body":"public"}`); r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
	}
	limited = tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	return d, spy, open, limited, s
}

// TestDocumentSearch_ACLLimitedCallerSendsNoDeniedID — for a caller an
// access-control set limits, the filter is narrowed to what it may read before
// anything is sent: no id of a read-denied document reaches the store, and the
// list is never inverted into a denylist (which would have to name them).
func TestDocumentSearch_ACLLimitedCallerSendsNoDeniedID(t *testing.T) {
	d, spy, _, ctx, s := aclCorpus(t)
	spy.reset()
	_, res := docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting","under_path":"/kb"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	if len(spy.filters) == 0 {
		t.Fatal("the search reached no store leg")
	}
	for _, f := range spy.filters {
		if len(f.ExcludeIDs) != 0 {
			t.Errorf("a limited caller's filter was sent as a denylist: %v", f.ExcludeIDs)
		}
		for _, id := range f.IDs {
			if id == s.secretChunk {
				t.Errorf("the read-denied chunk %s was sent to the store", id)
			}
		}
		if !setOf(f.IDs)[s.publicChunk] {
			t.Errorf("the readable chunk is missing from the list: %v", f.IDs)
		}
	}
	// A filter wide enough to invert for anyone else is still an allowlist here.
	spy.reset()
	if _, res = docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting","exclude_under_path":"/kb/public"}`); res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	for _, f := range spy.filters {
		if len(f.ExcludeIDs) != 0 || setOf(f.IDs)[s.secretChunk] {
			t.Errorf("a wide filter for a limited caller sent %d allowed (denied included: %v) / %d excluded",
				len(f.IDs), setOf(f.IDs)[s.secretChunk], len(f.ExcludeIDs))
		}
	}
}

// TestDocumentSearch_ACLDeniedDocumentsDoNotMoveTheCounts — the echo and the
// filter_too_wide refusal must not be a way to measure documents the caller may
// not read: growing a read-denied document inside the filtered subtree changes
// neither the counts, nor whether the refusal fires, nor its text.
func TestDocumentSearch_ACLDeniedDocumentsDoNotMoveTheCounts(t *testing.T) {
	d, _, open, ctx, s := aclCorpus(t)
	const in = `{"op":"search","scope":"tenant","query":"savepoint nesting","under_path":"/kb"}`
	observe := func(cap int) (string, bool) {
		t.Helper()
		d.Cfg = &config.Config{}
		d.Cfg.Env.DocumentSearchFilterMaxIDs = cap
		defer func() { d.Cfg = nil }()
		res, err := d.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			return res.Text, true
		}
		var out struct {
			Filter json.RawMessage `json:"filter"`
		}
		_ = json.Unmarshal([]byte(res.Text), &out)
		return string(out.Filter), false
	}
	// /kb holds 4 chunks (2 readable); the scope 15. A cap of 4 admits the
	// readable list and would refuse /kb's full list once the denied document grows.
	echo1, refused1 := observe(4)
	tight1, tightRefused1 := observe(1)
	for i := 0; i < 5; i++ {
		if _, r := docExec(t, d, open, `{"op":"create_chunk","scope":"tenant","document_id":"`+s.secretDoc+`","title":"more","body":"savepoint nesting secret"}`); r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
	}
	echo2, refused2 := observe(4)
	tight2, tightRefused2 := observe(1)
	if refused1 || refused2 {
		t.Errorf("a cap the readable list fits was refused (before %v, after %v): %s", refused1, refused2, echo2)
	}
	if echo1 != echo2 {
		t.Errorf("the echo moved when only a read-denied document grew:\n before %s\n after  %s", echo1, echo2)
	}
	if !tightRefused1 || !tightRefused2 || !strings.Contains(tight1, "filter_too_wide") {
		t.Fatalf("precondition: a 1-id cap refuses the readable list (before %v, after %v)", tightRefused1, tightRefused2)
	}
	if tight1 != tight2 {
		t.Errorf("the refusal moved when only a read-denied document grew:\n before %s\n after  %s", tight1, tight2)
	}
	if strings.Contains(tight1, "scope_chunks") {
		t.Errorf("a limited caller's refusal names the scope total: %s", tight1)
	}
}

// TestDocumentSearch_UnlimitedCallerStillInverts — the narrowing is for a caller
// a set actually limits. One whose set denies nothing in this scope keeps the
// full behaviour: full counts, and inversion for a wide filter.
func TestDocumentSearch_UnlimitedCallerStillInverts(t *testing.T) {
	d, spy, _, _, _ := aclCorpus(t)
	base := tools.WithRunIdentity(tools.WithAgentName(context.Background(), "doc-agent"),
		tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	// A rule about another scope: it limits nothing here.
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:user:/x/**"}}))
	spy.reset()
	_, res := docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting public","exclude_under_path":"/kb"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	var out struct {
		Filter map[string]any `json:"filter"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	if len(spy.filters) == 0 || len(spy.filters[0].ExcludeIDs) == 0 {
		t.Errorf("an unlimited caller's exclusion was not sent as a denylist: %+v", spy.filters)
	}
	spy.reset()
	_, res = docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting public","under_path":"/other"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	if out.Filter["inverted"] != true || len(spy.filters) == 0 || len(spy.filters[0].ExcludeIDs) == 0 {
		t.Errorf("an unlimited caller's wide filter did not invert: echo %v", out.Filter)
	}
}
