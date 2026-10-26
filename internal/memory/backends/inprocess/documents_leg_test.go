package inprocess_test

import (
	"context"
	"encoding/json"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A documents leg: the agent's own memory in the user scope, its reference corpus
// in the tenant scope, ranked as one list.

// legFixture: in the user scope a note "go" and a user document chunk "go rust";
// in the tenant scope a document chunk "go python" (uc), a tenant NOTE "go" that
// a documents leg must never return, and a chunk "alice" (ut) that matches only
// through its units.
func legFixture(t *testing.T) (*inprocess.Backend, *countEmbedder, func()) {
	t.Helper()
	s, cleanup := newStore(t)
	vs := newVectorStore(s)
	emb := &countEmbedder{fakeEmbedder: newFakeEmbedder("alice", "bob", "go", "rust", "python")}
	b := inprocess.New(vs, emb)
	ctx := context.Background()
	set := func(scope store.MemoryScope, id, key, value, embed string) {
		if _, err := b.Set(ctx, scope, id, key, json.RawMessage(value), memory.SetOptions{Embed: true, EmbedText: embed}); err != nil {
			t.Fatal(err)
		}
	}
	set(store.MemoryScopeUser, "u1", "notes/n1", `"go"`, "go")
	set(store.MemoryScopeUser, "u1", "doc.chunk:uc", `{"body":"go rust"}`, "Mine — One\ngo rust")
	set(store.MemoryScopeTenant, "", "doc.chunk:tc", `{"body":"go python"}`, "Board — Norms\ngo python")
	set(store.MemoryScopeTenant, "", "notes/tn", `"go"`, "go")
	set(store.MemoryScopeTenant, "", "doc.chunk:ut", `{"body":"alice"}`, "Board — Units\nalice")
	v, _ := json.Marshal(memory.UnitValue{ChunkID: "ut", Kind: memory.UnitQuestion, Text: "go python bob?"})
	set(store.MemoryScopeTenant, "", memory.UnitKey("ut", memory.UnitQuestion, 0), string(v), "go python bob")
	emb.calls = 0
	return b, emb, cleanup
}

type countEmbedder struct {
	*fakeEmbedder
	calls int
}

func (c *countEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls++
	return c.fakeEmbedder.Embed(ctx, texts)
}

var tenantLeg = &memory.DocumentsLeg{Scope: store.MemoryScopeTenant, ScopeID: ""}

func scopeOf(res memory.SearchResult) map[string]store.MemoryScope {
	out := map[string]store.MemoryScope{}
	for i, e := range res.Entries {
		if i < len(res.Scopes) {
			out[e.Key] = res.Scopes[i]
		}
	}
	return out
}

func TestSearch_DocumentsLegAddsAnotherScopesDocumentsOnly(t *testing.T) {
	b, emb, cleanup := legFixture(t)
	defer cleanup()
	res := search(t, b, memory.SearchQuery{QueryText: "go python", TopK: 10, Documents: tenantLeg})
	got := scopeOf(res)
	want := map[string]store.MemoryScope{
		"notes/n1":     store.MemoryScopeUser,
		"doc.chunk:uc": store.MemoryScopeUser,
		"doc.chunk:tc": store.MemoryScopeTenant,
		// Found through its unit, whose chunk body is read in the TENANT scope.
		"doc.chunk:ut": store.MemoryScopeTenant,
	}
	for k, s := range want {
		if got[k] != s {
			t.Errorf("%s: scope %q, want %q (all: %v)", k, got[k], s, got)
		}
	}
	if _, ok := got["notes/tn"]; ok {
		t.Errorf("the documents leg returned a tenant note: %v", got)
	}
	if len(res.Scopes) != len(res.Entries) {
		t.Errorf("scopes %d not aligned with %d entries", len(res.Scopes), len(res.Entries))
	}
	if emb.calls != 1 {
		t.Errorf("the query was embedded %d times, want once for both legs", emb.calls)
	}
	// The unit that found ut is reported, aligned with its entry.
	for i, e := range res.Entries {
		if e.Key == "doc.chunk:ut" && (i >= len(res.MatchedUnits) || res.MatchedUnits[i] == nil) {
			t.Errorf("ut came back without the unit that found it: %+v", res.MatchedUnits)
		}
	}
}

func TestSearch_NoDocumentsLegReportsNoScopes(t *testing.T) {
	b, _, cleanup := legFixture(t)
	defer cleanup()
	res := search(t, b, memory.SearchQuery{QueryText: "go python", TopK: 10})
	if res.Scopes != nil {
		t.Errorf("a single-scope search reports scopes: %v", res.Scopes)
	}
	for _, e := range res.Entries {
		if e.Key == "doc.chunk:tc" || e.Key == "doc.chunk:ut" {
			t.Errorf("a user-scope search returned the tenant's %s", e.Key)
		}
	}
}

func TestSearch_DocumentsLegIsCutByOneTopK(t *testing.T) {
	b, _, cleanup := legFixture(t)
	defer cleanup()
	res := search(t, b, memory.SearchQuery{QueryText: "go python", TopK: 2, Documents: tenantLeg})
	if len(res.Entries) != 2 || !res.Truncated {
		t.Errorf("entries %d truncated %v, want the fused list cut to top_k 2", len(res.Entries), res.Truncated)
	}
}

// The rerank is shown ONE pool drawn from both scopes, each candidate's text read
// from its own scope, and its order applies across them.
func TestInProcessRerank_DocumentsLegRanksOnePool(t *testing.T) {
	b, _, cleanup := legFixture(t)
	defer cleanup()
	r := &orderRanker{report: memory.RerankReport{Applied: true}}
	b.SetRanker(r)
	// Reverse whatever the pool is.
	q := memory.SearchQuery{QueryText: "go python", TopK: 10, Sources: []memory.Source{memory.SourceFacts, memory.SourceNotes},
		Documents: tenantLeg, Rerank: rerankOn}
	before := search(t, b, memory.SearchQuery{QueryText: q.QueryText, TopK: 10, Sources: q.Sources, Documents: tenantLeg})
	n := len(before.Entries)
	r.order = make([]int, n)
	for i := range r.order {
		r.order[i] = n - 1 - i
	}
	r.report.Candidates = n
	res := search(t, b, q)
	if res.Rerank == nil || !res.Rerank.Applied {
		t.Fatalf("a notes search with a documents leg was not reranked: %+v", res.Rerank)
	}
	seen := map[string]bool{}
	for _, txt := range r.texts {
		seen[txt] = true
	}
	for _, txt := range []string{"go", "Board — Norms\ngo python", "Board — Units\nalice"} {
		if !seen[txt] {
			t.Errorf("the rerank was not shown %q (its own scope's index text): %q", txt, r.texts)
		}
	}
	if res.Entries[0].Key != before.Entries[n-1].Key {
		t.Errorf("rerank order not applied across scopes: first %s, want %s", res.Entries[0].Key, before.Entries[n-1].Key)
	}
	got := scopeOf(res)
	if got["doc.chunk:tc"] != store.MemoryScopeTenant || got["notes/n1"] != store.MemoryScopeUser {
		t.Errorf("scopes after the rerank: %v", got)
	}
}

func TestRecall_DocumentsLegCarriesEachFactsScope(t *testing.T) {
	b, _, cleanup := legFixture(t)
	defer cleanup()
	res, err := b.Recall(context.Background(), store.MemoryScopeUser, "u1",
		memory.RecallQuery{Query: "go python", TopK: 10, Documents: tenantLeg})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.MemoryScope{}
	for _, f := range res.Facts {
		got[f.ID] = f.Scope
	}
	// (That recall's own default keeps the user's prose out is the stores'
	// ExcludeDocumentPrefix predicate, which this double does not model.)
	if got["notes/n1"] != store.MemoryScopeUser || got["doc.chunk:tc"] != store.MemoryScopeTenant || got["doc.chunk:ut"] != store.MemoryScopeTenant {
		t.Errorf("recall scopes: %v", got)
	}
	if _, ok := got["notes/tn"]; ok {
		t.Errorf("the documents leg returned a tenant note: %v", got)
	}
}

// A chunk id the primary scope already holds is never fused with the leg's row of
// the same id: the primary scope's row wins and keeps its scope.
func TestSearch_DocumentsLegNeverFusesTwoRowsOfOneChunkID(t *testing.T) {
	b, _, cleanup := legFixture(t)
	defer cleanup()
	if _, err := b.Set(context.Background(), store.MemoryScopeTenant, "", "doc.chunk:uc", json.RawMessage(`{"body":"go rust tenant"}`),
		memory.SetOptions{Embed: true, EmbedText: "go rust"}); err != nil {
		t.Fatal(err)
	}
	res := search(t, b, memory.SearchQuery{QueryText: "go rust", TopK: 10, Documents: tenantLeg})
	n := 0
	for i, e := range res.Entries {
		if e.Key == "doc.chunk:uc" {
			n++
			if res.Scopes[i] != store.MemoryScopeUser || string(e.Value) != `{"body":"go rust"}` {
				t.Errorf("uc came back as %s %s, want the user scope's row", res.Scopes[i], e.Value)
			}
		}
	}
	if n != 1 {
		t.Errorf("uc appears %d times", n)
	}
}
