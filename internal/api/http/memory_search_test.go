package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// searchVectorStore wraps a real Store with an in-memory embedding map and
// reports SupportsVectors()=true, so the /v1/_memory/search handler can be
// exercised without a Postgres+pgvector container. MemoryEmbedSearch returns the
// live k/v values (read from the wrapped store) for every embedded key under the
// requested (tenant, scope, scope_id) — this double tests the HANDLER's
// kind/chunk_id mapping and tenant routing, not the store's cosine math (covered
// by the store contract suite). It partitions by tenant so the isolation test
// can prove the handler stamps the principal's tenant.
type searchVectorStore struct {
	store.Store
	mu     sync.Mutex
	embeds map[string]store.MemoryEmbedding // "tenant|scope|scopeID|key" → embedding
}

func newSearchVectorStore(s store.Store) *searchVectorStore {
	return &searchVectorStore{Store: s, embeds: map[string]store.MemoryEmbedding{}}
}

func (v *searchVectorStore) SupportsVectors() bool { return true }

func (v *searchVectorStore) MemoryEmbedSet(_ context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.embeds[tenantID+"|"+string(scope)+"|"+scopeID+"|"+key] = e
	return nil
}

func (v *searchVectorStore) MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string, filter store.MemorySearchFilter, _ []float32, topK int) ([]store.MemorySearchEntry, error) {
	v.mu.Lock()
	prefix := tenantID + "|" + string(scope) + "|" + scopeID + "|"
	keys := []string{}
	models := map[string]store.MemoryEmbedding{}
	for k, e := range v.embeds {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		key := strings.TrimPrefix(k, prefix)
		if filter.KeyPrefix != "" && !strings.HasPrefix(key, filter.KeyPrefix) {
			continue
		}
		keys = append(keys, key)
		models[key] = e
	}
	v.mu.Unlock()

	// Deterministic order (by key) so the test is stable; cosine order is not
	// what this double is asserting.
	sort.Strings(keys)
	out := make([]store.MemorySearchEntry, 0, len(keys))
	for i, key := range keys {
		// Read the live value so dropDeadLinks (which drops empty-Value hits)
		// keeps it — mirrors a consistent store where the body outlives nothing.
		entry, err := v.Store.MemoryGet(ctx, tenantID, scope, scopeID, key)
		if err != nil {
			continue
		}
		se := store.MemorySearchEntry{MemoryEntry: entry, Score: 1.0 - float64(i)*0.01}
		se.EmbeddedWith.Provider = models[key].Provider
		se.EmbeddedWith.Model = models[key].Model
		out = append(out, se)
		if topK > 0 && len(out) >= topK {
			break
		}
	}
	return out, nil
}

func memorySearchServer(t *testing.T) (*Server, *searchVectorStore) {
	t.Helper()
	base, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })
	vs := newSearchVectorStore(base)
	return &Server{store: vs, embedder: &backfillEmbedder{}}, vs
}

// seedSearchRow writes a k/v value AND its embedding under one tenant so the
// double's MemoryEmbedSearch returns it.
func seedSearchRow(t *testing.T, vs *searchVectorStore, tenant, scopeID, key, value string) {
	t.Helper()
	if err := vs.MemorySet(context.Background(), tenant, store.MemoryScopeUser, scopeID, key, []byte(value), 0); err != nil {
		t.Fatalf("MemorySet(%s/%s): %v", tenant, key, err)
	}
	if err := vs.MemoryEmbedSet(context.Background(), tenant, store.MemoryScopeUser, scopeID, key,
		store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
		t.Fatalf("MemoryEmbedSet(%s/%s): %v", tenant, key, err)
	}
}

func postMemorySearch(s *Server, tenant, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/search", strings.NewReader(body)).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: tenant, Subject: "op", Scopes: []string{auth.ScopeAdmin},
		}))
	s.handleMemorySearch(rec, req)
	return rec
}

// TestMemorySearch_UnifiedKvAndDocChunk: one search spans BOTH a plain k/v entry
// (kind=memory) and a document-chunk body (kind=document, carrying chunk_id) —
// the whole reason the endpoint runs with an empty key prefix.
func TestMemorySearch_UnifiedKvAndDocChunk(t *testing.T) {
	s, vs := memorySearchServer(t)
	seedSearchRow(t, vs, "A", "alice", "voice", `"warm and direct"`)
	seedSearchRow(t, vs, "A", "alice", "doc.chunk:chunk-1", `{"body":"Ada was a mathematician","fields":null}`)

	rec := postMemorySearch(s, "A", `{"query":"ada","scope":"user","scope_id":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp memorySearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var gotMemory, gotDocument bool
	var docChunkID string
	for _, e := range resp.Entries {
		switch e.Kind {
		case "memory":
			gotMemory = true
			if e.ChunkID != "" {
				t.Errorf("a memory hit must not carry chunk_id: %+v", e)
			}
		case "document":
			gotDocument = true
			docChunkID = e.ChunkID
		default:
			t.Errorf("unexpected kind %q", e.Kind)
		}
	}
	if !gotMemory {
		t.Errorf("no kind=memory hit for the k/v entry: %+v", resp.Entries)
	}
	if !gotDocument {
		t.Errorf("no kind=document hit for the doc.chunk body: %+v", resp.Entries)
	}
	if docChunkID != "chunk-1" {
		t.Errorf("document hit chunk_id = %q, want chunk-1", docChunkID)
	}
}

// TestMemorySearch_TenantIsolation is the security regression: a search as
// principal A must return ONLY A's rows, never B's — even when B holds the SAME
// (scope, scope_id, key). This FAILS if the handler's tools.WithRunIdentity
// tenant stamp is removed (the in-process backend would then run at the shared
// "" tenant), which is the whole reason the stamp exists.
func TestMemorySearch_TenantIsolation(t *testing.T) {
	s, vs := memorySearchServer(t)
	seedSearchRow(t, vs, "A", "alice", "voice", `"alice-A-voice"`)
	seedSearchRow(t, vs, "B", "alice", "voice", `"bob-B-voice"`)

	rec := postMemorySearch(s, "A", `{"query":"voice","scope":"user","scope_id":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp memorySearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var sawA bool
	for _, e := range resp.Entries {
		if strings.Contains(string(e.Value), "bob-B-voice") {
			t.Errorf("tenant A's search returned tenant B's row: %s", string(e.Value))
		}
		if strings.Contains(string(e.Value), "alice-A-voice") {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("tenant A's own row was not returned — the principal tenant was not stamped onto the search; entries: %+v", resp.Entries)
	}
}

// TestMemorySearch_MissingQuery: an empty query is a 400, not an embed of "".
func TestMemorySearch_MissingQuery(t *testing.T) {
	s, _ := memorySearchServer(t)
	rec := postMemorySearch(s, "A", `{"scope":"user","scope_id":"alice"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "missing_query" {
		t.Errorf("code = %v, want missing_query", e["code"])
	}
}

// TestMemorySearch_InvalidScope: only the closed admin scope set is accepted.
func TestMemorySearch_InvalidScope(t *testing.T) {
	s, _ := memorySearchServer(t)
	rec := postMemorySearch(s, "A", `{"query":"x","scope":"tenant","scope_id":"alice"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "invalid_scope" {
		t.Errorf("code = %v, want invalid_scope", e["code"])
	}
}
