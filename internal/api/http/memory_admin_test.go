package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// ---- Test fakes ----
//
// vectorAdminStore wraps a real Store and overrides the vector
// methods with an in-memory map + reports SupportsVectors() = true.
// Lets the admin endpoint tests exercise both the refusal path
// (SupportsVectors=false, set via wrapper field) and the round-trip
// path without a Postgres+pgvector container.

type vectorAdminStore struct {
	store.Store
	mu          sync.Mutex
	embeds      map[string]store.MemoryEmbedding // (scope,id,key) → embedding
	supports    bool
	staticStats *store.MemoryEmbedStats // when set, returned verbatim
}

func newVectorAdminStore(s store.Store, supports bool) *vectorAdminStore {
	return &vectorAdminStore{Store: s, embeds: map[string]store.MemoryEmbedding{}, supports: supports}
}

func (v *vectorAdminStore) SupportsVectors() bool { return v.supports }

func (v *vectorAdminStore) MemoryEmbedSet(ctx context.Context, _ string, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	if !v.supports {
		return store.ErrVectorUnsupported
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.embeds[string(scope)+"|"+scopeID+"|"+key] = e
	return nil
}

func (v *vectorAdminStore) MemoryEmbedGet(ctx context.Context, _ string, scope store.MemoryScope, scopeID, key string) (store.MemoryEmbedding, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.embeds[string(scope)+"|"+scopeID+"|"+key]
	if !ok {
		return store.MemoryEmbedding{}, &store.ErrNotFound{Kind: "memory_embedding", ID: key}
	}
	return e, nil
}

func (v *vectorAdminStore) MemoryEmbedListByModel(ctx context.Context, _ string, scope store.MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]store.MemoryEntry, error) {
	if !v.supports {
		return nil, store.ErrVectorUnsupported
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := []store.MemoryEntry{}
	prefix := string(scope) + "|" + scopeID + "|"
	for k, e := range v.embeds {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if e.Provider == currentProvider && e.Model == currentModel {
			continue
		}
		key := strings.TrimPrefix(k, prefix)
		entry, err := v.Store.MemoryGet(ctx, "", scope, scopeID, key)
		if err != nil {
			continue
		}
		out = append(out, entry)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (v *vectorAdminStore) MemoryEmbedSearch(ctx context.Context, _ string, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	return nil, errors.New("MemoryEmbedSearch not implemented in admin fake")
}

func (v *vectorAdminStore) MemoryEmbedStats(ctx context.Context, _ string, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	if !v.supports {
		return store.MemoryEmbedStats{}, store.ErrVectorUnsupported
	}
	if v.staticStats != nil {
		return *v.staticStats, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	counts := map[string]int{}
	dims := map[string]int{}
	prefix := string(scope) + "|"
	var totalBytes int64
	for k, e := range v.embeds {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		key := e.Provider + "/" + e.Model
		counts[key]++
		dims[key] = e.Dimension
		totalBytes += int64(e.Dimension) * 4
	}
	stats := store.MemoryEmbedStats{Scope: scope, TotalEmbeddingBytes: totalBytes}
	for k, c := range counts {
		parts := strings.SplitN(k, "/", 2)
		stats.Models = append(stats.Models, store.MemoryEmbedModelStats{
			Provider:  parts[0],
			Model:     parts[1],
			Dimension: dims[k],
			RowCount:  c,
		})
	}
	return stats, nil
}

// adminFakeEmbedder is a deterministic embedder for the reembed
// endpoint tests. Returns 4-dim unit vectors derived from input
// length. failNext=true causes the next Embed() to error.
type adminFakeEmbedder struct {
	provider string
	model    string
	dim      int
	failNext bool
}

func (e *adminFakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if e.failNext {
		e.failNext = false
		return nil, errors.New("fake embedder injected failure")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dim)
		for j := 0; j < e.dim; j++ {
			v[j] = float32(len(t) + j)
		}
		out[i] = v
	}
	return out, nil
}
func (e *adminFakeEmbedder) Provider() string { return e.provider }
func (e *adminFakeEmbedder) Model() string    { return e.model }
func (e *adminFakeEmbedder) Dimension() int   { return e.dim }

// vectorAdminFixture builds a Server with a vector-capable Store
// backed by SQLite + the admin fake embedder. Returns the server
// + the embedder so tests can poke the failNext flag.
func vectorAdminFixture(t *testing.T, supportsVectors bool) (*Server, *adminFakeEmbedder, *vectorAdminStore) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	vs := newVectorAdminStore(st, supportsVectors)
	emb := &adminFakeEmbedder{provider: "openai", model: "text-embedding-3-large", dim: 4}
	cfg := &config.Config{}
	hookReg := hooks.NewRegistry()
	srv := &Server{
		cfg:            cfg,
		store:          vs,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	srv.SetEmbedder(emb)
	return srv, emb, vs
}

// Pre-load: write k/v rows + matching embeddings under (user, alice).
// Two rows are on the OLD embedder (provider+model differ from
// current); one row is on the CURRENT embedder.
func preloadReembedFixture(t *testing.T, srv *Server, vs *vectorAdminStore) {
	t.Helper()
	ctx := context.Background()
	// k/v rows.
	for _, k := range []string{"old1", "old2", "current"} {
		if err := srv.store.MemorySet(ctx, "", store.MemoryScopeUser, "alice", k, []byte(`"x"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Embeddings — two old, one current. Use Store directly to avoid
	// the upfront SupportsVectors check in the admin store wrapper
	// (we want explicit control).
	old := store.MemoryEmbedding{Provider: "openai", Model: "text-embedding-3-small", Dimension: 4, Vector: []float32{1, 0, 0, 0}, EmbedText: "x", CreatedAt: time.Now().UTC()}
	current := store.MemoryEmbedding{Provider: "openai", Model: "text-embedding-3-large", Dimension: 4, Vector: []float32{0, 1, 0, 0}, EmbedText: "x", CreatedAt: time.Now().UTC()}
	vs.embeds["user|alice|old1"] = old
	vs.embeds["user|alice|old2"] = old
	vs.embeds["user|alice|current"] = current
}

// ---- /v1/_memory/embed_stats tests ----

func TestEmbedStats_HappyPath(t *testing.T) {
	srv, _, vs := vectorAdminFixture(t, true)
	preloadReembedFixture(t, srv, vs)

	req := httptest.NewRequest("GET", "/v1/_memory/embed_stats?scope=user", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryEmbedStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp memoryEmbedStatsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scope != "user" {
		t.Errorf("scope=%q want user", resp.Scope)
	}
	// 3 rows total — 2 small + 1 large.
	got := map[string]int{}
	for _, m := range resp.Models {
		got[m.Provider+"/"+m.Model] = m.RowCount
	}
	if got["openai/text-embedding-3-small"] != 2 {
		t.Errorf("small row_count=%d want 2", got["openai/text-embedding-3-small"])
	}
	if got["openai/text-embedding-3-large"] != 1 {
		t.Errorf("large row_count=%d want 1", got["openai/text-embedding-3-large"])
	}
	// 3 rows × 4 dim × 4 bytes = 48.
	if resp.TotalEmbeddingBytes != 48 {
		t.Errorf("total bytes=%d want 48", resp.TotalEmbeddingBytes)
	}
}

func TestEmbedStats_NoVectorSupportReturns503(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, false)
	req := httptest.NewRequest("GET", "/v1/_memory/embed_stats?scope=user", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryEmbedStats(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vector_unsupported") {
		t.Errorf("expected vector_unsupported error code: %s", rec.Body.String())
	}
}

func TestEmbedStats_InvalidScopeReturns400(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	req := httptest.NewRequest("GET", "/v1/_memory/embed_stats?scope=tenant", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryEmbedStats(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestEmbedStats_EmptyScopeReturnsEmptyArray(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	// No preload — scope is empty.
	req := httptest.NewRequest("GET", "/v1/_memory/embed_stats?scope=agent", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryEmbedStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
	}
	// Must serialise an empty array, not null (the UI relies on this).
	if !strings.Contains(rec.Body.String(), `"models":[]`) {
		t.Errorf("expected models:[] for empty scope, got %s", rec.Body.String())
	}
}

func TestEmbedStats_NoStoreReturns503(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("GET", "/v1/_memory/embed_stats?scope=user", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryEmbedStats(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
}

// ---- /v1/_memory/reembed tests ----

func TestReembed_DryRunListsRowsAndSampleKeys(t *testing.T) {
	srv, _, vs := vectorAdminFixture(t, true)
	preloadReembedFixture(t, srv, vs)

	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp memoryReembedDryRunResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.DryRun {
		t.Errorf("DryRun=false, default should be true")
	}
	if resp.RowsToReembed != 2 {
		t.Errorf("RowsToReembed=%d want 2 (old1+old2; current excluded)", resp.RowsToReembed)
	}
	if len(resp.SampleKeys) != 2 {
		t.Errorf("SampleKeys len=%d want 2: %v", len(resp.SampleKeys), resp.SampleKeys)
	}
	if resp.CurrentEmbedder.Provider != "openai" || resp.CurrentEmbedder.Model != "text-embedding-3-large" {
		t.Errorf("current_embedder=%+v", resp.CurrentEmbedder)
	}
}

func TestReembed_DryRunDefaultEvenWithoutFlag(t *testing.T) {
	srv, _, vs := vectorAdminFixture(t, true)
	preloadReembedFixture(t, srv, vs)

	// No dry_run flag → must default to TRUE (safety) — operator
	// typing `curl -X POST .../reembed?scope=user&scope_id=alice`
	// must NOT accidentally re-embed everything.
	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	var resp memoryReembedDryRunResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.DryRun {
		t.Errorf("default dry_run must be true; got false (safety regression)")
	}
	// Confirm no writes happened by re-checking the embedder
	// versions on the two old rows.
	for _, k := range []string{"old1", "old2"} {
		emb, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "alice", k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if emb.Model != "text-embedding-3-small" {
			t.Errorf("dry_run unexpectedly re-embedded %s: model=%s", k, emb.Model)
		}
	}
}

func TestReembed_RealRunReembedsRowsAndUpdatesModel(t *testing.T) {
	srv, _, vs := vectorAdminFixture(t, true)
	preloadReembedFixture(t, srv, vs)

	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice&dry_run=false", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp memoryReembedRealResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.DryRun {
		t.Errorf("DryRun should be false")
	}
	if resp.RowsReembedded != 2 {
		t.Errorf("RowsReembedded=%d want 2", resp.RowsReembedded)
	}
	if resp.RowsFailed != 0 {
		t.Errorf("RowsFailed=%d want 0", resp.RowsFailed)
	}
	// Both old rows must now report the new model.
	for _, k := range []string{"old1", "old2"} {
		emb, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "alice", k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if emb.Model != "text-embedding-3-large" {
			t.Errorf("%s still on old model: %s", k, emb.Model)
		}
	}
	// The already-current row must NOT have been re-embedded —
	// MemoryEmbedListByModel excludes it.
	emb, _ := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "alice", "current")
	if emb.Model != "text-embedding-3-large" {
		t.Errorf("current row unexpectedly changed: %+v", emb)
	}
}

func TestReembed_PartialFailureCollectsFailedKeys(t *testing.T) {
	srv, emb, vs := vectorAdminFixture(t, true)
	preloadReembedFixture(t, srv, vs)
	// First call will fail; second will succeed.
	emb.failNext = true

	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice&dry_run=false", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp memoryReembedRealResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.RowsReembedded != 1 || resp.RowsFailed != 1 {
		t.Errorf("counts: reembedded=%d failed=%d, want 1/1: %s",
			resp.RowsReembedded, resp.RowsFailed, rec.Body.String())
	}
	if len(resp.FailedKeys) != 1 {
		t.Errorf("FailedKeys len=%d want 1", len(resp.FailedKeys))
	}
}

func TestReembed_NoVectorSupportReturns503(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, false)
	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
}

func TestReembed_NoEmbedderReturns503(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	srv.embedder = nil
	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "embedder_not_configured") {
		t.Errorf("expected embedder_not_configured code: %s", rec.Body.String())
	}
}

func TestReembed_MissingScopeIDReturns400(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestReembed_InvalidScopeReturns400(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=tenant&scope_id=x", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestReembed_EmptyScopeReturnsZeroCounts(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	// No preload — nothing in (user, ghost).
	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=ghost", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp memoryReembedDryRunResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.RowsToReembed != 0 {
		t.Errorf("expected 0 rows in empty scope, got %d", resp.RowsToReembed)
	}
}

// Sanity: SetEmbedder is the wire point for the live instance.
// Verify the same instance survives round-trip via the server.
func TestSetEmbedder_StoresLiveInstance(t *testing.T) {
	srv, emb, _ := vectorAdminFixture(t, true)
	var got providers.Embedder = srv.embedder
	if got != emb {
		t.Errorf("expected same embedder instance")
	}
}
