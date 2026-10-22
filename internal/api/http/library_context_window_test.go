package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// windowProvider is a provider that only reports a context window. The library
// listing never calls a model, so Call is never reached.
type windowProvider struct {
	id     string
	window int
}

func (p *windowProvider) ID() string { return p.id }
func (p *windowProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{MaxContextTokens: p.window}
}
func (p *windowProvider) Probe(context.Context) error { return nil }
func (p *windowProvider) ListModels(context.Context) ([]string, error) {
	return []string{"m"}, nil
}
func (p *windowProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	panic("the library listing must not call a model")
}

func windowsByName(entries []LibraryEntry) map[string]int {
	out := make(map[string]int, len(entries))
	for _, e := range entries {
		out[e.Name] = e.ContextWindow
	}
	return out
}

// TestUnifiedLibrary_Agents_ContextWindowIsTheEffectiveRunWindow pins the
// window the agent list reports: the resolved provider's window, lowered by the
// agent's own max_context_tokens — and never raised by it, since a cloud window
// is fixed. An agent whose provider reports no window, and has no cap of its
// own, carries no context_window at all (not a 0 the UI would draw as empty).
func TestUnifiedLibrary_Agents_ContextWindowIsTheEffectiveRunWindow(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"plain":       {Provider: "big", Model: "m"},
		"capped":      {Provider: "big", Model: "m", MaxContextTokens: 50000},
		"over-capped": {Provider: "big", Model: "m", MaxContextTokens: 999999},
		"unknown":     {Provider: "nowindow", Model: "m"},
		"unknown-cap": {Provider: "nowindow", Model: "m", MaxContextTokens: 8192},
		"no-provider": {Provider: "missing", Model: "m"},
	}, nil)
	defer cleanup()
	srv.providers = &multiResolver{byID: map[string]providers.Provider{
		"big":      &windowProvider{id: "big", window: 200000},
		"nowindow": &windowProvider{id: "nowindow"},
	}}

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	got := windowsByName(decodeLibraryEntries(t, rec))

	want := map[string]int{
		"plain":       200000,
		"capped":      50000,
		"over-capped": 200000,
		"unknown":     0,
		"unknown-cap": 8192,
		"no-provider": 0,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: context_window = %d, want %d", name, got[name], w)
		}
	}
}

// TestUnifiedLibrary_Agents_UnknownContextWindowIsOmitted pins the wire shape
// for an unknown window: the key is absent, so a consumer reading it as
// optional draws nothing rather than "0 tokens".
func TestUnifiedLibrary_Agents_UnknownContextWindowIsOmitted(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"unknown": {Provider: "nowindow", Model: "m"},
	}, nil)
	defer cleanup()
	srv.providers = &multiResolver{byID: map[string]providers.Provider{
		"nowindow": &windowProvider{id: "nowindow"},
	}}

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	var raw struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(raw.Entries))
	}
	if v, ok := raw.Entries[0]["context_window"]; ok {
		t.Errorf("context_window present (%s) for an agent with no known window", v)
	}
}

// TestUnifiedLibrary_Agents_ContextWindowResolvesInTheCallersTenant pins that
// the window is resolved the way a run of the CALLER's tenant would resolve
// the agent: a tenant that overrides a shared agent with its own capped
// definition sees its cap, and another tenant — which cannot see that
// override — sees the shared definition's window, not the first tenant's cap.
func TestUnifiedLibrary_Agents_ContextWindowResolvesInTheCallersTenant(t *testing.T) {
	srv, s, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"shared": {Provider: "big", Model: "m"},
	}, nil)
	defer cleanup()
	srv.providers = &multiResolver{byID: map[string]providers.Provider{
		"big": &windowProvider{id: "big", window: 200000},
	}}
	ctx := t.Context()
	if _, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_acme_shared", Name: "shared", Version: 1, TenantID: "acme",
		Definition: []byte(`{"provider":"big","model":"m","max_context_tokens":1000}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentDefSetActive(ctx, "acme", "shared", "def_acme_shared", ""); err != nil {
		t.Fatal(err)
	}

	windowFor := func(p auth.Principal) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/_library/agents", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		srv.handleListLibraryAgents(rec, req)
		return windowsByName(decodeLibraryEntries(t, rec))["shared"]
	}
	if got := windowFor(auth.Principal{TenantID: "acme", Subject: "acme-op", Scopes: []string{auth.ScopeTenant}}); got != 1000 {
		t.Errorf("acme sees context_window %d, want 1000 (its own capped override)", got)
	}
	if got := windowFor(auth.Principal{TenantID: "globex", Subject: "globex-op", Scopes: []string{auth.ScopeTenant}}); got != 200000 {
		t.Errorf("globex sees context_window %d, want 200000 (the shared definition, not acme's override)", got)
	}
}
