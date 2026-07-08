package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// libraryFixture builds a Server with an in-memory sqlite store, no
// pre-existing rows in any substrate. Each test seeds its own data
// then exercises the read-only library endpoints.
func libraryFixture(t *testing.T) (*Server, store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Env: config.Env{
			AuthToken:             "test-token",
			ChannelsMaxValueBytes: 64 * 1024,
			ChannelsLongPollCapMS: 1000,
		},
	}
	hookReg := hooks.NewRegistry()
	srv := &Server{
		cfg:            cfg,
		store:          s,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	return srv, s, func() { _ = s.Close() }
}

// TestLibrary_AgentDefNames_EmptyStore covers the cold-start case —
// freshly-created store, no agent defs, endpoint returns `{names: []}`
// not `{names: null}`. Wire shape matters for the TS adapter consumer.
func TestLibrary_AgentDefNames_EmptyStore(t *testing.T) {
	srv, _, cleanup := libraryFixture(t)
	defer cleanup()

	req := authedRequest("GET", "/v1/_agentdef/names", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Names []store.AgentDefNameSummary `json:"names"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Must be an empty slice, never null — the TS adapter's
	// resp.names.length check would NPE on null.
	if resp.Names == nil {
		t.Errorf("Names is null, want [] for empty store")
	}
	if len(resp.Names) != 0 {
		t.Errorf("Names = %d entries, want 0", len(resp.Names))
	}
}

// TestLibrary_AgentDefNames_AfterSeed verifies the endpoint returns
// every declared name + the active_def_id pointer + version counts
// after a few definitions have been written.
func TestLibrary_AgentDefNames_AfterSeed(t *testing.T) {
	srv, s, cleanup := libraryFixture(t)
	defer cleanup()
	ctx := t.Context()

	// Two agents: researcher with two versions (v1 retired, v2 active);
	// summariser with one version (v1 active).
	r1, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_researcher_v1", Name: "researcher", Version: 1,
		Definition: []byte(`{"system":"hi"}`), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = r1
	r2, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_researcher_v2", Name: "researcher", Version: 2,
		ParentDefID: "def_researcher_v1",
		Definition:  []byte(`{"system":"hi v2"}`), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = r2
	_ = s.AgentDefSetRetired(ctx, "def_researcher_v1", true)
	_ = s.AgentDefSetActive(ctx, "", "researcher", "def_researcher_v2", "")
	sum1, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_summariser_v1", Name: "summariser", Version: 1,
		Definition: []byte(`{"system":"sum"}`), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = sum1
	_ = s.AgentDefSetActive(ctx, "", "summariser", "def_summariser_v1", "")

	req := authedRequest("GET", "/v1/_agentdef/names", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Names []store.AgentDefNameSummary `json:"names"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]store.AgentDefNameSummary{}
	for _, n := range resp.Names {
		byName[n.Name] = n
	}
	r, ok := byName["researcher"]
	if !ok {
		t.Fatalf("researcher not in response: %+v", resp.Names)
	}
	if r.VersionCount != 2 || r.LatestVersion != 2 || r.ActiveDefID != "def_researcher_v2" {
		t.Errorf("researcher summary wrong: %+v", r)
	}
	sm, ok := byName["summariser"]
	if !ok {
		t.Fatalf("summariser not in response: %+v", resp.Names)
	}
	if sm.VersionCount != 1 || sm.ActiveDefID != "def_summariser_v1" {
		t.Errorf("summariser summary wrong: %+v", sm)
	}
}

// TestLibrary_SkillDefNames_AfterSeed mirrors the AgentDef happy path
// for the skill substrate.
func TestLibrary_SkillDefNames_AfterSeed(t *testing.T) {
	srv, s, cleanup := libraryFixture(t)
	defer cleanup()
	ctx := t.Context()

	_, err := s.SkillDefCreate(ctx, store.SkillDefRow{
		DefID: "sdef_voice_v1", Name: "voice-applier", Version: 1,
		Definition: []byte(`{"body":"speak crisply"}`), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SkillDefSetActive(ctx, "", "voice-applier", "sdef_voice_v1", "")

	req := authedRequest("GET", "/v1/_skilldef/names", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Names []store.SkillDefNameSummary `json:"names"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Names) != 1 || resp.Names[0].Name != "voice-applier" {
		t.Errorf("Names = %+v, want one voice-applier entry", resp.Names)
	}
}

// TestLibrary_MCPServerDefNames_AfterSeed mirrors the AgentDef happy
// path for the MCPServerDef substrate.
func TestLibrary_MCPServerDefNames_AfterSeed(t *testing.T) {
	srv, s, cleanup := libraryFixture(t)
	defer cleanup()
	ctx := t.Context()

	_, err := s.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID: "mdef_n8n_v1", Name: "n8n-mailgun", Version: 1,
		Definition: []byte(`{"transport":"streamable-http","url":"https://x/mcp"}`),
		CreatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.MCPServerDefSetActive(ctx, "", "n8n-mailgun", "mdef_n8n_v1", "")

	req := authedRequest("GET", "/v1/_mcpserverdef/names", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Names []store.MCPServerDefNameSummary `json:"names"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Names) != 1 || resp.Names[0].Name != "n8n-mailgun" {
		t.Errorf("Names = %+v, want one n8n-mailgun entry", resp.Names)
	}
}

// TestLibrary_RequiresBearer guards the auth middleware wiring on
// every new route — one assertion across all four endpoints.
func TestLibrary_RequiresBearer(t *testing.T) {
	srv, _, cleanup := libraryFixture(t)
	defer cleanup()

	for _, path := range []string{
		"/v1/_agentdef/names",
		"/v1/_skilldef/names",
		"/v1/_mcpserverdef/names",
		"/v1/agents/alice/channels",
	} {
		req := httptest.NewRequest("GET", path, nil) // no Authorization header
		rec := httptest.NewRecorder()
		srv.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("path %q: status = %d, want 401", path, rec.Code)
		}
	}
}

// TestAgentChannels_HappyPath publishes + acks on two channels under
// scope=agent/scope_id=alice, then verifies the endpoint returns both
// cursor rows (alphabetised by channel) with non-empty cursor strings.
func TestAgentChannels_HappyPath(t *testing.T) {
	srv, s, cleanup := libraryFixture(t)
	defer cleanup()
	ctx := t.Context()

	for _, ch := range []string{"team-updates", "findings"} {
		_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
			Channel: ch, Scope: store.MemoryScopeAgent, ScopeID: "alice",
			Payload: []byte(`{}`),
		}, 0)
		time.Sleep(time.Microsecond)
		_, next, _ := s.ChannelSubscribe(ctx, ch, store.MemoryScopeAgent, "alice", "", 1)
		_ = s.ChannelAck(ctx, ch, store.MemoryScopeAgent, "alice", next)
	}

	req := authedRequest("GET", "/v1/agents/alice/channels", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Channels []store.ChannelCursorEntry `json:"channels"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Channels) != 2 {
		t.Fatalf("Channels = %d, want 2: %+v", len(resp.Channels), resp.Channels)
	}
	// Channel ASC ordering: "findings" before "team-updates".
	if resp.Channels[0].Channel != "findings" || resp.Channels[1].Channel != "team-updates" {
		t.Errorf("ordering wrong: %+v", resp.Channels)
	}
	for _, c := range resp.Channels {
		if c.Cursor == "" {
			t.Errorf("channel %q has empty cursor", c.Channel)
		}
		if c.ScopeID != "alice" || string(c.Scope) != "agent" {
			t.Errorf("channel %q has wrong scope: %+v", c.Channel, c)
		}
	}
}

// TestAgentChannels_SlashGroupedName (RFC BA agent grouping) verifies a
// `/`-grouped agent name reaches the handler through the percent-encoded path
// the Web UI sends (encodeURIComponent("doc/manager") = "doc%2Fmanager"): Go's
// ServeMux keeps %2F within one path segment and PathValue decodes it, and the
// handler now validates with the `/`-aware grammar (was validIdent, which 400'd
// any `/`). Fail-before: on the old validIdent check this returns 400.
func TestAgentChannels_SlashGroupedName(t *testing.T) {
	srv, s, cleanup := libraryFixture(t)
	defer cleanup()
	ctx := t.Context()

	const agentName = "doc/manager"
	_, _, _ = s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "findings", Scope: store.MemoryScopeAgent, ScopeID: agentName,
		Payload: []byte(`{}`),
	}, 0)
	time.Sleep(time.Microsecond)
	_, next, _ := s.ChannelSubscribe(ctx, "findings", store.MemoryScopeAgent, agentName, "", 1)
	_ = s.ChannelAck(ctx, "findings", store.MemoryScopeAgent, agentName, next)

	// The Web UI encodes the name; the `/` becomes %2F in the path.
	req := authedRequest("GET", "/v1/agents/doc%2Fmanager/channels", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s (grouped agent name must reach the handler)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Channels []store.ChannelCursorEntry `json:"channels"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Channels) != 1 || resp.Channels[0].ScopeID != agentName {
		t.Errorf("Channels = %+v, want one row for scope_id %q", resp.Channels, agentName)
	}
}

// TestScopeNames (RFC AS Phase 1) unit-tests the shared tenant filter that
// every /v1/_*def/names handler applies. Covers: all → passthrough (non-nil
// even for a nil input); !all → keep only the caller's tenant; unknown tenant
// → empty.
func TestScopeNames(t *testing.T) {
	type row struct{ tenant string }
	tenantOf := func(r row) string { return r.tenant }
	rows := []row{{"acme"}, {"globex"}, {"acme"}}

	if got := scopeNames(rows, true, "", tenantOf); len(got) != 3 {
		t.Errorf("all: got %d rows, want 3 (passthrough)", len(got))
	}
	if got := scopeNames[row](nil, true, "", tenantOf); got == nil || len(got) != 0 {
		t.Errorf("all+nil: got %v, want non-nil empty", got)
	}
	if got := scopeNames(rows, false, "acme", tenantOf); len(got) != 2 {
		t.Errorf("tenant=acme: got %d rows, want 2", len(got))
	}
	if got := scopeNames(rows, false, "nope", tenantOf); len(got) != 0 {
		t.Errorf("tenant=nope: got %d rows, want 0", len(got))
	}
}

// callAgentDefNames invokes the /v1/_agentdef/names handler directly with an
// injected principal (bypassing authMiddleware, which would re-resolve from the
// bearer), returning the def names in the response.
func callAgentDefNames(t *testing.T, srv *Server, p auth.Principal, query string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/_agentdef/names"+query, nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	srv.handleListAgentDefNames(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Names []store.AgentDefNameSummary `json:"names"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make([]string, len(resp.Names))
	for i, n := range resp.Names {
		out[i] = n.Name
	}
	return out
}

// TestLibraryAdmin_AgentDefNames_TenantIsolation (RFC AS Phase 1) pins the
// wiring of principalTenantScope + scopeNames in a real /names handler: a
// substrate:tenant principal sees only its own tenant; admin sees all; admin
// ?tenant= focuses one. Fail-before: the handler returned the tenant-blind
// AgentDefListNames result, leaking every tenant's names.
func TestLibraryAdmin_AgentDefNames_TenantIsolation(t *testing.T) {
	srv, s, cleanup := libraryFixture(t)
	defer cleanup()
	ctx := t.Context()
	for _, d := range []struct{ id, name, tenant string }{
		{"def_a", "acme-agent", "acme"},
		{"def_g", "globex-agent", "globex"},
	} {
		if _, err := s.AgentDefCreate(ctx, store.AgentDefRow{
			DefID: d.id, Name: d.name, Version: 1, TenantID: d.tenant,
			Definition: []byte(`{}`), CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	tenant := auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	if got := callAgentDefNames(t, srv, tenant, ""); len(got) != 1 || got[0] != "acme-agent" {
		t.Fatalf("acme tenant sees %v, want [acme-agent] only", got)
	}
	admin := auth.Principal{TenantID: "default", Subject: "ops", Scopes: []string{auth.ScopeAdmin}}
	if got := callAgentDefNames(t, srv, admin, ""); len(got) != 2 {
		t.Fatalf("admin sees %v, want both", got)
	}
	if got := callAgentDefNames(t, srv, admin, "?tenant=globex"); len(got) != 1 || got[0] != "globex-agent" {
		t.Fatalf("admin ?tenant=globex sees %v, want [globex-agent] only", got)
	}
}

// TestAgentChannels_InvalidAgentName guards the validIdent check on
// the path-derived agent_name (same pattern as the per-user channel
// routes).
func TestAgentChannels_InvalidAgentName(t *testing.T) {
	srv, _, cleanup := libraryFixture(t)
	defer cleanup()

	req := authedRequest("GET", "/v1/agents/alice@bob/channels", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid_agent_name)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_agent_name") {
		t.Errorf("body should mention invalid_agent_name: %s", rec.Body.String())
	}
}
