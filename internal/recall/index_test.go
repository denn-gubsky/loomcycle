package recall

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// bagEmbedder is a deterministic bag-of-words embedder: each token bumps one
// dimension chosen by its hash. Two texts that share distinctive tokens land near
// each other in cosine space, so a query sharing the needle's words retrieves it.
// Good enough to exercise Harvest/Search ranking without a real embedding model.
type bagEmbedder struct {
	dim  int
	fail bool
}

func (b bagEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if b.fail {
		return nil, errors.New("embed unavailable")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, b.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[h.Sum32()%uint32(b.dim)]++
		}
		out[i] = v
	}
	return out, nil
}

func (b bagEmbedder) Model() string    { return "bag" }
func (b bagEmbedder) Provider() string { return "test" }
func (b bagEmbedder) Dimension() int   { return b.dim }

func userMsg(text string) providers.Message {
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: text}}}
}

func TestIndex_HarvestThenSearchFindsNeedle(t *testing.T) {
	ix := NewIndex(bagEmbedder{dim: 256}, 0)
	ix.Harvest(context.Background(), []providers.Message{
		userMsg("the weather in paris was mild that afternoon"),
		userMsg("the deployment token is zephyr-quartz-42 keep it safe"),
		userMsg("lunch was a sandwich and some coffee"),
		userMsg("the meeting is scheduled for next tuesday morning"),
	})
	if ix.Len() != 4 {
		t.Fatalf("Len = %d, want 4", ix.Len())
	}
	hits, err := ix.Search(context.Background(), "what is the deployment token", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if !strings.Contains(hits[0].Text, "zephyr-quartz-42") {
		t.Fatalf("top hit did not recover the needle: %q", hits[0].Text)
	}
	if hits[0].Source != "run" {
		t.Fatalf("Source = %q, want run", hits[0].Source)
	}
}

func TestIndex_NilEmbedderIsNoOp(t *testing.T) {
	ix := NewIndex(nil, 0)
	ix.Harvest(context.Background(), []providers.Message{userMsg("anything")})
	if ix.Len() != 0 {
		t.Fatalf("Len = %d, want 0 with nil embedder", ix.Len())
	}
	hits, err := ix.Search(context.Background(), "anything", 3)
	if err != nil || hits != nil {
		t.Fatalf("Search on nil-embedder index = (%v, %v), want (nil, nil)", hits, err)
	}
}

func TestIndex_CapEvictsOldest(t *testing.T) {
	ix := NewIndex(bagEmbedder{dim: 64}, 3)
	for _, s := range []string{"alpha one", "bravo two", "charlie three", "delta four", "echo five"} {
		ix.Harvest(context.Background(), []providers.Message{userMsg(s)})
	}
	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3 after cap", ix.Len())
	}
	// The two oldest ("alpha", "bravo") must be gone; the newest kept.
	hits, _ := ix.Search(context.Background(), "echo five", MaxTopK)
	for _, h := range hits {
		if strings.Contains(h.Text, "alpha") || strings.Contains(h.Text, "bravo") {
			t.Fatalf("evicted entry still present: %q", h.Text)
		}
	}
}

func TestIndex_HarvestEmbedFailureSwallowed(t *testing.T) {
	// A harvest whose embed call fails must not break the run: the batch is
	// dropped, the index stays empty, and a later Search is a clean no-op.
	ix := NewIndex(bagEmbedder{dim: 64, fail: true}, 0)
	ix.Harvest(context.Background(), []providers.Message{userMsg("dropped on embed error")})
	if ix.Len() != 0 {
		t.Fatalf("Len = %d, want 0 when the embed call fails", ix.Len())
	}
	if hits, err := ix.Search(context.Background(), "x", 3); err != nil || hits != nil {
		t.Fatalf("Search after failed harvest = (%v, %v), want (nil, nil)", hits, err)
	}
}

func TestIndex_SearchEmbedFailureSurfaces(t *testing.T) {
	// With entries present but the query-embed failing, Search surfaces the error
	// rather than silently returning no hits (the caller should know recall broke,
	// not mistake it for "nothing relevant").
	ix := &Index{embedder: bagEmbedder{dim: 64}, maxEntries: DefaultMaxEntries}
	ix.Harvest(context.Background(), []providers.Message{userMsg("indexed while embedder healthy")})
	if ix.Len() != 1 {
		t.Fatalf("Len = %d, want 1", ix.Len())
	}
	ix.embedder = bagEmbedder{dim: 64, fail: true} // query-time failure only
	if _, err := ix.Search(context.Background(), "x", 3); err == nil {
		t.Fatal("Search should surface the query-embed error when the index is non-empty")
	}
}

func TestIndex_EmptyQueryAndEmptyIndex(t *testing.T) {
	ix := NewIndex(bagEmbedder{dim: 64}, 0)
	if hits, err := ix.Search(context.Background(), "q", 3); err != nil || hits != nil {
		t.Fatalf("empty index Search = (%v, %v), want (nil, nil)", hits, err)
	}
	ix.Harvest(context.Background(), []providers.Message{userMsg("something")})
	if hits, err := ix.Search(context.Background(), "   ", 3); err != nil || hits != nil {
		t.Fatalf("blank query Search = (%v, %v), want (nil, nil)", hits, err)
	}
}

func TestIndex_HarvestSkipsEmptyMessages(t *testing.T) {
	ix := NewIndex(bagEmbedder{dim: 64}, 0)
	ix.Harvest(context.Background(), []providers.Message{
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "image", Data: "abc"}}}, // no text
		userMsg("real content here"),
	})
	if ix.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (empty message skipped)", ix.Len())
	}
}

func TestIndex_ToolResultAndReasoningIndexed(t *testing.T) {
	ix := NewIndex(bagEmbedder{dim: 256}, 0)
	ix.Harvest(context.Background(), []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", Text: "quarterly revenue was 4.2 million dollars"}}},
		{Role: "assistant", Reasoning: "the client prefers vanadium alloy fittings", Content: []providers.ContentBlock{{Type: "text", Text: "ok"}}},
	})
	rev, _ := ix.Search(context.Background(), "revenue", 1)
	if len(rev) == 0 || !strings.Contains(rev[0].Text, "4.2 million") {
		t.Fatalf("tool_result not recalled: %+v", rev)
	}
	al, _ := ix.Search(context.Background(), "vanadium alloy fittings preference", 1)
	if len(al) == 0 || !strings.Contains(al[0].Text, "vanadium") {
		t.Fatalf("reasoning not recalled: %+v", al)
	}
}

func TestContext_RoundTripAndNil(t *testing.T) {
	if FromContext(context.Background()) != nil {
		t.Fatal("FromContext on a bare ctx should be nil")
	}
	// A nil index leaves ctx unchanged.
	if got := NewContext(context.Background(), nil); FromContext(got) != nil {
		t.Fatal("NewContext(nil) should not stamp an index")
	}
	ix := NewIndex(bagEmbedder{dim: 8}, 0)
	ctx := NewContext(context.Background(), ix)
	if FromContext(ctx) != ix {
		t.Fatal("FromContext did not round-trip the index")
	}
}
