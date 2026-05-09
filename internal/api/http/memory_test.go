package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
