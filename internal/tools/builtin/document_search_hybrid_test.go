package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ftsStore gives the vector fake a full-text leg, as Postgres has: a row matches
// when its indexed text contains EVERY query word (plainto_tsquery's AND), ranked
// by how many times they occur.
type ftsStore struct{ *vectorStore }

func (f *ftsStore) SupportsFullText() bool { return true }

func (f *ftsStore) MemoryFullTextSearch(ctx context.Context, tenant string, scope store.MemoryScope, scopeID string,
	filter store.MemorySearchFilter, query string, limit int) ([]store.MemorySearchEntry, error) {
	words := strings.Fields(strings.ToLower(query))
	type hit struct {
		key string
		n   int
	}
	var hits []hit
	f.mu.Lock()
	prefix := vsKey(scope, scopeID, "")
	for k, e := range f.embeds {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		key := strings.TrimPrefix(k, prefix)
		if filter.KeyPrefix != "" && !strings.HasPrefix(key, filter.KeyPrefix) {
			continue
		}
		text, n := strings.ToLower(e.EmbedText), 0
		for _, w := range words {
			c := strings.Count(text, w)
			if c == 0 {
				n = 0
				break
			}
			n += c
		}
		if n > 0 {
			hits = append(hits, hit{key, n})
		}
	}
	f.mu.Unlock()
	for i := 1; i < len(hits); i++ { // insertion sort: few rows, and a stable order
		for j := i; j > 0 && (hits[j].n > hits[j-1].n || (hits[j].n == hits[j-1].n && hits[j].key < hits[j-1].key)); j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	var out []store.MemorySearchEntry
	for _, h := range hits {
		if len(out) == limit {
			break
		}
		row, err := f.Store.MemoryGet(ctx, tenant, scope, scopeID, h.key)
		if err != nil {
			continue
		}
		out = append(out, store.MemorySearchEntry{MemoryEntry: row})
	}
	return out, nil
}

// lexicalFixture builds a document with two chunks: DECOY shares the query's one
// known word and nothing else, so its cosine is the highest; TARGET carries the
// error code too, a word the embedder does not know — so only the full-text leg
// can see what makes it the answer.
func lexicalFixture(t *testing.T) (*Document, context.Context, string, string) {
	t.Helper()
	d, vs, ctx := mermaidDocFixture(t, "failed", "build")
	d.Store = &ftsStore{vs}
	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Log"}`))
	docID := resultField(res, "document_id")
	mk := func(title, body string) string {
		b, _ := json.Marshal(map[string]any{"op": "create_chunk", "document_id": docID, "title": title, "body": body})
		r, err := d.Execute(ctx, b)
		if err != nil || r.IsError {
			t.Fatalf("create_chunk %q: %v %s", title, err, r.Text)
		}
		return resultField(r, "id")
	}
	decoy := mk("One", "failed")
	target := mk("Two", "build failed with err_4711")
	return d, ctx, decoy, target
}

func firstChunk(t *testing.T, d *Document, ctx context.Context, query string, limit int) (string, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "search", "query": query, "limit": limit})
	out, err := d.Execute(ctx, b)
	if err != nil || out.IsError {
		t.Fatalf("search: %v %s", err, out.Text)
	}
	var got struct {
		Chunks []map[string]any `json:"chunks"`
	}
	_ = json.Unmarshal([]byte(out.Text), &got)
	if len(got.Chunks) == 0 {
		t.Fatalf("no chunks for %q: %s", query, out.Text)
	}
	return got.Chunks[0]["chunk_id"].(string), got.Chunks[0]
}

// TestDocumentSearch_ALexicalOnlyMatchSurfaces — the reason op=search moved onto
// the hybrid path. The word that identifies the target (an error code) means
// nothing to the embedder, so a vector-only search ranks the decoy first; the
// full-text leg is what puts the target on top.
func TestDocumentSearch_ALexicalOnlyMatchSurfaces(t *testing.T) {
	d, ctx, decoy, target := lexicalFixture(t)
	// Precondition, asserted rather than assumed: on cosine alone the decoy wins.
	if e1, e2 := embeddedTextFor(t, d.Store.(*ftsStore).vectorStore, decoy), embeddedTextFor(t, d.Store.(*ftsStore).vectorStore, target); e1 == "" || e2 == "" {
		t.Fatal("precondition: both chunks are indexed")
	}
	got, hit := firstChunk(t, d, ctx, "failed err_4711", 1)
	if got != target {
		t.Errorf("top hit = %s, want the chunk holding the error code (%s); the decoy is %s", got, target, decoy)
	}
	if rs, ok := hit["rank_score"].(float64); !ok || rs <= 0 {
		t.Errorf("a hit carries rank_score, the value its order used: %v", hit["rank_score"])
	}
}

// TestDocumentSearch_OrdersLikeMemorySearch — op=search and `Memory op=search`
// with the chunk prefix run one pipeline, so they agree on the order.
func TestDocumentSearch_OrdersLikeMemorySearch(t *testing.T) {
	d, ctx, _, _ := lexicalFixture(t)
	b, _ := json.Marshal(map[string]any{"op": "search", "query": "failed err_4711", "limit": 5})
	out, _ := d.Execute(ctx, b)
	var doc struct {
		Chunks []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal([]byte(out.Text), &doc)

	m := &Memory{Store: d.Store, Embedder: d.Embedder, MaxValueBytes: 1 << 16, DefaultQuotaBytes: 1 << 20}
	mctx := tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}})
	res, _ := m.Execute(mctx, json.RawMessage(`{"op":"search","scope":"user","query":"failed err_4711","prefix":"doc.chunk:","top_k":5}`))
	if res.IsError {
		t.Fatalf("memory search: %s", res.Text)
	}
	var mem struct {
		Entries []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"entries"`
	}
	_ = json.Unmarshal([]byte(res.Text), &mem)

	if len(doc.Chunks) == 0 || len(doc.Chunks) != len(mem.Entries) {
		t.Fatalf("document search returned %d, memory search %d", len(doc.Chunks), len(mem.Entries))
	}
	for i := range doc.Chunks {
		if doc.Chunks[i].ChunkID != mem.Entries[i].ChunkID {
			t.Errorf("rank %d: document search %s, memory search %s", i, doc.Chunks[i].ChunkID, mem.Entries[i].ChunkID)
		}
	}
}

// spyBackend counts the searches it serves.
type spyBackend struct {
	memrank.Backend
	searches int
}

func (s *spyBackend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memrank.SearchQuery,
	rank memrank.RankConfig, dedup memrank.DedupConfig) (memrank.SearchResult, error) {
	s.searches++
	return s.Backend.Search(ctx, scope, scopeID, q, rank, dedup)
}

// TestDocumentSearch_RunsOnTheWiredBackend — the server hands this tool the same
// backend instance the Memory tool uses; a search must go through it rather than
// build one of its own, or whatever that instance is configured with would
// silently not apply to document search.
func TestDocumentSearch_RunsOnTheWiredBackend(t *testing.T) {
	d, ctx, _, target := lexicalFixture(t)
	spy := &spyBackend{Backend: d.searchBackend()}
	d.Backend = spy
	if got, _ := firstChunk(t, d, ctx, "failed err_4711", 1); got != target || spy.searches != 1 {
		t.Errorf("wired backend served %d searches (top %s)", spy.searches, got)
	}
}
