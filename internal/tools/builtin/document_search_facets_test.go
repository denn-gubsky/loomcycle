package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Facet counts on op=search (RFC GL P3). Every corpus here is small enough that
// the expected counts are written out by hand.

// countCorpus is a scope of documents whose one-word query "alpha" ranks their
// chunks by the number of noise words a body carries: fewer, nearer.
type countCorpus struct {
	t     *testing.T
	d     *Document
	vs    *vectorStore
	ctx   context.Context
	scope string
	docs  map[string]string // path → document id
}

func newCountCorpus(t *testing.T, d *Document, ctx context.Context, scope string) *countCorpus {
	t.Helper()
	vs := newVectorStore(d.Store)
	d.Store = vs
	d.Embedder = newFakeEmbedder("fake", "m1", "alpha", "w1", "w2", "w3", "w4", "w5", "w6", "w7", "w8")
	return &countCorpus{t: t, d: d, vs: vs, ctx: ctx, scope: scope, docs: map[string]string{}}
}

// doc files a document at path with a root type ("" = untyped).
func (c *countCorpus) doc(path, typ string) string {
	c.t.Helper()
	v := map[string]any{"op": "create_document", "scope": c.scope, "title": "Doc", "path": path}
	if typ != "" {
		v["type"] = typ
	}
	id := c.exec(c.ctx, v)["document_id"].(string)
	c.docs[path] = id
	return id
}

// chunk adds a chunk whose body is "alpha" plus noise words, the rank's key.
func (c *countCorpus) chunk(docID string, noise int) string {
	c.t.Helper()
	body := "alpha"
	for i := 1; i <= noise; i++ {
		body += fmt.Sprintf(" w%d", i)
	}
	return c.exec(c.ctx, map[string]any{"op": "create_chunk", "scope": c.scope, "document_id": docID, "title": "Part", "body": body})["id"].(string)
}

func (c *countCorpus) exec(ctx context.Context, v map[string]any) map[string]any {
	c.t.Helper()
	b, _ := json.Marshal(v)
	out, r := docExec(c.t, c.d, ctx, string(b))
	if r.IsError {
		c.t.Fatalf("%s: %s", b, r.Text)
	}
	return out
}

// search runs op=search as ctx and returns the decoded answer and its text.
func (c *countCorpus) search(ctx context.Context, v map[string]any) (map[string]any, string) {
	c.t.Helper()
	v["op"], v["scope"] = "search", c.scope
	if _, ok := v["query"]; !ok {
		v["query"] = "alpha"
	}
	b, _ := json.Marshal(v)
	out, r := docExec(c.t, c.d, ctx, string(b))
	if r.IsError {
		c.t.Fatalf("%s: %s", b, r.Text)
	}
	return out, r.Text
}

// facetOf is one facet of an answer as value → count, and the answer's facets block.
func facetOf(t *testing.T, out map[string]any, kind string) map[string]int {
	t.Helper()
	block, ok := out["facets"].(map[string]any)
	if !ok {
		t.Fatalf("no facets block in %v", out)
	}
	list, ok := block[kind].([]any)
	if !ok {
		t.Fatalf("facet %q missing from %v", kind, block)
	}
	got := map[string]int{}
	for _, e := range list {
		m := e.(map[string]any)
		got[m["value"].(string)] = int(m["count"].(float64))
	}
	return got
}

func facetNumber(t *testing.T, out map[string]any, field string) int {
	t.Helper()
	block, _ := out["facets"].(map[string]any)
	n, ok := block[field].(float64)
	if !ok {
		t.Fatalf("facets.%s missing: %v", field, block)
	}
	return int(n)
}

func wantFacet(t *testing.T, what string, got, want map[string]int) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestDocumentSearchFacets_CountDocumentsNotChunks: a document whose three chunks
// all match counts once; type is the root's class; folder is each Path name's
// directory, so a document filed twice counts in both folders; an untyped
// document has no type value. The hits stay at limit while the counts cover
// every matching document.
func TestDocumentSearchFacets_CountDocumentsNotChunks(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		c := newCountCorpus(t, d, ctx, "user")
		a := c.doc("/kb/rfcs/a", "rfc")
		c.chunk(a, 0)
		c.chunk(a, 1)
		c.chunk(a, 2)
		c.chunk(c.doc("/kb/rfcs/b", "rfc"), 3)
		g := c.doc("/kb/guides/c", "guide")
		c.chunk(g, 4)
		c.exec(ctx, map[string]any{"op": "set_path", "scope": "user", "id": g, "path": "/kb/extra/c"})
		c.chunk(c.doc("/other/d", ""), 5)

		out, text := c.search(ctx, map[string]any{"limit": 2, "facets": []string{"type", "folder"}})
		if n := len(out["chunks"].([]any)); n != 2 {
			t.Fatalf("hits = %d, want the limit 2: %s", n, text)
		}
		wantFacet(t, "type", facetOf(t, out, "type"), map[string]int{"rfc": 2, "guide": 1})
		wantFacet(t, "folder", facetOf(t, out, "folder"), map[string]int{"/kb/rfcs": 2, "/kb/guides": 1, "/kb/extra": 1, "/other": 1})
		if n := facetNumber(t, out, "documents_counted"); n != 4 {
			t.Errorf("documents_counted = %d, want 4: %s", n, text)
		}
		if n := facetNumber(t, out, "chunks_counted"); n != 6 {
			t.Errorf("chunks_counted = %d, want 6: %s", n, text)
		}
		if n := facetNumber(t, out, "facet_window"); n != facetWindowDefault {
			t.Errorf("facet_window = %d, want the default %d", n, facetWindowDefault)
		}
		if _, asked := out["facets"].(map[string]any)["concept"]; asked {
			t.Errorf("a facet not asked for is present: %s", text)
		}
	})
}

// TestDocumentSearchFacets_WindowBoundsTheCount: the counts cover the facet_window
// nearest chunks (and the hits), not the scope. Four documents, one chunk each,
// ranked d1 … d4: a window of 2 counts d1 and d2 only. The window is clamped at
// the store's maximum, and the store is asked for exactly the window.
func TestDocumentSearchFacets_WindowBoundsTheCount(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	for i := 1; i <= 4; i++ {
		c.chunk(c.doc(fmt.Sprintf("/f%d/doc", i), ""), i-1)
	}
	out, text := c.search(ctx, map[string]any{"limit": 1, "facets": []string{"folder"}, "facet_window": 2})
	wantFacet(t, "folder over a window of 2", facetOf(t, out, "folder"), map[string]int{"/f1": 1, "/f2": 1})
	if n := facetNumber(t, out, "facet_window"); n != 2 {
		t.Errorf("facet_window = %d, want 2: %s", n, text)
	}
	if got := c.vs.keyLimits; !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("window queries = %v, want one of 2", got)
	}

	out, _ = c.search(ctx, map[string]any{"limit": 1, "facets": []string{"folder"}, "facet_window": 100000})
	if n := facetNumber(t, out, "facet_window"); n != 500 {
		t.Errorf("facet_window 100000 = %d, want it clamped to 500", n)
	}
	wantFacet(t, "folder over the whole scope", facetOf(t, out, "folder"), map[string]int{"/f1": 1, "/f2": 1, "/f3": 1, "/f4": 1})
}

// TestDocumentSearchFacets_FollowTheFilter: a structural filter narrows the counts
// as it narrows the hits.
func TestDocumentSearchFacets_FollowTheFilter(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	c.chunk(c.doc("/kb/rfcs/a", "rfc"), 0)
	c.chunk(c.doc("/kb/rfcs/b", "rfc"), 1)
	c.chunk(c.doc("/kb/guides/c", "guide"), 2)

	out, text := c.search(ctx, map[string]any{"under_path": "/kb/rfcs", "facets": []string{"type", "folder"}})
	wantFacet(t, "type under /kb/rfcs", facetOf(t, out, "type"), map[string]int{"rfc": 2})
	wantFacet(t, "folder under /kb/rfcs", facetOf(t, out, "folder"), map[string]int{"/kb/rfcs": 2})
	if _, ok := out["filter"]; !ok {
		t.Errorf("the filter echo is missing: %s", text)
	}

	// A filter that matches nothing answers with empty facets, not an error.
	out, text = c.search(ctx, map[string]any{"under_path": "/nowhere", "facets": []string{"type"}})
	wantFacet(t, "type under /nowhere", facetOf(t, out, "type"), map[string]int{})
	if n := facetNumber(t, out, "documents_counted"); n != 0 {
		t.Errorf("documents_counted under /nowhere = %d: %s", n, text)
	}
}

// TestDocumentSearchFacets_AbsentLeavesTheAnswerUnchanged: without facets the
// response has no facets key and the window query never runs; with them, the
// answer is the same answer plus the facets key.
func TestDocumentSearchFacets_AbsentLeavesTheAnswerUnchanged(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	c.chunk(c.doc("/kb/a", "rfc"), 0)
	c.chunk(c.doc("/kb/b", "rfc"), 1)

	plain, plainText := c.search(ctx, map[string]any{})
	if _, ok := plain["facets"]; ok {
		t.Fatalf("a search without facets carries a facets key: %s", plainText)
	}
	if len(c.vs.keyLimits) != 0 {
		t.Fatalf("a search without facets ran the window query: %v", c.vs.keyLimits)
	}
	with, withText := c.search(ctx, map[string]any{"facets": []string{"type"}})
	if _, ok := with["facets"]; !ok {
		t.Fatalf("a search with facets has no facets key: %s", withText)
	}
	delete(with, "facets")
	if !reflect.DeepEqual(plain, with) {
		t.Errorf("facets changed the rest of the answer:\n without %s\n with    %s", plainText, withText)
	}
}

// TestDocumentSearchFacets_RefuseMalformedArguments: an unknown facet, a negative
// window, and a window with no facets are refused before anything runs.
func TestDocumentSearchFacets_RefuseMalformedArguments(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	c.chunk(c.doc("/kb/a", "rfc"), 0)
	for _, tc := range []struct{ in, want string }{
		{`{"op":"search","query":"alpha","facets":["author"]}`, `unknown facet "author"`},
		{`{"op":"search","query":"alpha","facets":["type"],"facet_window":-1}`, "facet_window must be positive"},
		{`{"op":"search","query":"alpha","facet_window":50}`, "facet_window is taken with facets"},
	} {
		_, r := docExec(t, d, ctx, tc.in)
		if !r.IsError || !strings.Contains(r.Text, tc.want) {
			t.Errorf("%s = %s, want a refusal containing %q", tc.in, r.Text, tc.want)
		}
	}
	if len(c.vs.keyLimits) != 0 {
		t.Errorf("a refused call ran the window query: %v", c.vs.keyLimits)
	}
}

// TestDocumentSearchFacets_CountOnlyReadableDocuments: a reader denied part of the
// scope gets counts over what it may read — the hidden documents' types and
// folders appear nowhere, and documents_counted does not include them. Both the
// narrowed search and the one whose access list is over the operator's cap (which
// then runs unnarrowed) are covered: the second is where only the facet's own
// per-document check stands between a count and a hidden document.
func TestDocumentSearchFacets_CountOnlyReadableDocuments(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		c := newCountCorpus(t, d, open, "tenant")
		for i, p := range []string{"/hidden/x", "/hidden/y", "/visible/v", "/visible/w"} {
			typ := "public"
			if strings.HasPrefix(p, "/hidden") {
				typ = "secret"
			}
			c.chunk(c.doc(p, typ), i)
		}
		all, _ := c.search(open, map[string]any{"facets": []string{"type", "folder"}})
		wantFacet(t, "open reader's types", facetOf(t, all, "type"), map[string]int{"secret": 2, "public": 2})

		deny := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/hidden/x", "document:tenant:/hidden/y"}}))
		allow := authz.WithMode(tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:/visible/v", "document:tenant:/visible/w"}})), authz.ModeEnforce)
		for _, maxIDs := range []int{0, 1} {
			if maxIDs > 0 {
				d.Cfg = &config.Config{}
				d.Cfg.Env.DocumentSearchFilterMaxIDs = maxIDs
			}
			for name, reader := range map[string]context.Context{"deny": deny, "allow": allow} {
				what := fmt.Sprintf("%s reader, max ids %d", name, maxIDs)
				out, text := c.search(reader, map[string]any{"facets": []string{"type", "folder"}})
				wantFacet(t, what+": type", facetOf(t, out, "type"), map[string]int{"public": 2})
				wantFacet(t, what+": folder", facetOf(t, out, "folder"), map[string]int{"/visible": 2})
				if n := facetNumber(t, out, "documents_counted"); n != 2 {
					t.Errorf("%s: documents_counted = %d, want 2: %s", what, n, text)
				}
				// Chunks are counted as documents are: a count that included the
				// hidden chunks would tell the reader how many there are.
				if n := facetNumber(t, out, "chunks_counted"); n != 2 {
					t.Errorf("%s: chunks_counted = %d, want the 2 readable chunks: %s", what, n, text)
				}
				if strings.Contains(text, "secret") || strings.Contains(text, "/hidden") {
					t.Errorf("%s: the answer names a hidden document: %s", what, text)
				}
				// Narrowed in the query, the window reached the end of what the
				// reader may read; unnarrowed, whether it did would depend on the
				// hidden chunks it met, so it is never claimed.
				if got, want := facetComplete(t, out), maxIDs == 0; got != want {
					t.Errorf("%s: complete = %v, want %v: %s", what, got, want, text)
				}

				// The hidden chunks rank first. A window of 2 must still be two
				// chunks the reader may read, not two hidden places that leave the
				// counts empty (and so measure what is hidden).
				out, text = c.search(reader, map[string]any{"limit": 1, "facets": []string{"folder"}, "facet_window": 2})
				wantFacet(t, what+": folder over a window of 2", facetOf(t, out, "folder"), map[string]int{"/visible": 2})
				if n := facetNumber(t, out, "chunks_counted"); n != 2 {
					t.Errorf("%s: window of 2, chunks_counted = %d, want 2: %s", what, n, text)
				}
			}
		}
		d.Cfg = nil
	})
}

// TestDocumentSearchFacets_ConceptsByStatementAndByTag: in the tenant scope a
// document counts under each concept a live subject statement (or a narrower kind)
// from any of its chunks reaches, once however many chunks do; a proposed
// statement does not count. In a user scope a chunk's tag counts under the most
// specific concept it is at or under. A caller who may not read the ontology gets
// no concept values at all.
func TestDocumentSearchFacets_ConceptsByStatementAndByTag(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertConceptFacets(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertConceptFacets(t, d, ctx)
	})
}

func assertConceptFacets(t *testing.T, d *Document, base context.Context) {
	f := newConceptFixture(t, d, base)
	rm := f.concepts["topics/risk-management"]
	outsourcing := f.concepts["topics/risk-management/outsourcing"]

	a := f.doc("tenant", "/kb/a")
	f.link("link_chunks", f.chunk("tenant", a), rm, "subject")
	f.link("link_chunks", f.chunk("tenant", a), rm, "primary_topic") // the same document again
	b := f.doc("tenant", "/kb/b")
	f.link("link_chunks", f.chunk("tenant", b), outsourcing, "subject")
	p := f.doc("tenant", "/kb/p")
	f.link("propose_statement", f.chunk("tenant", p), rm, "subject")
	f.chunk("tenant", f.doc("tenant", "/kb/tagged"), "topics/risk-management") // a tag alone: not in the tenant scope

	facets := func(ctx context.Context, scope string) (map[string]int, string) {
		t.Helper()
		_, text := f.search(ctx, map[string]any{"scope": scope, "under_path": "/kb", "facets": []string{"concept"}})
		if strings.Contains(text, `"error"`) || !strings.Contains(text, `"facets"`) {
			t.Fatalf("%s search: %s", scope, text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(text), &out)
		return facetOf(t, out, "concept"), text
	}
	got, text := facets(f.reader, "tenant")
	wantFacet(t, "tenant concepts ("+text+")", got, map[string]int{"topics/risk-management": 1, "topics/risk-management/outsourcing": 1})

	ud := f.doc("user", "/kb/u1")
	f.chunk("user", ud, "topics/risk-management")
	f.chunk("user", ud, "topics/risk-management/undeclared") // the same concept, the same document
	f.chunk("user", f.doc("user", "/kb/u2"), "topics/risk-management/outsourcing")
	f.chunk("user", f.doc("user", "/kb/u3"), "topics/zzz") // under no concept
	got, text = facets(f.reader, "user")
	wantFacet(t, "user concepts ("+text+")", got, map[string]int{"topics/risk-management": 1, "topics/risk-management/outsourcing": 1})

	for _, scope := range []string{"tenant", "user"} {
		got, text := facets(f.unreader, scope)
		wantFacet(t, scope+" concepts for a caller who may not read the ontology ("+text+")", got, map[string]int{})
	}
}

func facetComplete(t *testing.T, out map[string]any) bool {
	t.Helper()
	block, _ := out["facets"].(map[string]any)
	v, ok := block["complete"].(bool)
	if !ok {
		t.Fatalf("facets.complete missing: %v", block)
	}
	return v
}

// TestDocumentSearchFacets_CompleteOnlyWhenTheWindowReachedTheEnd: complete is
// true only when the window provably held every matching chunk — the store
// ranked exhaustively and returned fewer keys than asked, and every key was a
// document's chunk. A window smaller than the matches, an approximate index, or
// a chunk body with no structure row (dropped, so chunks_counted alone would
// still read as "less than the window") each make it false.
func TestDocumentSearchFacets_CompleteOnlyWhenTheWindowReachedTheEnd(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	for i := 1; i <= 3; i++ {
		c.chunk(c.doc(fmt.Sprintf("/f%d/doc", i), "rfc"), i-1)
	}
	out, text := c.search(ctx, map[string]any{"facets": []string{"type"}})
	if !facetComplete(t, out) {
		t.Errorf("3 matches in a window of 100: complete = false: %s", text)
	}
	out, text = c.search(ctx, map[string]any{"facets": []string{"type"}, "facet_window": 2})
	if facetComplete(t, out) {
		t.Errorf("3 matches in a window of 2: complete = true: %s", text)
	}

	c.vs.approximate = true
	out, text = c.search(ctx, map[string]any{"facets": []string{"type"}})
	if facetComplete(t, out) {
		t.Errorf("an approximate index's window reported complete: %s", text)
	}
	c.vs.approximate = false

	// A chunk body whose structure row is gone, nearest of all to the query.
	const orphan = "doc.chunk:00000000000000000000000000000000"
	if err := d.Store.MemorySet(ctx, direntTenant(ctx), store.MemoryScopeUser, "u1", orphan, json.RawMessage(`{"body":"alpha"}`), 0); err != nil {
		t.Fatal(err)
	}
	vec, _ := d.Embedder.Embed(ctx, []string{"alpha"})
	if err := c.vs.MemoryEmbedSet(ctx, direntTenant(ctx), store.MemoryScopeUser, "u1", orphan, store.MemoryEmbedding{
		Provider: "fake", Model: "m1", Dimension: len(vec[0]), Vector: vec[0], EmbedText: "alpha",
	}); err != nil {
		t.Fatal(err)
	}
	out, text = c.search(ctx, map[string]any{"facets": []string{"type"}})
	if n := facetNumber(t, out, "chunks_counted"); n != 3 {
		t.Fatalf("precondition: chunks_counted = %d, want the 3 real chunks (the orphan is not one): %s", n, text)
	}
	if facetComplete(t, out) {
		t.Errorf("a window that dropped a chunk with no structure row reported complete: %s", text)
	}
}

// TestDocumentSearchFacets_RefusedWithAnnotationsOnly: an annotations-only search
// ranks no passage, so facets (which count passages) are refused with it rather
// than answered over a set the response does not show.
func TestDocumentSearchFacets_RefusedWithAnnotationsOnly(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	c.chunk(c.doc("/kb/a", "rfc"), 0)
	_, r := docExec(t, d, ctx, `{"op":"search","query":"alpha","annotations":"only","facets":["type"]}`)
	if !r.IsError || !strings.Contains(r.Text, `not taken with annotations "only"`) {
		t.Errorf("facets with annotations only = %s, want a refusal", r.Text)
	}
	if out, text := c.search(ctx, map[string]any{"annotations": "include", "facets": []string{"type"}}); len(facetOf(t, out, "type")) == 0 {
		t.Errorf("facets with annotations include counted nothing: %s", text)
	}
}

// TestDocumentSearchFacets_ALimitedReaderWithNothingHiddenFetchesTheWindow: a
// reader whose access set names part of the scope but hides nothing in it gets a
// narrowed (no-op) search, so its window is facet_window, not the maximum the
// over-cap case needs.
func TestDocumentSearchFacets_ALimitedReaderWithNothingHiddenFetchesTheWindow(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	c := newCountCorpus(t, d, open, "tenant")
	c.chunk(c.doc("/kb/a", "rfc"), 0)
	reader := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/nothing/here"}}))
	c.vs.keyLimits = nil
	out, text := c.search(reader, map[string]any{"facets": []string{"type"}})
	if got := c.vs.keyLimits; !reflect.DeepEqual(got, []int{facetWindowDefault}) {
		t.Errorf("window queries = %v, want one of %d", got, facetWindowDefault)
	}
	if !facetComplete(t, out) {
		t.Errorf("nothing hidden, 1 match in a window of 100: complete = false: %s", text)
	}
}

// failingOntologyStore fails the lookup of the ontology's Path name when armed,
// the first read the tenant ontology makes.
type failingOntologyStore struct {
	store.Store
	armed bool
}

func (f *failingOntologyStore) DirentGet(ctx context.Context, tenantID, scope, scopeID, parentPath, name string) (store.DirentRow, error) {
	if f.armed && parentPath+name == memrank.OntologyPath {
		return store.DirentRow{}, errors.New("injected: ontology name lookup failed")
	}
	return f.Store.DirentGet(ctx, tenantID, scope, scopeID, parentPath, name)
}

// TestDocumentSearchFacets_AnOntologyFaultLeavesOutOnlyTheConcepts: a fault
// reading the ontology keeps the search and the other facets, and reports why
// the concept counts are missing.
func TestDocumentSearchFacets_AnOntologyFaultLeavesOutOnlyTheConcepts(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	c := newCountCorpus(t, d, ctx, "user")
	c.chunk(c.doc("/kb/a", "rfc"), 0)
	fs := &failingOntologyStore{Store: d.Store}
	d.Store = fs
	fs.armed = true
	out, text := c.search(ctx, map[string]any{"facets": []string{"type", "concept"}})
	if len(out["chunks"].([]any)) == 0 {
		t.Fatalf("the search lost its hits: %s", text)
	}
	wantFacet(t, "type beside an ontology fault", facetOf(t, out, "type"), map[string]int{"rfc": 1})
	block := out["facets"].(map[string]any)
	if _, ok := block["concept"]; ok {
		t.Errorf("concept counts present despite the fault: %s", text)
	}
	if reason, _ := block["concept_error"].(string); reason == "" {
		t.Errorf("no concept_error: %s", text)
	}
}

// TestDocumentSearchFacets_ConceptsCountOnlyLiveStatementsOnLiveChunks: a
// rejected, refuted or retired statement does not make a document about a
// concept; a statement counts at an as_of inside its dates and not outside; a
// chunk closed by its own validity no longer speaks for its document, except at
// an as_of inside its life.
func TestDocumentSearchFacets_ConceptsCountOnlyLiveStatementsOnLiveChunks(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertLiveConceptFacets(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertLiveConceptFacets(t, d, ctx)
	})
}

func assertLiveConceptFacets(t *testing.T, d *Document, base context.Context) {
	f := newConceptFixture(t, d, base)
	rm := f.concepts["topics/risk-management"]
	op := operatorCtx(f.operator)
	statement := func(path, kind string) string {
		t.Helper()
		return f.link(kind, f.chunk("tenant", f.doc("tenant", path)), rm, "subject")
	}
	statement("/kb/live", "link_chunks")
	rejected := statement("/kb/rejected", "propose_statement")
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"wrong"}`, rejected)); r.IsError {
		t.Fatalf("reject_statement: %s", r.Text)
	}
	refuted := statement("/kb/refuted", "link_chunks")
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"judge_fact","scope":"tenant","id":%q,"verdict":"unsupported","reason":"not about it"}`, refuted)); r.IsError {
		t.Fatalf("judge_fact: %s", r.Text)
	}
	// Retired as maintain_statement's expire leaves a rule-made statement (a
	// manual one cannot be expired, so the row is written directly).
	retired := statement("/kb/retired", "link_chunks")
	tkey, _, err := d.resolveScope(f.reader, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.exec(f.reader, tkey, `UPDATE chunk_edges SET expired_at = ? WHERE id = ?`, int64(500), retired); err != nil {
		t.Fatalf("retire: %v", err)
	}
	closed := f.chunk("tenant", f.doc("tenant", "/kb/closed"))
	f.link("link_chunks", closed, rm, "subject")
	f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": closed, "valid_at": 1000, "invalid_at": 2000,
		"revision": f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": closed})["revision"]})

	count := func(v map[string]any) int {
		t.Helper()
		v["scope"], v["under_path"], v["facets"] = "tenant", "/kb", []string{"concept"}
		_, text := f.search(f.reader, v)
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil || out["facets"] == nil {
			t.Fatalf("search %v: %s", v, text)
		}
		return facetOf(t, out, "concept")["topics/risk-management"]
	}
	// Now: only /kb/live. The closed chunk's life ended at 2000; the rejected,
	// refuted and retired statements never count.
	if n := count(map[string]any{}); n != 1 {
		t.Errorf("concept count now = %d, want 1 (the live statement only)", n)
	}

	// A statement dated 1000..2000. Without as_of its dates are not consulted, as
	// in the concept filter's rule; with as_of it counts only inside them.
	dated := f.chunk("tenant", f.doc("tenant", "/kb/dated"))
	f.c.exec(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": dated, "to_id": rm, "kind": "subject", "valid_at": 1000, "invalid_at": 2000})
	if n := count(map[string]any{}); n != 2 {
		t.Errorf("concept count now, with the dated statement = %d, want 2 (live, dated)", n)
	}
	// At 1500 the dated statement and the closed chunk were both in force.
	if n := count(map[string]any{"as_of": 1500}); n != 3 {
		t.Errorf("concept count at as_of 1500 = %d, want 3 (live, dated, closed)", n)
	}
	if n := count(map[string]any{"as_of": 3000}); n != 1 {
		t.Errorf("concept count at as_of 3000 = %d, want 1", n)
	}
}
