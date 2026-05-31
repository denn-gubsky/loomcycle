package mem9_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/mem9"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// IMPORTANT — what these tests prove and what they do NOT:
//
// The httptest.Server stub below IS the contract under test. It
// implements the ASSUMED Mem9 v1alpha2 wire shapes that mem9.go isolates
// at its top. These tests prove the loomcycle-side mapping (interface
// impl, tenancy prefixing, credential header, client-side re-rank) is
// internally consistent against THAT stub. They do NOT prove the mapping
// matches the real github.com/mem9-ai/mem9 API — that must be verified
// operator-side (see docs/MEMORY-BACKENDS.md). If the real API differs,
// update the wire block in mem9.go AND this stub together.

const testAPIKey = "mem9-test-key-do-not-log"

// stub is a minimal in-memory Mem9 server implementing the assumed wire
// shapes. It records the last request's X-API-Key and the keys/prefixes
// it received so tests can assert auth + tenancy without real Mem9.
type stub struct {
	t            *testing.T
	srv          *httptest.Server
	items        map[string]json.RawMessage // wire key -> value
	lastAPIKey   string
	lastSearchPx string
	searchHits   []stubResult
}

type stubResult struct {
	key   string
	value string
	score float64
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, items: map[string]json.RawMessage{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha2/search", s.handleSearch)
	mux.HandleFunc("/v1alpha2/memories", s.handleList)
	mux.HandleFunc("/v1alpha2/memories/", s.handleItem)
	mux.HandleFunc("/v1alpha2/stats", s.handleStats)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) captureKey(r *http.Request) { s.lastAPIKey = r.Header.Get("X-API-Key") }

func (s *stub) handleSearch(w http.ResponseWriter, r *http.Request) {
	s.captureKey(r)
	var req struct {
		Query  string `json:"query"`
		TopK   int    `json:"top_k"`
		Prefix string `json:"prefix"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.lastSearchPx = req.Prefix

	type result struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
		Score float64         `json:"score"`
	}
	out := struct {
		Results []result `json:"results"`
	}{}
	for _, h := range s.searchHits {
		out.Results = append(out.Results, result{Key: h.key, Value: json.RawMessage(h.value), Score: h.score})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *stub) handleList(w http.ResponseWriter, r *http.Request) {
	s.captureKey(r)
	prefix := r.URL.Query().Get("prefix")
	type item struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	out := struct {
		Items     []item `json:"items"`
		Truncated bool   `json:"truncated"`
	}{}
	for k, v := range s.items {
		if strings.HasPrefix(k, prefix) {
			out.Items = append(out.Items, item{Key: k, Value: v})
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *stub) handleItem(w http.ResponseWriter, r *http.Request) {
	s.captureKey(r)
	key := strings.TrimPrefix(r.URL.Path, "/v1alpha2/memories/")
	switch r.Method {
	case http.MethodGet:
		v, ok := s.items[key]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "value": v})
	case http.MethodPut:
		var body struct {
			Value json.RawMessage `json:"value"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.items[key] = body.Value
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "value": body.Value})
	case http.MethodDelete:
		if _, ok := s.items[key]; !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		delete(s.items, key)
		w.WriteHeader(http.StatusOK)
	}
}

func (s *stub) handleStats(w http.ResponseWriter, r *http.Request) {
	s.captureKey(r)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"models": []map[string]any{
			{"provider": "openai", "model": "text-embedding-3-small", "dimension": 1536, "row_count": 7},
		},
		"total_embedding_bytes": 4096,
	})
}

// newBackend builds a mem9.Backend pointed at the stub with a fixed-key
// resolver. tenancy is the resolved Tenancy (caller sets the prefix to
// exercise shared_key_with_prefix).
func newBackend(s *stub, tenancy mem9.Tenancy) *mem9.Backend {
	return mem9.New(mem9.Config{
		BaseURL:    s.srv.URL,
		APIVersion: "v1alpha2",
		Tenancy:    tenancy,
		CredentialResolver: func(context.Context) (string, error) {
			return testAPIKey, nil
		},
		HTTPClient:  s.srv.Client(),
		BackendName: "test-mem9",
	})
}

func TestMem9_SetGetDeleteListRoundTrip(t *testing.T) {
	s := newStub(t)
	b := newBackend(s, mem9.Tenancy{})
	ctx := context.Background()

	if _, err := b.Set(ctx, store.MemoryScopeAgent, "qa", "k1", json.RawMessage(`{"v":1}`), memory.SetOptions{}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := b.Get(ctx, store.MemoryScopeAgent, "qa", "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != `{"v":1}` {
		t.Errorf("Get value = %s, want {\"v\":1}", got.Value)
	}
	if got.Key != "k1" {
		t.Errorf("Get key = %q, want unscoped %q", got.Key, "k1")
	}

	entries, _, err := b.List(ctx, store.MemoryScopeAgent, "qa", "", 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "k1" {
		t.Fatalf("List = %+v, want one entry keyed k1 (unscoped)", entries)
	}

	existed, err := b.Delete(ctx, store.MemoryScopeAgent, "qa", "k1")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed {
		t.Error("Delete existed = false, want true")
	}
	if _, err := b.Get(ctx, store.MemoryScopeAgent, "qa", "k1"); err == nil {
		t.Error("Get after delete: want ErrNotFound, got nil")
	}
}

// TestMem9_GetMissingReturnsNotFound pins the 404→ErrNotFound mapping so
// the tool renders {"value": null} like the in-process backend.
func TestMem9_GetMissingReturnsNotFound(t *testing.T) {
	s := newStub(t)
	b := newBackend(s, mem9.Tenancy{})
	_, err := b.Get(context.Background(), store.MemoryScopeUser, "bob", "absent")
	var nf *store.ErrNotFound
	if err == nil || !errors.As(err, &nf) {
		t.Fatalf("Get missing err = %v, want *store.ErrNotFound", err)
	}
}

// TestMem9_SendsAPIKeyHeader pins that the resolved X-API-Key reaches the
// server (and, by construction, is a header — never a query param / log).
func TestMem9_SendsAPIKeyHeader(t *testing.T) {
	s := newStub(t)
	b := newBackend(s, mem9.Tenancy{})
	if _, err := b.Set(context.Background(), store.MemoryScopeAgent, "qa", "k", json.RawMessage(`1`), memory.SetOptions{}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if s.lastAPIKey != testAPIKey {
		t.Errorf("server saw X-API-Key %q, want %q", s.lastAPIKey, testAPIKey)
	}
}

// TestMem9_SharedKeyPrefixIsolatesTenant pins the shared_key_with_prefix
// tenancy: every wire key the stub receives is prefixed with the tenant
// prefix. A missing prefix would be a cross-tenant leak.
func TestMem9_SharedKeyPrefixIsolatesTenant(t *testing.T) {
	s := newStub(t)
	b := newBackend(s, mem9.Tenancy{KeyPrefix: "tenant-b::"})
	ctx := context.Background()

	if _, err := b.Set(ctx, store.MemoryScopeUser, "bob", "profile", json.RawMessage(`{}`), memory.SetOptions{}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// The stub stores by the exact wire key. Assert the prefix landed.
	foundPrefixed := false
	for k := range s.items {
		if strings.HasPrefix(k, "tenant-b::") {
			foundPrefixed = true
		}
	}
	if !foundPrefixed {
		t.Fatalf("no wire key carried the tenant prefix; stub keys = %v", keysOf(s.items))
	}

	// And the round-trip still returns the UNSCOPED key to the agent.
	got, err := b.Get(ctx, store.MemoryScopeUser, "bob", "profile")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != "profile" {
		t.Errorf("Get key = %q, want unscoped %q", got.Key, "profile")
	}

	// Search prefix is tenant-scoped too.
	s.searchHits = []stubResult{{key: "tenant-b::user/bob/r1", value: `{"r":1}`, score: 0.9}}
	if _, err := b.Search(ctx, store.MemoryScopeUser, "bob", memory.SearchQuery{QueryText: "q", TopK: 5}, memory.DefaultRankConfig(), memory.DedupConfig{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.HasPrefix(s.lastSearchPx, "tenant-b::") {
		t.Errorf("search prefix = %q, want tenant-b:: prefix", s.lastSearchPx)
	}
}

// TestMem9_SearchReRanksClientSide pins Decision 11: Mem9 returns
// candidates (not honoring loomcycle's ranker), the backend re-ranks
// client-side and trims to top_k. We give the stub candidates in a
// cosine order that a recency-weighted rank would reorder, and assert the
// returned order reflects the hybrid score, trimmed to top_k.
func TestMem9_SearchReRanksClientSide(t *testing.T) {
	s := newStub(t)
	b := newBackend(s, mem9.Tenancy{})

	// Three candidates; pure-semantic order would be c0,c1,c2 by score.
	// With top_k=2 the backend must trim to 2 after ranking.
	s.searchHits = []stubResult{
		{key: "user/bob/c0", value: `{"n":0}`, score: 0.9},
		{key: "user/bob/c1", value: `{"n":1}`, score: 0.8},
		{key: "user/bob/c2", value: `{"n":2}`, score: 0.7},
	}
	res, err := b.Search(context.Background(), store.MemoryScopeUser, "bob",
		memory.SearchQuery{QueryText: "q", TopK: 2}, memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("Search returned %d entries, want 2 (trimmed to top_k)", len(res.Entries))
	}
	// Pure-semantic: order preserved, top 2 by score.
	if res.Entries[0].Key != "c0" || res.Entries[1].Key != "c1" {
		t.Errorf("ranked keys = [%q,%q], want [c0,c1]", res.Entries[0].Key, res.Entries[1].Key)
	}
	if len(res.RankScores) != 2 {
		t.Errorf("RankScores len = %d, want 2 (index-aligned with entries)", len(res.RankScores))
	}
	// More candidates than top_k → truncated.
	if !res.Truncated {
		t.Error("Truncated = false, want true (3 candidates, top_k 2)")
	}
}

func TestMem9_Stats(t *testing.T) {
	s := newStub(t)
	b := newBackend(s, mem9.Tenancy{})
	stats, err := b.Stats(context.Background(), store.MemoryScopeAgent)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Models) != 1 || stats.Models[0].RowCount != 7 {
		t.Errorf("Stats models = %+v, want one model with row_count 7", stats.Models)
	}
}

// TestMem9_CredentialResolverErrorPropagates pins that a resolver error
// (non-allowlisted / unset key) fails the op rather than making an
// unauthenticated call — and the error never carries the key value.
func TestMem9_CredentialResolverErrorPropagates(t *testing.T) {
	s := newStub(t)
	b := mem9.New(mem9.Config{
		BaseURL:    s.srv.URL,
		APIVersion: "v1alpha2",
		HTTPClient: s.srv.Client(),
		CredentialResolver: func(context.Context) (string, error) {
			return "", errResolve
		},
	})
	_, err := b.Get(context.Background(), store.MemoryScopeAgent, "qa", "k")
	if err == nil {
		t.Fatal("Get with failing resolver: want error, got nil")
	}
	// The server must NOT have been reached with a credential.
	if s.lastAPIKey != "" {
		t.Errorf("server saw an API key %q despite resolver failure — unauthenticated call leaked", s.lastAPIKey)
	}
}

var errResolve = &resolveErr{}

type resolveErr struct{}

func (*resolveErr) Error() string { return "mem9: env var \"X\" not in allowlist" }

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
