package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
