package inprocess_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These tests cover the in-process backend in isolation (the Memory tool
// has its own suite). They use a real SQLite store for the k/v
// round-trips, plus a vector-capable wrapper + deterministic fake
// embedder for the Search / embed-on-set paths — the SQLite store ships
// without vector support until v0.9.1, so the wrapper supplies it the
// same way internal/tools/builtin's vector tests do.

func newStore(t *testing.T) (store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	return s, func() { _ = s.Close() }
}

// ---- k/v round-trips (no vector stack needed) ----

func TestInProcess_GetSetDeleteListRoundTrip(t *testing.T) {
	s, cleanup := newStore(t)
	defer cleanup()
	b := inprocess.New(s, nil)
	ctx := context.Background()
	scope, id := store.MemoryScopeAgent, "a1"

	// Set two keys.
	if _, err := b.Set(ctx, scope, id, "alpha", json.RawMessage(`{"n":1}`), memory.SetOptions{}); err != nil {
		t.Fatalf("set alpha: %v", err)
	}
	if _, err := b.Set(ctx, scope, id, "beta", json.RawMessage(`2`), memory.SetOptions{}); err != nil {
		t.Fatalf("set beta: %v", err)
	}

	// Get back.
	e, err := b.Get(ctx, scope, id, "alpha")
	if err != nil {
		t.Fatalf("get alpha: %v", err)
	}
	if string(e.Value) != `{"n":1}` {
		t.Errorf("alpha value = %s", e.Value)
	}

	// List with prefix.
	entries, truncated, err := b.List(ctx, scope, id, "al", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if truncated {
		t.Errorf("unexpected truncated")
	}
	if len(entries) != 1 || entries[0].Key != "alpha" {
		t.Errorf("prefix list = %+v, want only alpha", entries)
	}

	// Delete reports existence.
	deleted, err := b.Delete(ctx, scope, id, "alpha")
	if err != nil {
		t.Fatalf("delete alpha: %v", err)
	}
	if !deleted {
		t.Errorf("delete alpha should report deleted=true")
	}
	deleted, err = b.Delete(ctx, scope, id, "alpha")
	if err != nil {
		t.Fatalf("delete alpha again: %v", err)
	}
	if deleted {
		t.Errorf("second delete should report deleted=false")
	}

	// Get on a missing key returns *store.ErrNotFound (the tool maps this
	// to {"value": null}).
	if _, err := b.Get(ctx, scope, id, "alpha"); err == nil {
		t.Errorf("get after delete should error")
	} else {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			t.Errorf("get after delete err = %v, want *store.ErrNotFound", err)
		}
	}
}

// ---- vector paths: wrapper store + fake embedder ----

type vectorStore struct {
	store.Store
	mu     sync.Mutex
	embeds map[string]store.MemoryEmbedding
}

func newVectorStore(s store.Store) *vectorStore {
	return &vectorStore{Store: s, embeds: map[string]store.MemoryEmbedding{}}
}

func vsKey(scope store.MemoryScope, id, key string) string {
	return string(scope) + "|" + id + "|" + key
}

func (v *vectorStore) SupportsVectors() bool { return true }

func (v *vectorStore) MemoryEmbedSet(_ context.Context, _ string, scope store.MemoryScope, id, key string, e store.MemoryEmbedding) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.embeds[vsKey(scope, id, key)] = e
	return nil
}

func (v *vectorStore) MemoryEmbedSearch(ctx context.Context, _ string, scope store.MemoryScope, id, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if topK > 51 {
		topK = 51
	}
	prefix := string(scope) + "|" + id + "|"
	type scored struct {
		key string
		s   float64
		emb store.MemoryEmbedding
	}
	var rows []scored
	for k, e := range v.embeds {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		key := strings.TrimPrefix(k, prefix)
		if keyPrefix != "" && !strings.HasPrefix(key, keyPrefix) {
			continue
		}
		rows = append(rows, scored{key: key, s: cosine(query, e.Vector), emb: e})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].s > rows[j].s })
	if len(rows) > topK {
		rows = rows[:topK]
	}
	out := make([]store.MemorySearchEntry, 0, len(rows))
	for _, r := range rows {
		entry, err := v.Store.MemoryGet(ctx, "", scope, id, r.key)
		if err != nil {
			continue
		}
		se := store.MemorySearchEntry{MemoryEntry: entry, Score: r.s}
		se.EmbeddedWith.Provider = r.emb.Provider
		se.EmbeddedWith.Model = r.emb.Model
		// Hand back the stored vector so the in-process backend's MR-5 dedup
		// pass has per-entry vectors to compare — mirrors the real
		// sqlite/pgvector stores after the MR-5 store change.
		se.Vector = r.emb.Vector
		out = append(out, se)
	}
	return out, nil
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	// 8-iteration Newton sqrt — plenty for the one-hot test vectors.
	sq := func(x float64) float64 {
		if x <= 0 {
			return 0
		}
		z := x
		for i := 0; i < 8; i++ {
			z = (z + x/z) / 2
		}
		return z
	}
	return dot / (sq(na) * sq(nb))
}

// fakeEmbedder one-hot encodes whitespace tokens against a fixed vocab.
type fakeEmbedder struct {
	vocab    map[string]int
	failNext bool
}

func newFakeEmbedder(tokens ...string) *fakeEmbedder {
	v := map[string]int{}
	for i, t := range tokens {
		v[t] = i
	}
	return &fakeEmbedder{vocab: v}
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.failNext {
		f.failNext = false
		return nil, errors.New("injected embed failure")
	}
	out := make([][]float32, len(texts))
	for i, txt := range texts {
		vec := make([]float32, len(f.vocab))
		clean := strings.Map(func(r rune) rune {
			switch r {
			case '"', '{', '}', '[', ']', ',', ':':
				return ' '
			}
			return r
		}, txt)
		for _, tok := range strings.Fields(strings.ToLower(clean)) {
			if idx, ok := f.vocab[tok]; ok {
				vec[idx] = 1
			}
		}
		out[i] = vec
	}
	return out, nil
}

func (f *fakeEmbedder) Provider() string { return "fake" }
func (f *fakeEmbedder) Model() string    { return "fake-001" }
func (f *fakeEmbedder) Dimension() int   { return len(f.vocab) }

func vectorFixture(t *testing.T) (*inprocess.Backend, *vectorStore, *fakeEmbedder, func()) {
	t.Helper()
	s, cleanup := newStore(t)
	vs := newVectorStore(s)
	emb := newFakeEmbedder("alice", "bob", "go", "rust", "python")
	return inprocess.New(vs, emb), vs, emb, cleanup
}

func TestInProcess_SetEmbedThenSearchRanks(t *testing.T) {
	b, vs, _, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	scope, id := store.MemoryScopeAgent, "a1"

	if r, err := b.Set(ctx, scope, id, "rec1", json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "alice go rust"}); err != nil || !r.Embedded {
		t.Fatalf("set rec1: r=%+v err=%v", r, err)
	}
	if r, err := b.Set(ctx, scope, id, "rec2", json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "bob python"}); err != nil || !r.Embedded {
		t.Fatalf("set rec2: r=%+v err=%v", r, err)
	}
	// The embedding row landed in the store.
	if _, ok := vs.embeds[vsKey(scope, id, "rec1")]; !ok {
		t.Fatalf("rec1 embedding not stored")
	}

	res, err := b.Search(ctx, scope, id, memory.SearchQuery{QueryText: "go rust", TopK: 5}, memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(res.Entries))
	}
	if res.Entries[0].Key != "rec1" {
		t.Errorf("rec1 should rank first, got order %s,%s", res.Entries[0].Key, res.Entries[1].Key)
	}
	if res.QueryEmbeddingDim != 5 {
		t.Errorf("query_embedding_dim = %d, want 5", res.QueryEmbeddingDim)
	}
	if len(res.RankScores) != len(res.Entries) {
		t.Errorf("rank scores not index-aligned: %d vs %d", len(res.RankScores), len(res.Entries))
	}
}

// TestInProcess_SearchDedupCollapsesNearDuplicates pins the MR-5 wiring:
// the in-process backend runs dedup AFTER rank and BEFORE the top_k trim,
// using the vectors the store now returns. Three rows embed identical text
// ("alice") — their one-hot vectors are identical, so dedup must collapse
// them to one; a distinct row ("bob") survives.
func TestInProcess_SearchDedupCollapsesNearDuplicates(t *testing.T) {
	b, _, _, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	scope, id := store.MemoryScopeAgent, "a1"

	for _, k := range []string{"d1", "d2", "d3"} {
		if _, err := b.Set(ctx, scope, id, k, json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "alice"}); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	// NOTE: this key must NOT start with "d" — countKeyPrefix(…, "d") below
	// counts the alice cluster (d1/d2/d3), and a "distinct"-style key would
	// collide with that prefix and inflate the count.
	if _, err := b.Set(ctx, scope, id, "other", json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "bob"}); err != nil {
		t.Fatalf("set other: %v", err)
	}

	// With dedup OFF the alice cluster is NOT collapsed (zero-regression).
	off, err := b.Search(ctx, scope, id, memory.SearchQuery{QueryText: "alice", TopK: 10}, memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatalf("search (dedup off): %v", err)
	}
	if off.DedupDropped != 0 {
		t.Errorf("dedup off: DedupDropped = %d, want 0", off.DedupDropped)
	}
	// The three identical-vector rows all match the "alice" query.
	if countKeyPrefix(off.Entries, "d") != 3 {
		t.Fatalf("dedup off: expected all 3 alice rows, got %d", countKeyPrefix(off.Entries, "d"))
	}

	// With dedup ON the alice cluster collapses to one survivor.
	on, err := b.Search(ctx, scope, id, memory.SearchQuery{QueryText: "alice", TopK: 10}, memory.DefaultRankConfig(), memory.DedupConfig{Enabled: true})
	if err != nil {
		t.Fatalf("search (dedup on): %v", err)
	}
	if on.DedupDropped != 2 {
		t.Errorf("dedup on: DedupDropped = %d, want 2", on.DedupDropped)
	}
	if got := countKeyPrefix(on.Entries, "d"); got != 1 {
		t.Errorf("dedup on: alice cluster collapsed to %d, want 1", got)
	}
}

func countKeyPrefix(entries []store.MemorySearchEntry, prefix string) int {
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Key, prefix) {
			n++
		}
	}
	return n
}

func TestInProcess_SearchTruncatedAndTopK(t *testing.T) {
	b, _, _, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	scope, id := store.MemoryScopeAgent, "a1"
	for _, k := range []string{"a", "b", "c", "d"} {
		if _, err := b.Set(ctx, scope, id, k, json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "alice"}); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	res, err := b.Search(ctx, scope, id, memory.SearchQuery{QueryText: "alice", TopK: 2}, memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Errorf("top_k=2 returned %d entries", len(res.Entries))
	}
	if !res.Truncated {
		t.Errorf("4 rows with top_k=2 must be truncated")
	}
}

// RankNote surfaces when a reserved (source/frequency) weight is set.
func TestInProcess_SearchRankNoteOnReservedWeight(t *testing.T) {
	b, _, _, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	scope, id := store.MemoryScopeAgent, "a1"
	if _, err := b.Set(ctx, scope, id, "k", json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "alice"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	cfg := memory.DefaultRankConfig()
	cfg.SourceWeight = 0.5
	res, err := b.Search(ctx, scope, id, memory.SearchQuery{QueryText: "alice", TopK: 5}, cfg, memory.DedupConfig{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.RankNote == "" {
		t.Errorf("expected a rank_note for a non-zero reserved weight")
	}
}

func TestInProcess_SetEmbedTransientFailureIsNonFatal(t *testing.T) {
	b, vs, emb, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	scope, id := store.MemoryScopeUser, "u1"
	emb.failNext = true

	r, err := b.Set(ctx, scope, id, "k", json.RawMessage(`{"v":1}`), memory.SetOptions{Embed: true, EmbedText: "alice"})
	if err != nil {
		t.Fatalf("transient embed failure must NOT fail the set: %v", err)
	}
	if r.Embedded {
		t.Errorf("embedded should be false on transient failure")
	}
	if r.EmbedWarning == "" {
		t.Errorf("embed_warning expected on transient failure")
	}
	// k/v row still landed.
	if _, err := b.Get(ctx, scope, id, "k"); err != nil {
		t.Errorf("k/v row must survive a transient embed failure: %v", err)
	}
	// No embedding row written.
	if _, ok := vs.embeds[vsKey(scope, id, "k")]; ok {
		t.Errorf("no embedding should be stored on transient failure")
	}
}

// ---- nil-embedder refusals (the unconditional-fallback misconfig path) ----

func TestInProcess_SetEmbedRefusesWithoutEmbedder(t *testing.T) {
	s, cleanup := newStore(t)
	defer cleanup()
	vs := newVectorStore(s)
	b := inprocess.New(vs, nil) // vectors supported, but no embedder
	ctx := context.Background()
	scope, id := store.MemoryScopeUser, "u1"

	_, err := b.Set(ctx, scope, id, "k", json.RawMessage(`1`), memory.SetOptions{Embed: true})
	if !errors.Is(err, store.ErrEmbedderNotConfigured) {
		t.Fatalf("want ErrEmbedderNotConfigured, got %v", err)
	}
	// Critical: the k/v row must NOT have been written (upfront refusal).
	if _, err := b.Get(ctx, scope, id, "k"); err == nil {
		t.Errorf("k/v must not land when embed refused upfront")
	}
}

func TestInProcess_SearchRefusesWithoutEmbedder(t *testing.T) {
	s, cleanup := newStore(t)
	defer cleanup()
	vs := newVectorStore(s)
	b := inprocess.New(vs, nil)
	_, err := b.Search(context.Background(), store.MemoryScopeAgent, "a1", memory.SearchQuery{QueryText: "x", TopK: 5}, memory.DefaultRankConfig(), memory.DedupConfig{})
	if !errors.Is(err, store.ErrEmbedderNotConfigured) {
		t.Fatalf("want ErrEmbedderNotConfigured, got %v", err)
	}
}

// A non-embed Set works on a store without vector support — proves the
// k/v path is independent of the vector stack.
func TestInProcess_SetNoEmbedWorksWithoutVectorStack(t *testing.T) {
	s, cleanup := newStore(t)
	defer cleanup()
	b := inprocess.New(s, nil) // bare sqlite: SupportsVectors() == false
	if _, err := b.Set(context.Background(), store.MemoryScopeUser, "u1", "k", json.RawMessage(`{"v":1}`), memory.SetOptions{}); err != nil {
		t.Fatalf("non-embed set should succeed on bare store: %v", err)
	}
}
