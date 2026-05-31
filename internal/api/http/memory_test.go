package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// memoryAdminFixture wires just enough of Server to exercise the
// /v1/_memory/* handlers with an in-memory SQLite backend pre-loaded
// with rows under both scopes.
func memoryAdminFixture(t *testing.T) *Server {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := t.Context()
	for _, k := range []string{"voice", "tone"} {
		if err := st.MemorySet(ctx, store.MemoryScopeUser, "alice", k, []byte(`"`+k+`-value"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MemorySet(ctx, store.MemoryScopeUser, "bob", "voice", []byte(`"bob-voice"`), 0); err != nil {
		t.Fatal(err)
	}
	if err := st.MemorySet(ctx, store.MemoryScopeAgent, "qa-agent", "warnings", []byte(`5`), 0); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	hookReg := hooks.NewRegistry()
	return &Server{
		cfg:            cfg,
		store:          st,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
}

func TestHandleListMemoryScopes_ReturnsConstantSet(t *testing.T) {
	s := memoryAdminFixture(t)
	rec := httptest.NewRecorder()
	s.handleListMemoryScopes(rec, httptest.NewRequest("GET", "/v1/_memory/scopes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp memoryScopesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Scopes) != 2 {
		t.Errorf("scopes len = %d, want 2", len(resp.Scopes))
	}
	got := map[string]bool{}
	for _, sc := range resp.Scopes {
		got[sc.Name] = true
		if sc.Description == "" {
			t.Errorf("scope %q missing description", sc.Name)
		}
	}
	if !got["agent"] || !got["user"] {
		t.Errorf("missing scopes in response: %v", resp.Scopes)
	}
}

func TestHandleListMemoryScopeIDs_FiltersByScope(t *testing.T) {
	s := memoryAdminFixture(t)
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/user", nil)
	req.SetPathValue("scope", "user")
	rec := httptest.NewRecorder()
	s.handleListMemoryScopeIDs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp memoryScopeIDsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scope != "user" {
		t.Errorf("scope = %q", resp.Scope)
	}
	got := map[string]int{}
	for _, sc := range resp.ScopeIDs {
		got[sc.ScopeID] = sc.KeyCount
	}
	if got["alice"] != 2 || got["bob"] != 1 {
		t.Errorf("scope_ids: %v, want alice=2 bob=1", got)
	}
	if _, ok := got["qa-agent"]; ok {
		t.Errorf("agent-scope row leaked into user listing: %v", got)
	}
}

func TestHandleListMemoryScopeIDs_RejectsUnknownScope(t *testing.T) {
	s := memoryAdminFixture(t)
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/tenant", nil)
	req.SetPathValue("scope", "tenant")
	rec := httptest.NewRecorder()
	s.handleListMemoryScopeIDs(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleListMemoryEntries_ReturnsKeysAndValues(t *testing.T) {
	s := memoryAdminFixture(t)
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/user/alice/keys", nil)
	req.SetPathValue("scope", "user")
	req.SetPathValue("scope_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListMemoryEntries(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp memoryEntriesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(resp.Entries))
	}
	if resp.Truncated {
		t.Errorf("unexpected truncation")
	}
	keys := map[string]string{}
	for _, e := range resp.Entries {
		keys[e.Key] = string(e.Value)
	}
	if keys["voice"] != `"voice-value"` || keys["tone"] != `"tone-value"` {
		t.Errorf("entries: %v", keys)
	}
}

// RFC I MR-6: include_embedding_metadata is opt-in and best-effort. On a
// store without vector support (the in-memory sqlite fixture), the param is
// honoured by OMITTING the metadata map rather than erroring — the entries
// list is unaffected. (The populated-map path needs a vector-enabled build
// and is exercised by the inprocess backend's vector tests.)
func TestHandleListMemoryEntries_EmbeddingMetadataOptInGracefulWithoutVectors(t *testing.T) {
	s := memoryAdminFixture(t)

	// Default: no metadata requested, no metadata returned.
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/user/alice/keys", nil)
	req.SetPathValue("scope", "user")
	req.SetPathValue("scope_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListMemoryEntries(rec, req)
	var base memoryEntriesResponse
	if err := json.NewDecoder(rec.Body).Decode(&base); err != nil {
		t.Fatal(err)
	}
	if base.EmbeddingMetadata != nil {
		t.Errorf("metadata returned without opt-in: %v", base.EmbeddingMetadata)
	}

	// Opt-in on a non-vector store: still 200, entries intact, metadata
	// omitted (SupportsVectors()==false short-circuits the enrichment).
	req2 := httptest.NewRequest("GET", "/v1/_memory/scopes/user/alice/keys?include_embedding_metadata=true", nil)
	req2.SetPathValue("scope", "user")
	req2.SetPathValue("scope_id", "alice")
	rec2 := httptest.NewRecorder()
	s.handleListMemoryEntries(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	var opt memoryEntriesResponse
	if err := json.NewDecoder(rec2.Body).Decode(&opt); err != nil {
		t.Fatal(err)
	}
	if len(opt.Entries) != 2 {
		t.Errorf("opt-in changed the entries list: got %d, want 2", len(opt.Entries))
	}
	if len(opt.EmbeddingMetadata) != 0 {
		t.Errorf("non-vector store returned metadata: %v", opt.EmbeddingMetadata)
	}
}

func TestHandleListMemoryEntries_PrefixFilter(t *testing.T) {
	s := memoryAdminFixture(t)
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/user/alice/keys?prefix=voi", nil)
	req.SetPathValue("scope", "user")
	req.SetPathValue("scope_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListMemoryEntries(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp memoryEntriesResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Entries) != 1 || resp.Entries[0].Key != "voice" {
		t.Errorf("prefix filter returned %v, want [voice]", resp.Entries)
	}
}

func TestHandleGetMemoryEntry_RoundTrip(t *testing.T) {
	s := memoryAdminFixture(t)
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/agent/qa-agent/keys/warnings", nil)
	req.SetPathValue("scope", "agent")
	req.SetPathValue("scope_id", "qa-agent")
	req.SetPathValue("key", "warnings")
	rec := httptest.NewRecorder()
	s.handleGetMemoryEntry(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp memoryEntryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Entry.Value) != `5` {
		t.Errorf("value = %s, want 5", resp.Entry.Value)
	}
	if resp.Scope != "agent" || resp.ScopeID != "qa-agent" {
		t.Errorf("response (scope, scope_id) = (%q, %q)", resp.Scope, resp.ScopeID)
	}
}

// Memory keys often contain `/` (e.g. `events/2026-05-09`). The mux
// pattern uses {key...} so the multi-segment path resolves correctly;
// a plain {key} would 404 these silently. Exercise the route through
// the full Mux() so the pattern matching is part of the test.
func TestHandleGetMemoryEntry_MultiSegmentKey(t *testing.T) {
	s := memoryAdminFixture(t)
	if err := s.store.MemorySet(t.Context(), store.MemoryScopeAgent, "qa-agent",
		"events/2026-05-09T10:00", []byte(`"first event"`), 0); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(
		"GET", "/v1/_memory/scopes/agent/qa-agent/keys/events/2026-05-09T10:00", nil,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp memoryEntryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Entry.Value) != `"first event"` {
		t.Errorf("value = %s", resp.Entry.Value)
	}
}

func TestHandleGetMemoryEntry_NotFound(t *testing.T) {
	s := memoryAdminFixture(t)
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/agent/qa-agent/keys/missing", nil)
	req.SetPathValue("scope", "agent")
	req.SetPathValue("scope_id", "qa-agent")
	req.SetPathValue("key", "missing")
	rec := httptest.NewRecorder()
	s.handleGetMemoryEntry(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---- v0.11.5 PUT / DELETE memory entry admin tests ---------------

// TestHandlePutMemoryEntry_CreatesAndOverwrites checks the idempotent
// upsert path — PUT a value, then PUT a new value at the same key
// and verify the row was overwritten (not duplicated).
func TestHandlePutMemoryEntry_CreatesAndOverwrites(t *testing.T) {
	s := memoryAdminFixture(t)

	body1 := `{"value": {"role": "manager"}}`
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(
		"PUT", "/v1/_memory/scopes/user/charlie/keys/profile",
		strings.NewReader(body1),
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Read back via the store directly (the admin GET is exercised
	// in TestHandleGetMemoryEntry_*).
	got, err := s.store.MemoryGet(t.Context(), store.MemoryScopeUser, "charlie", "profile")
	if err != nil {
		t.Fatalf("MemoryGet after PUT: %v", err)
	}
	if string(got.Value) != `{"role": "manager"}` {
		t.Errorf("value = %s", got.Value)
	}

	// Overwrite.
	body2 := `{"value": {"role": "director"}}`
	rec2 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec2, httptest.NewRequest(
		"PUT", "/v1/_memory/scopes/user/charlie/keys/profile",
		strings.NewReader(body2),
	))
	if rec2.Code != http.StatusOK {
		t.Fatalf("overwrite status = %d, want 200", rec2.Code)
	}
	got2, _ := s.store.MemoryGet(t.Context(), store.MemoryScopeUser, "charlie", "profile")
	if string(got2.Value) != `{"role": "director"}` {
		t.Errorf("overwrite value = %s", got2.Value)
	}
}

// TestHandlePutMemoryEntry_RejectsInvalidScope refuses scopes outside
// the admin allow-set.
func TestHandlePutMemoryEntry_RejectsInvalidScope(t *testing.T) {
	s := memoryAdminFixture(t)
	body := `{"value": "x"}`
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(
		"PUT", "/v1/_memory/scopes/system/x/keys/y",
		strings.NewReader(body),
	))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandlePutMemoryEntry_RejectsMissingValue refuses an empty body.
func TestHandlePutMemoryEntry_RejectsMissingValue(t *testing.T) {
	s := memoryAdminFixture(t)
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(
		"PUT", "/v1/_memory/scopes/user/alice/keys/voice",
		strings.NewReader(`{}`),
	))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleDeleteMemoryEntry_RemovesRow deletes an existing row + 204s.
func TestHandleDeleteMemoryEntry_RemovesRow(t *testing.T) {
	s := memoryAdminFixture(t)
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(
		"DELETE", "/v1/_memory/scopes/user/alice/keys/voice", nil,
	))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := s.store.MemoryGet(t.Context(), store.MemoryScopeUser, "alice", "voice"); err == nil {
		t.Errorf("expected NotFound after DELETE, got nil error")
	}
}

// TestHandleDeleteMemoryEntry_IdempotentMissingRow returns 204 even
// when the row didn't exist (mirrors the store's MemoryDelete shape:
// "row didn't exist" is not an error).
func TestHandleDeleteMemoryEntry_IdempotentMissingRow(t *testing.T) {
	s := memoryAdminFixture(t)
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(
		"DELETE", "/v1/_memory/scopes/user/alice/keys/nonexistent", nil,
	))
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (idempotent delete)", rec.Code)
	}
}

// memoryAdminAuthedFixture mirrors memoryAdminFixture but configures
// a bearer token + the seed rows. Used for the auth-gate tests below
// (the default fixture leaves AuthToken empty, which puts the mux in
// open mode — useless for verifying the auth middleware).
func memoryAdminAuthedFixture(t *testing.T) *Server {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.MemorySet(t.Context(), store.MemoryScopeUser, "alice", "voice",
		[]byte(`"alice-voice"`), 0); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Env: config.Env{AuthToken: "test-token"}}
	hookReg := hooks.NewRegistry()
	return &Server{
		cfg:            cfg,
		store:          st,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
}

// TestHandlePutMemoryEntry_RequiresBearer confirms the auth middleware
// gates the new mutation route — without it, anyone could overwrite
// any memory row.
func TestHandlePutMemoryEntry_RequiresBearer(t *testing.T) {
	s := memoryAdminAuthedFixture(t)
	req := httptest.NewRequest("PUT", "/v1/_memory/scopes/user/alice/keys/voice",
		strings.NewReader(`{"value":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	// no Authorization header
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestHandleDeleteMemoryEntry_RequiresBearer mirrors the PUT test for
// the DELETE route.
func TestHandleDeleteMemoryEntry_RequiresBearer(t *testing.T) {
	s := memoryAdminAuthedFixture(t)
	req := httptest.NewRequest("DELETE", "/v1/_memory/scopes/user/alice/keys/voice", nil)
	// no Authorization header
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleListMemoryEntries_StoreUnavailable(t *testing.T) {
	cfg := &config.Config{}
	hookReg := hooks.NewRegistry()
	s := &Server{
		cfg:            cfg,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	req := httptest.NewRequest("GET", "/v1/_memory/scopes/user/alice/keys", nil)
	req.SetPathValue("scope", "user")
	req.SetPathValue("scope_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListMemoryEntries(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
