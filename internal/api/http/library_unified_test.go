package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// libraryUnifiedFixture is a variant of libraryFixture (library_admin_test.go)
// that allows pre-seeding cfg.Agents / cfg.MCPServers so we can exercise the
// static-side merge in the unified endpoints.
func libraryUnifiedFixture(
	t *testing.T,
	staticAgents map[string]config.AgentDef,
	staticMCP map[string]config.MCPServer,
) (*Server, store.Store, func()) {
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
		Agents:     staticAgents,
		MCPServers: staticMCP,
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

func decodeLibraryEntries(t *testing.T, rec *httptest.ResponseRecorder) []LibraryEntry {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp libraryListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Entries
}

// TestUnifiedLibrary_Agents_StaticOnly — yaml-only entry, empty store.
// Expect source=static-only, in_static=true, in_substrate=false,
// version_count=0, static_definition non-nil.
func TestUnifiedLibrary_Agents_StaticOnly(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"qa": {
			Model:        "stub-model",
			SystemPrompt: "you are qa",
			AllowedTools: []string{"Read"},
		},
	}, nil)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	entries := decodeLibraryEntries(t, rec)

	if len(entries) != 1 || entries[0].Name != "qa" {
		t.Fatalf("entries = %+v, want 1 entry 'qa'", entries)
	}
	e := entries[0]
	if e.Source != "static-only" || !e.InStatic || e.InSubstrate {
		t.Errorf("source flags wrong: source=%q in_static=%v in_substrate=%v", e.Source, e.InStatic, e.InSubstrate)
	}
	if e.VersionCount != 0 || e.ActiveDefID != "" || e.LatestVersion != 0 {
		t.Errorf("substrate counters should be zero: %+v", e)
	}
	if len(e.StaticDefinition) == 0 {
		t.Fatalf("static_definition empty")
	}
	var def struct {
		SystemPrompt string   `json:"system_prompt"`
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.Unmarshal(e.StaticDefinition, &def); err != nil {
		t.Fatal(err)
	}
	if def.SystemPrompt != "you are qa" || len(def.AllowedTools) != 1 || def.AllowedTools[0] != "Read" {
		t.Errorf("static_definition payload wrong: %+v", def)
	}
}

// TestUnifiedLibrary_Agents_StaticCodeBody pins that a static yaml code-js
// agent surfaces its inline code_body in static_definition, so the Web UI can
// display + fork it. Fails on the pre-fix staticAgentDefJSON, which omitted
// the field → the UI would show a code agent with no body.
func TestUnifiedLibrary_Agents_StaticCodeBody(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"batch": {
			Provider: "code-js",
			Code:     `function run(input){ return {final_text:"ok"}; }`,
		},
	}, nil)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	entries := decodeLibraryEntries(t, rec)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	var def struct {
		Provider string `json:"provider"`
		CodeBody string `json:"code_body"`
	}
	if err := json.Unmarshal(entries[0].StaticDefinition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Provider != "code-js" || def.CodeBody != `function run(input){ return {final_text:"ok"}; }` {
		t.Errorf("static code agent definition missing code_body: %+v", def)
	}
}

// TestUnifiedLibrary_Agents_DynamicOnly — substrate row with no yaml twin.
// Expect source=dynamic-only, in_substrate=true, no static_definition.
func TestUnifiedLibrary_Agents_DynamicOnly(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, nil, nil)
	defer cleanup()

	ctx := t.Context()
	_, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_evaluator_v1", Name: "evaluator", Version: 1,
		Definition: []byte(`{"system_prompt":"eval"}`), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AgentDefSetActive(ctx, "", "evaluator", "def_evaluator_v1", "")

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	entries := decodeLibraryEntries(t, rec)

	if len(entries) != 1 || entries[0].Name != "evaluator" {
		t.Fatalf("entries = %+v, want 1 entry 'evaluator'", entries)
	}
	e := entries[0]
	if e.Source != "dynamic-only" || e.InStatic || !e.InSubstrate {
		t.Errorf("source flags wrong: %+v", e)
	}
	if e.VersionCount != 1 || e.ActiveDefID != "def_evaluator_v1" || e.LatestVersion != 1 {
		t.Errorf("substrate counters missing: %+v", e)
	}
	if len(e.StaticDefinition) != 0 {
		t.Errorf("static_definition should be absent for dynamic-only: %s", e.StaticDefinition)
	}
}

// TestUnifiedLibrary_Agents_Both — yaml AND substrate hold the name.
// Expect source=both, both flags true, substrate counters populated,
// static_definition populated.
func TestUnifiedLibrary_Agents_Both(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"researcher": {Model: "stub", SystemPrompt: "yaml-side"},
	}, nil)
	defer cleanup()

	ctx := t.Context()
	_, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_r_v1", Name: "researcher", Version: 1,
		Definition: []byte(`{"system_prompt":"substrate-side"}`), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AgentDefSetActive(ctx, "", "researcher", "def_r_v1", "")

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	entries := decodeLibraryEntries(t, rec)

	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1 entry", entries)
	}
	e := entries[0]
	if e.Source != "both" || !e.InStatic || !e.InSubstrate {
		t.Errorf("source flags wrong: %+v", e)
	}
	if e.VersionCount != 1 || e.ActiveDefID != "def_r_v1" {
		t.Errorf("substrate counters missing: %+v", e)
	}
	if len(e.StaticDefinition) == 0 {
		t.Fatalf("static_definition missing for both-source entry")
	}
}

// TestUnifiedLibrary_Agents_SortedAlphabetically — three names, one of each
// source flavor, mixed insertion order. Output sorted by name.
func TestUnifiedLibrary_Agents_SortedAlphabetically(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"zebra": {Model: "x"},
		"alpha": {Model: "x"},
	}, nil)
	defer cleanup()
	ctx := t.Context()
	_, _ = s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_m_v1", Name: "middle", Version: 1,
		Definition: []byte(`{}`), CreatedAt: time.Now(),
	})

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	entries := decodeLibraryEntries(t, rec)

	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, n := range want {
		if entries[i].Name != n {
			t.Errorf("entries[%d].Name = %q, want %q", i, entries[i].Name, n)
		}
	}
}

// TestUnifiedLibrary_MCP_StdioStaticOnly — cfg has a stdio server with
// Command/Args/Env; no substrate row, no pool inspector.
// Expect static_definition.transport="stdio", command/args/env populated,
// discovered_tools omitted (no inspector wired).
func TestUnifiedLibrary_MCP_StdioStaticOnly(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, nil, map[string]config.MCPServer{
		"local-tools": {
			Transport: "stdio",
			Command:   "node",
			Args:      []string{"server.js"},
			Env:       map[string]string{"MCP_VERBOSE": "1"},
			PoolSize:  2,
		},
	})
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/mcp-servers", nil))
	entries := decodeLibraryEntries(t, rec)

	if len(entries) != 1 || entries[0].Name != "local-tools" {
		t.Fatalf("entries = %+v", entries)
	}
	var def struct {
		Transport       string            `json:"transport"`
		Command         string            `json:"command"`
		Args            []string          `json:"args"`
		Env             map[string]string `json:"env"`
		PoolSize        int               `json:"pool_size"`
		DiscoveredTools json.RawMessage   `json:"discovered_tools"`
	}
	if err := json.Unmarshal(entries[0].StaticDefinition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Transport != "stdio" || def.Command != "node" || len(def.Args) != 1 || def.Args[0] != "server.js" {
		t.Errorf("stdio fields wrong: %+v", def)
	}
	if def.Env["MCP_VERBOSE"] != "1" || def.PoolSize != 2 {
		t.Errorf("env/pool_size wrong: %+v", def)
	}
	if len(def.DiscoveredTools) != 0 {
		t.Errorf("discovered_tools should be absent without pool inspector: %s", def.DiscoveredTools)
	}
}

// TestUnifiedLibrary_MCP_HTTPWithPoolInspector — cfg has http server,
// pool inspector returns 2 tools. Expect discovered_tools populated.
func TestUnifiedLibrary_MCP_HTTPWithPoolInspector(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, nil, map[string]config.MCPServer{
		"remote-mcp": {
			Transport: "http",
			URL:       "https://example.invalid/api/mcp",
			Headers:   map[string]string{"Authorization": "Bearer x"},
		},
	})
	defer cleanup()

	srv.SetMCPPoolInspector(func(name string) json.RawMessage {
		if name != "remote-mcp" {
			return nil
		}
		return json.RawMessage(`[{"name":"search","description":"web search","input_schema":{"type":"object"}},{"name":"fetch","input_schema":{"type":"object"}}]`)
	})

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/mcp-servers", nil))
	entries := decodeLibraryEntries(t, rec)

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	var def struct {
		Transport       string `json:"transport"`
		URL             string `json:"url"`
		DiscoveredTools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"discovered_tools"`
	}
	if err := json.Unmarshal(entries[0].StaticDefinition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Transport != "http" || def.URL != "https://example.invalid/api/mcp" {
		t.Errorf("http fields wrong: %+v", def)
	}
	if len(def.DiscoveredTools) != 2 || def.DiscoveredTools[0].Name != "search" || def.DiscoveredTools[1].Name != "fetch" {
		t.Errorf("discovered_tools missing or wrong shape: %+v", def.DiscoveredTools)
	}
}

// TestUnifiedLibrary_OldEndpoints_StillWork — invoking the v1 /names
// endpoint after the new unified endpoint exists must return the
// pre-v0.9.x wire shape byte-for-byte. Backwards-compat guard for the
// TS adapter consumer.
func TestUnifiedLibrary_OldEndpoints_StillWork(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, nil, nil)
	defer cleanup()

	ctx := t.Context()
	_, _ = s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_v1", Name: "x", Version: 1,
		Definition: []byte(`{}`), CreatedAt: time.Now(),
	})
	_ = s.AgentDefSetActive(ctx, "", "x", "def_v1", "")

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_agentdef/names", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("old endpoint broken: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Names []store.AgentDefNameSummary `json:"names"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Names) != 1 || resp.Names[0].Name != "x" {
		t.Errorf("old endpoint shape changed: %+v", resp.Names)
	}
}

// callLibraryAgents invokes the handler directly with an injected principal
// (bypassing authMiddleware, which would re-resolve from the bearer) so each
// case exercises a specific principal's tenant scope.
func callLibraryAgents(t *testing.T, srv *Server, p auth.Principal) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/_library/agents", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	srv.handleListLibraryAgents(rec, req)
	es := decodeLibraryEntries(t, rec)
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}

// sameNameSet reports whether got and want contain the same names (order-
// independent). Library entries are sorted by name, but the tenant tests assert
// on membership, so compare as sets.
func sameNameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[string]struct{}, len(got))
	for _, g := range got {
		set[g] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

// TestUnifiedLibrary_Agents_TenantIsolation (RFC AS Phase 1) pins that the
// library listing is tenant-scoped for SUBSTRATE rows: a substrate:tenant
// principal sees its own tenant's substrate agents PLUS the operator-global
// static cfg agents (the shared catalog floor, shown to every principal like
// the static-volume bind floor) — but NEVER another tenant's substrate rows.
// A substrate:admin principal sees all. Fail-before: the pre-RFC-AS handler
// called the tenant-blind AgentDefListNames, so any principal could enumerate
// every tenant's agent names.
func TestUnifiedLibrary_Agents_TenantIsolation(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"global-static": {Model: "x", SystemPrompt: "operator-global"},
	}, nil)
	defer cleanup()

	ctx := t.Context()
	mustDef := func(defID, name, tenant string) {
		if _, err := s.AgentDefCreate(ctx, store.AgentDefRow{
			DefID: defID, Name: name, Version: 1, TenantID: tenant,
			Definition: []byte(`{}`), CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("AgentDefCreate(%s): %v", name, err)
		}
		if err := s.AgentDefSetActive(ctx, tenant, name, defID, ""); err != nil {
			t.Fatalf("AgentDefSetActive(%s): %v", name, err)
		}
	}
	mustDef("def_acme", "acme-agent", "acme")
	mustDef("def_globex", "globex-agent", "globex")

	// A substrate:tenant principal sees its own tenant's substrate row PLUS the
	// operator-global static — but NOT another tenant's substrate row.
	if got := callLibraryAgents(t, srv, auth.Principal{TenantID: "acme", Subject: "acme-op", Scopes: []string{auth.ScopeTenant}}); !sameNameSet(got, []string{"acme-agent", "global-static"}) {
		t.Fatalf("acme tenant sees %v, want [acme-agent global-static] (own substrate + shared static, no globex)", got)
	}
	if got := callLibraryAgents(t, srv, auth.Principal{TenantID: "globex", Subject: "globex-op", Scopes: []string{auth.ScopeTenant}}); !sameNameSet(got, []string{"globex-agent", "global-static"}) {
		t.Fatalf("globex tenant sees %v, want [globex-agent global-static] (own substrate + shared static, no acme)", got)
	}

	// A substrate:admin principal sees everything: both tenants' substrate rows
	// plus the operator-global static agent.
	got := callLibraryAgents(t, srv, auth.Principal{TenantID: "default", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	if !sameNameSet(got, []string{"acme-agent", "globex-agent", "global-static"}) {
		t.Fatalf("admin sees %v, want all 3 (acme-agent, globex-agent, global-static)", got)
	}
}

// TestUnifiedLibrary_Agents_AdminTenantFocus (RFC AS Phase 1) pins the admin
// ?tenant= focus: a substrate:admin with ?tenant=acme sees acme's substrate
// rows + the shared operator-global static (acting as a view of that tenant,
// which now includes the shared catalog floor) — but NOT another tenant's
// substrate rows.
func TestUnifiedLibrary_Agents_AdminTenantFocus(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"global-static": {Model: "x"},
	}, nil)
	defer cleanup()
	ctx := t.Context()
	for _, d := range []struct{ id, name, tenant string }{
		{"def_acme", "acme-agent", "acme"},
		{"def_globex", "globex-agent", "globex"},
	} {
		if _, err := s.AgentDefCreate(ctx, store.AgentDefRow{
			DefID: d.id, Name: d.name, Version: 1, TenantID: d.tenant,
			Definition: []byte(`{}`), CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		_ = s.AgentDefSetActive(ctx, d.tenant, d.name, d.id, "")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/_library/agents?tenant=acme", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{
		TenantID: "default", Subject: "ops", Scopes: []string{auth.ScopeAdmin},
	}))
	srv.handleListLibraryAgents(rec, req)
	es := decodeLibraryEntries(t, rec)
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Name
	}
	// The focused view is "as tenant acme": acme's substrate row + the shared
	// static, but NOT globex's substrate row.
	if !sameNameSet(names, []string{"acme-agent", "global-static"}) {
		t.Fatalf("admin ?tenant=acme sees %v, want [acme-agent global-static] (focused tenant + shared static, no globex)", names)
	}
}
