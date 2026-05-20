package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ---- Fakes for the v0.9.0 Vector Memory Memory-tool tests ----
//
// We layer two fakes over the existing sqlite-backed test fixture:
//
//   - vectorStore wraps a real store.Store and overrides
//     SupportsVectors() + the MemoryEmbed* methods with an in-memory
//     map. This lets us drive the Memory tool's search / embed-on-set
//     paths without needing a Postgres+pgvector container in the
//     unit-test path.
//
//   - fakeEmbedder returns deterministic vectors derived from text
//     prefixes so the test can assert ordering without floating-point
//     fuzz.
//
// Both fakes are package-private + test-only.

type vectorStore struct {
	store.Store
	mu      sync.Mutex
	embeds  map[string]store.MemoryEmbedding // key = scope|id|key
	enabled bool
}

func newVectorStore(s store.Store) *vectorStore {
	return &vectorStore{Store: s, embeds: map[string]store.MemoryEmbedding{}, enabled: true}
}

func vsKey(scope store.MemoryScope, scopeID, key string) string {
	return string(scope) + "|" + scopeID + "|" + key
}

func (v *vectorStore) SupportsVectors() bool { return v.enabled }

func (v *vectorStore) MemoryEmbedSet(ctx context.Context, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	if !v.enabled {
		return store.ErrVectorUnsupported
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.embeds[vsKey(scope, scopeID, key)] = e
	return nil
}

func (v *vectorStore) MemoryEmbedGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEmbedding, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.embeds[vsKey(scope, scopeID, key)]
	if !ok {
		return store.MemoryEmbedding{}, &store.ErrNotFound{Kind: "memory_embedding", ID: key}
	}
	return e, nil
}

// cosineSim is the v0.9.0 search ranking function the Memory tool
// surfaces as MemorySearchEntry.Score. We compute it directly here so
// the fake matches what the Postgres adapter produces (1 - cosine_dist).
func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) {
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
	return dot / (sqrt(na) * sqrt(nb))
}

// sqrt avoids pulling in math just for one call.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton's method, 8 iterations — plenty for test vectors.
	z := x
	for i := 0; i < 8; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func (v *vectorStore) MemoryEmbedSearch(ctx context.Context, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	if !v.enabled {
		return nil, store.ErrVectorUnsupported
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}
	// Dim mismatch detection — take the first stored vector under (scope, scope_id).
	prefix := string(scope) + "|" + scopeID + "|"
	var storedDim int
	for k, e := range v.embeds {
		if strings.HasPrefix(k, prefix) {
			storedDim = e.Dimension
			break
		}
	}
	if storedDim == 0 {
		return nil, nil // empty scope
	}
	if storedDim != len(query) {
		return nil, &store.MemoryError{
			Code: store.ErrDimensionMismatch.Code,
			Msg:  fmt.Sprintf("memory: query embedding dimension %d does not match stored rows' dimension %d", len(query), storedDim),
		}
	}

	type scored struct {
		key   string
		score float64
		emb   store.MemoryEmbedding
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
		// Filter expired base rows.
		entry, err := v.Store.MemoryGet(ctx, scope, scopeID, key)
		if err != nil {
			continue
		}
		rows = append(rows, scored{key: key, score: cosineSim(query, e.Vector), emb: e})
		_ = entry
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	if len(rows) > topK {
		rows = rows[:topK]
	}
	out := make([]store.MemorySearchEntry, 0, len(rows))
	for _, r := range rows {
		entry, err := v.Store.MemoryGet(ctx, scope, scopeID, r.key)
		if err != nil {
			continue
		}
		se := store.MemorySearchEntry{
			MemoryEntry: entry,
			Score:       r.score,
		}
		se.EmbeddedWith.Provider = r.emb.Provider
		se.EmbeddedWith.Model = r.emb.Model
		out = append(out, se)
	}
	return out, nil
}

func (v *vectorStore) MemoryEmbedListByModel(ctx context.Context, scope store.MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]store.MemoryEntry, error) {
	return nil, errors.New("MemoryEmbedListByModel not implemented in fake")
}

func (v *vectorStore) MemoryEmbedStats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	return store.MemoryEmbedStats{}, errors.New("MemoryEmbedStats not implemented in fake")
}

// fakeEmbedder returns a deterministic vector based on a token map.
// Each text is split on whitespace; each unique token claims one
// dimension. The vector is a one-hot encoding of which tokens are
// present. This gives clean cosine similarity for matching queries.
//
// Dimensions are negotiated by the test setup: callers provide the
// token vocabulary up front so the vector dim stays consistent
// across embed/search calls.
type fakeEmbedder struct {
	vocab    map[string]int // token → vector index
	provider string
	model    string
	failNext bool // when set, the next Embed() returns an error
}

func newFakeEmbedder(provider, model string, tokens ...string) *fakeEmbedder {
	v := map[string]int{}
	for i, t := range tokens {
		v[t] = i
	}
	return &fakeEmbedder{vocab: v, provider: provider, model: model}
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.failNext {
		f.failNext = false
		return nil, errors.New("fake embedder injected failure")
	}
	out := make([][]float32, len(texts))
	for i, txt := range texts {
		vec := make([]float32, len(f.vocab))
		// Strip JSON syntax + punctuation before tokenizing so the
		// "value-as-text fallback" path (which feeds the
		// JSON-encoded value through, including surrounding quotes
		// and structural punctuation) still produces useful tokens.
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

func (f *fakeEmbedder) Provider() string { return f.provider }
func (f *fakeEmbedder) Model() string    { return f.model }
func (f *fakeEmbedder) Dimension() int   { return len(f.vocab) }

// vectorMemoryFixture builds a Memory tool with the in-memory
// vector store + fake embedder pre-wired. Returns the tool, ctx, and
// the cleanup func (closes the underlying SQLite).
func vectorMemoryFixture(t *testing.T) (*Memory, *fakeEmbedder, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	vs := newVectorStore(s)
	emb := newFakeEmbedder("fake", "fake-embed-001",
		"alice", "bob", "go", "rust", "python", "data", "science", "systems", "developer", "programmer")
	tool := &Memory{
		Store:             vs,
		MaxValueBytes:     65536,
		DefaultQuotaBytes: 1 << 20,
		Embedder:          emb,
	}
	ctx := tools.WithAgentName(context.Background(), "qa-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
	})
	return tool, emb, ctx, func() { _ = s.Close() }
}

// ---- Tests ----

func TestMemoryVector_SearchHappyPath(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()

	// Two rows, each with an embed_text covering different tokens.
	res, _ := tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"agent","key":"rec1",
		"value":{"name":"Alice","skills":["Go","Rust"]},
		"embed":true,"embed_text":"alice go rust developer"
	}`))
	if res.IsError {
		t.Fatalf("set rec1: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"embedded":true`) {
		t.Errorf("set rec1 missing embedded:true: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"agent","key":"rec2",
		"value":{"name":"Bob","skills":["Python","data science"]},
		"embed":true,"embed_text":"bob python data science"
	}`))
	if res.IsError {
		t.Fatalf("set rec2: %s", res.Text)
	}

	// Query for "systems programmer" — neither rec covers those
	// tokens exactly, but we'll use "go rust developer" which is in
	// the vocab to demonstrate that rec1 ranks above rec2.
	res, _ = tool.Execute(ctx, json.RawMessage(`{
		"op":"search","scope":"agent","query":"go rust developer","top_k":5
	}`))
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"key":"rec1"`) {
		t.Errorf("search must surface rec1: %s", res.Text)
	}
	// rec1 must appear before rec2 in the result list.
	if i, j := strings.Index(res.Text, "rec1"), strings.Index(res.Text, "rec2"); i < 0 || (j >= 0 && i > j) {
		t.Errorf("expected rec1 to rank above rec2: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"provider":"fake"`) || !strings.Contains(res.Text, `"embedded_with":`) {
		t.Errorf("search response missing embedded_with metadata: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"query_embedding_dim":10`) {
		t.Errorf("response missing query_embedding_dim: %s", res.Text)
	}
}

func TestMemoryVector_SearchRefusesWithoutVectorSupport(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	// Disable vector support on the fake store; everything else
	// stays wired.
	tool.Store.(*vectorStore).enabled = false

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent","query":"x"}`))
	if !res.IsError {
		t.Errorf("expected vector_unsupported error, got %s", res.Text)
	}
	if !strings.Contains(res.Text, "vector index not configured") {
		t.Errorf("expected vector-unsupported message, got %s", res.Text)
	}
}

func TestMemoryVector_SearchRefusesWithoutEmbedder(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	tool.Embedder = nil

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent","query":"x"}`))
	if !res.IsError {
		t.Errorf("expected embedder_not_configured, got %s", res.Text)
	}
	if !strings.Contains(res.Text, "no embedder configured") {
		t.Errorf("expected embedder-not-configured message, got %s", res.Text)
	}
}

func TestMemoryVector_SearchMissingQuery(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent"}`))
	if !res.IsError || !strings.Contains(res.Text, "query") {
		t.Errorf("expected missing-query error, got %s", res.Text)
	}
}

func TestMemoryVector_SetEmbedTrueSucceeds(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"user","key":"profile",
		"value":{"likes":"go"},
		"embed":true,"embed_text":"alice go"
	}`))
	if res.IsError {
		t.Fatalf("set: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"ok":true`) {
		t.Errorf("set should report ok:true: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"embedded":true`) {
		t.Errorf("set should report embedded:true: %s", res.Text)
	}
	// Confirm the embedding is stored under the right scope.
	vs := tool.Store.(*vectorStore)
	emb, err := vs.MemoryEmbedGet(ctx, store.MemoryScopeUser, "alice", "profile")
	if err != nil {
		t.Fatalf("embedding not stored: %v", err)
	}
	if emb.Provider != "fake" || emb.Model != "fake-embed-001" {
		t.Errorf("embedding metadata wrong: %+v", emb)
	}
	if emb.EmbedText != "alice go" {
		t.Errorf("embed_text wrong: %q", emb.EmbedText)
	}
}

func TestMemoryVector_SetEmbedFalseDoesNotEmbed(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"user","key":"profile",
		"value":{"likes":"go"}
	}`))
	if res.IsError {
		t.Fatalf("set: %s", res.Text)
	}
	if strings.Contains(res.Text, `"embedded"`) {
		t.Errorf("response should NOT mention embedded when embed:false: %s", res.Text)
	}
	vs := tool.Store.(*vectorStore)
	if _, err := vs.MemoryEmbedGet(ctx, store.MemoryScopeUser, "alice", "profile"); err == nil {
		t.Errorf("embedding should NOT have been written when embed:false")
	}
}

func TestMemoryVector_SetEmbedTrueFailsButKVStillLands(t *testing.T) {
	tool, emb, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	emb.failNext = true
	res, _ := tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"user","key":"k",
		"value":{"v":1},
		"embed":true,"embed_text":"alice"
	}`))
	if res.IsError {
		t.Fatalf("set should NOT fail on embed failure: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"ok":true`) {
		t.Errorf("ok:true should still surface: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"embedded":false`) {
		t.Errorf("embedded:false expected on embed failure: %s", res.Text)
	}
	if !strings.Contains(res.Text, "embed_warning") {
		t.Errorf("embed_warning expected on embed failure: %s", res.Text)
	}
	// k/v row must still be retrievable.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"k"}`))
	if res.IsError || !strings.Contains(res.Text, `"v":1`) {
		t.Errorf("get after embed-fail should return the value: %s", res.Text)
	}
}

func TestMemoryVector_SetEmbedTrueFallsBackToValueAsText(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	// No embed_text — the value JSON itself ("alice go") is used.
	res, _ := tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"user","key":"k",
		"value":"alice go",
		"embed":true
	}`))
	if res.IsError {
		t.Fatalf("set: %s", res.Text)
	}
	vs := tool.Store.(*vectorStore)
	emb, err := vs.MemoryEmbedGet(ctx, store.MemoryScopeUser, "alice", "k")
	if err != nil {
		t.Fatalf("embedding missing: %v", err)
	}
	// JSON-encoded "alice go" is "\"alice go\"" — the fake embedder
	// strips quotes via strings.Fields + ToLower, so "alice" + "go"
	// tokens light up.
	if emb.Vector[0] != 1 { // alice
		t.Errorf("alice token bit unset: %v", emb.Vector)
	}
	if emb.Vector[2] != 1 { // go
		t.Errorf("go token bit unset: %v", emb.Vector)
	}
}

func TestMemoryVector_SearchHonorsTopK(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	// Write 5 rows.
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		body := fmt.Sprintf(`{"op":"set","scope":"agent","key":%q,"value":1,"embed":true,"embed_text":"alice"}`, name)
		res, _ := tool.Execute(ctx, json.RawMessage(body))
		if res.IsError {
			t.Fatalf("set %s: %s", name, res.Text)
		}
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent","query":"alice","top_k":2}`))
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	// Count rec entries (each has its own "key":).
	count := strings.Count(res.Text, `"key":`)
	if count != 2 {
		t.Errorf("top_k=2 returned %d entries: %s", count, res.Text)
	}
	if !strings.Contains(res.Text, `"truncated":true`) {
		t.Errorf("truncated:true expected when result fills top_k: %s", res.Text)
	}
}

func TestMemoryVector_SearchScopeIsolation(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	// Set under agent scope.
	tool.Execute(ctx, json.RawMessage(`{
		"op":"set","scope":"agent","key":"k",
		"value":1,"embed":true,"embed_text":"alice"
	}`))
	// Search under user scope — must return empty.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"user","query":"alice"}`))
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	// "entries":[] (no rows).
	if !strings.Contains(res.Text, `"entries":[]`) {
		t.Errorf("cross-scope search must return empty entries, got %s", res.Text)
	}
}

func TestMemoryVector_SearchKeyPrefix(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	// Two key-namespaces under one scope.
	tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"job/golang","value":1,"embed":true,"embed_text":"alice go"}`))
	tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"note/general","value":1,"embed":true,"embed_text":"alice go"}`))

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent","query":"alice go","prefix":"job/"}`))
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"key":"job/golang"`) {
		t.Errorf("prefix='job/' should include job/golang: %s", res.Text)
	}
	if strings.Contains(res.Text, `"key":"note/general"`) {
		t.Errorf("prefix='job/' should EXCLUDE note/general: %s", res.Text)
	}
}

func TestMemoryVector_SearchDimensionMismatch(t *testing.T) {
	// Two different embedders → two different dims → mismatch.
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	vs := newVectorStore(s)

	emb4 := newFakeEmbedder("fake", "small", "alice", "bob", "go", "rust")
	tool := &Memory{Store: vs, Embedder: emb4, MaxValueBytes: 65536, DefaultQuotaBytes: 1 << 20}
	ctx := tools.WithAgentName(context.Background(), "qa")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent"}})

	// Set under the 4-dim embedder.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"k","value":1,"embed":true,"embed_text":"alice"}`))
	if res.IsError {
		t.Fatalf("set: %s", res.Text)
	}
	// Swap to an 8-dim embedder + search → should refuse with dim mismatch.
	tool.Embedder = newFakeEmbedder("fake", "large", "a", "b", "c", "d", "e", "f", "g", "h")
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent","query":"a"}`))
	if !res.IsError {
		t.Errorf("expected dim-mismatch refusal, got %s", res.Text)
	}
	if !strings.Contains(res.Text, "dimension") {
		t.Errorf("error should mention dimension: %s", res.Text)
	}
}

func TestMemoryVector_UnknownOpStillNamesSearch(t *testing.T) {
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"hallucinate","scope":"agent"}`))
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Text, "search") {
		t.Errorf("error message should list search among valid ops: %s", res.Text)
	}
}
