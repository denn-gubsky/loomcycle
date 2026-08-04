package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC BS Phase 3b — discovery: `related` (semantic neighbours of a chunk) and
// `unlinked_mentions` (chunks whose body text names a chunk's title but never
// link it).
//
// `related` needs a vector-capable store + an embedder, so it reuses the same
// in-memory vectorStore + fakeEmbedder fakes the Memory-tool vector tests use
// (memory_vector_test.go). That fake filters search hits at tenant "", so the
// related fixture runs on sqlite only, with an EMPTY tenant — a real
// pgvector-backed postgres tier is exercised elsewhere.
//
// `unlinked_mentions` is pure SQL (chunk_edges) + a k/v Memory body scan (no
// vectors), so it runs on BOTH SQL Memory tiers via pgDocumentOrSkip, proving the
// IN(...) enrichment's `?` rebind and the edge query on postgres too.

// relatedFixture builds a Document backed by the in-memory vector store + fake
// embedder, with an EMPTY tenant. The vectorStore fake filters search hits via
// MemoryGet at tenant "" (see memory_vector_test.go), so chunk bodies must be
// written under the "" partition for it to find them — direntTenant("")→"" makes
// writeBody store there.
func relatedFixture(t *testing.T) (*Document, context.Context) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	vs := newVectorStore(s)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	emb := newFakeEmbedder("fake", "fake-embed-001",
		"go", "rust", "systems", "programming", "concurrency",
		"python", "data", "science", "pandas")
	d := &Document{Store: vs, SqlMem: mgr, Bus: channels.NewBus(), Embedder: emb}
	ctx := tools.WithAgentName(context.Background(), "rel-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1"})
	return d, ctx
}

// TestDocumentRelated_RanksNeighboursAndExcludesSelf: a chunk whose body shares
// tokens with the query chunk ranks as a neighbour; a token-disjoint chunk ranks
// below it; and the query chunk is NEVER returned as its own neighbour. Each hit
// carries its score + enriched title/document_id.
func TestDocumentRelated_RanksNeighboursAndExcludesSelf(t *testing.T) {
	d, ctx := relatedFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Rel","path":"/rel/one"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := out["document_id"].(string)

	mk := func(title, body string) string {
		o, rr := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, docID, title, body))
		if rr.IsError {
			t.Fatalf("create_chunk(%s): %s", title, rr.Text)
		}
		return o["id"].(string)
	}
	idA := mk("A", "go rust systems programming") // the query chunk
	idB := mk("B", "go rust systems concurrency") // shares 3 tokens with A
	idC := mk("C", "python data science pandas")  // token-disjoint from A

	out, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"related","scope":"user","id":%q}`, idA))
	if r.IsError {
		t.Fatalf("related: %s", r.Text)
	}
	rel, _ := out["related"].([]any)
	if len(rel) == 0 {
		t.Fatalf("related returned no neighbours: %s", r.Text)
	}

	var order []string
	byID := map[string]map[string]any{}
	lastScore := 1e9
	for _, e := range rel {
		m := e.(map[string]any)
		cid := m["chunk_id"].(string)
		order = append(order, cid)
		byID[cid] = m
		// The query chunk is never its own neighbour.
		if cid == idA {
			t.Fatalf("related must exclude the query chunk itself; got order %v", order)
		}
		// Scores must be present and non-increasing (MemoryEmbedSearch ranks desc).
		sc, ok := m["score"].(float64)
		if !ok {
			t.Fatalf("hit %s missing float score: %v", cid, m)
		}
		if sc > lastScore {
			t.Fatalf("scores not descending at %s (%.4f > %.4f); order %v", cid, sc, lastScore, order)
		}
		lastScore = sc
	}

	posB, posC := indexOf(order, idB), indexOf(order, idC)
	if posB < 0 {
		t.Fatalf("related must surface the token-sharing neighbour B; got %v", order)
	}
	if posC >= 0 && posB > posC {
		t.Fatalf("B (shared tokens) must rank above C (disjoint); order %v", order)
	}
	// B is enriched with its structure + carries a positive similarity.
	bm := byID[idB]
	if bm["title"] != "B" {
		t.Errorf("neighbour B title = %v, want B", bm["title"])
	}
	if bm["document_id"] != docID {
		t.Errorf("neighbour B document_id = %v, want %s", bm["document_id"], docID)
	}
	if sc := bm["score"].(float64); sc <= 0 {
		t.Errorf("neighbour B score = %v, want > 0", sc)
	}
}

// TestDocumentRelated_RefusesWithoutEmbedder: related must fail with a clear
// message when the Document has no embedder wired (there is no vector plane).
func TestDocumentRelated_RefusesWithoutEmbedder(t *testing.T) {
	d, ctx, _ := documentFixture(t) // documentFixture wires no Embedder (nil)
	_, r := docExec(t, d, ctx, `{"op":"related","scope":"user","id":"any-chunk-id"}`)
	if !r.IsError {
		t.Fatalf("expected an error without an embedder, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "embedder") {
		t.Fatalf("expected an embedder-required message, got: %s", r.Text)
	}
}

// TestDocumentUnlinkedMentions_CoreBothTiers exercises the whole op on BOTH SQL
// Memory tiers: the edge exclusion, the case-insensitive body scan, self- and
// already-linked exclusion, a non-mentioner staying out, and truncation.
func TestDocumentUnlinkedMentions_CoreBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertUnlinkedMentions(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertUnlinkedMentions(t, d, ctx)
	})
}

func assertUnlinkedMentions(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Catalog","path":"/cat/one"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := out["document_id"].(string)

	mk := func(title, body string) string {
		o, rr := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, docID, title, body))
		if rr.IsError {
			t.Fatalf("create_chunk(%s): %s", title, rr.Text)
		}
		return o["id"].(string)
	}

	// The target chunk. Its own body mentions its own title → must be excluded.
	target := mk("Widget", "The Widget is the core primitive.")
	// Two unlinked mentioners (one exact-case, one lower-case → case-insensitive).
	m1 := mk("M1", "the Widget is great to use")
	m2 := mk("M2", "we depend on the widget here")
	// A chunk that mentions Widget via an inline [[name]] link → auto edge → excluded.
	linkedInline := mk("LinkedInline", "see [[Widget]] for the full spec")
	// A chunk that mentions Widget and is manually linked → excluded.
	linkedManual := mk("LinkedManual", "the Widget really rocks")
	// A chunk that does not mention it at all → never returned.
	noMention := mk("None", "nothing relevant here")

	// Manual link LinkedManual → target.
	if _, rr := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"references"}`, linkedManual, target)); rr.IsError {
		t.Fatalf("link_chunks: %s", rr.Text)
	}

	// The [[Widget]] inline link must have materialized an auto edge to the target.
	// (Guards against the substring match silently including it.)
	blOut, rr := docExec(t, d, ctx, fmt.Sprintf(`{"op":"backlinks","scope":"user","id":%q}`, target))
	if rr.IsError {
		t.Fatalf("backlinks: %s", rr.Text)
	}
	froms := map[string]bool{}
	for _, e := range blOut["backlinks"].([]any) {
		froms[e.(map[string]any)["from_id"].(string)] = true
	}
	if !froms[linkedInline] || !froms[linkedManual] {
		t.Fatalf("precondition: expected inline+manual edges into target; backlinks froms=%v", froms)
	}

	out, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"unlinked_mentions","scope":"user","id":%q}`, target))
	if r.IsError {
		t.Fatalf("unlinked_mentions: %s", r.Text)
	}
	got := map[string]map[string]any{}
	for _, e := range out["unlinked_mentions"].([]any) {
		m := e.(map[string]any)
		got[m["chunk_id"].(string)] = m
	}

	// Exactly the two unlinked mentioners, nothing else.
	if len(got) != 2 {
		t.Fatalf("unlinked_mentions = %d entries, want 2 (M1,M2); got ids %v", len(got), keysOf(got))
	}
	for _, want := range []string{m1, m2} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected %s among unlinked mentions; got %v", want, keysOf(got))
		}
	}
	for _, excluded := range []string{target, linkedInline, linkedManual, noMention} {
		if _, ok := got[excluded]; ok {
			t.Errorf("chunk %s must be excluded from unlinked_mentions; got %v", excluded, keysOf(got))
		}
	}
	// Enrichment: each hit carries its title + document_id.
	if got[m1]["title"] != "M1" || got[m1]["document_id"] != docID {
		t.Errorf("m1 enrichment = %v, want title M1 / document_id %s", got[m1], docID)
	}
	// Not truncated when the limit comfortably exceeds the match count.
	if out["truncated"] != false {
		t.Errorf("truncated = %v, want false", out["truncated"])
	}

	// A low limit must report truncated=true and return exactly one match.
	out, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"unlinked_mentions","scope":"user","id":%q,"limit":1}`, target))
	if r.IsError {
		t.Fatalf("unlinked_mentions(limit=1): %s", r.Text)
	}
	if n := len(out["unlinked_mentions"].([]any)); n != 1 {
		t.Errorf("unlinked_mentions(limit=1) returned %d, want 1", n)
	}
	if out["truncated"] != true {
		t.Errorf("unlinked_mentions(limit=1) truncated = %v, want true", out["truncated"])
	}
}

// keysOf returns the keys of a result map (for readable failure messages).
func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
