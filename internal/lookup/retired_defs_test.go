package lookup_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Retiring a definition's active version only flips its retired flag; the
// active pointer stays (fork forks from it). These pin that each resolver
// treats such a row as absent — the name then resolves from the next tier, as
// a retired agent, skill or webhook override does — and that un-retiring
// restores it. Stores are keyed "tenant/name" so each tier is distinguishable.

type tenantMemoryBackendStore map[string]store.MemoryBackendDefRow

func (s tenantMemoryBackendStore) MemoryBackendDefGetActive(_ context.Context, tenantID, name string) (store.MemoryBackendDefRow, error) {
	if row, ok := s[tenantID+"/"+name]; ok {
		return row, nil
	}
	return store.MemoryBackendDefRow{}, &store.ErrNotFound{Kind: "memory_backend_def_active", ID: name}
}

type tenantA2AServerCardStore map[string]store.A2AServerCardDefRow

func (s tenantA2AServerCardStore) A2AServerCardDefGetActive(_ context.Context, tenantID, name string) (store.A2AServerCardDefRow, error) {
	if row, ok := s[tenantID+"/"+name]; ok {
		return row, nil
	}
	return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def_active", ID: name}
}

type tenantA2AAgentStore map[string]store.A2AAgentDefRow

func (s tenantA2AAgentStore) A2AAgentDefGetActive(_ context.Context, tenantID, name string) (store.A2AAgentDefRow, error) {
	if row, ok := s[tenantID+"/"+name]; ok {
		return row, nil
	}
	return store.A2AAgentDefRow{}, &store.ErrNotFound{Kind: "a2a_agent_def_active", ID: name}
}

func TestMemoryBackend_RetiredActiveVersionDoesNotResolve(t *testing.T) {
	ss := tenantMemoryBackendStore{}
	setRetired := func(retired bool) {
		row := store.MemoryBackendDefRow{Retired: retired, Definition: json.RawMessage(`{"kind":"remote","config":{"base_url":"https://mb.example"}}`)}
		ss["acme/mb"], ss["/base"] = row, row
	}
	resolves := func(tenant, name string) bool {
		_, _, ok := lookup.MemoryBackend(context.Background(), ss, &config.Config{}, tenant, name)
		return ok
	}

	setRetired(false)
	if !resolves("acme", "mb") || !resolves("", "base") {
		t.Fatal("precondition: a live active version must resolve")
	}
	setRetired(true)
	if resolves("acme", "mb") {
		t.Error("tenant memory backend whose active version is retired still resolves")
	}
	if resolves("", "base") {
		t.Error("shared memory backend whose active version is retired still resolves")
	}
	setRetired(false)
	if !resolves("acme", "mb") || !resolves("", "base") {
		t.Error("un-retired memory backend does not resolve again")
	}
}

func TestMemoryBackend_RetiredTenantOverrideFallsThroughToNextTier(t *testing.T) {
	def := func(url string, retired bool) store.MemoryBackendDefRow {
		return store.MemoryBackendDefRow{Retired: retired, Definition: json.RawMessage(`{"kind":"remote","config":{"base_url":"` + url + `"}}`)}
	}
	ss := tenantMemoryBackendStore{
		"acme/yaml":   def("https://acme.example", true),
		"acme/shared": def("https://acme.example", true),
		"/shared":     def("https://shared.example", false),
	}
	cfg := &config.Config{MemoryBackends: map[string]config.MemoryBackend{
		"yaml": {Kind: "remote", Config: config.MemoryBackendConfig{BaseURL: "https://yaml.example"}},
	}}
	for name, want := range map[string]string{"yaml": "https://yaml.example", "shared": "https://shared.example"} {
		got, _, ok := lookup.MemoryBackend(context.Background(), ss, cfg, "acme", name)
		if !ok || got.Config.BaseURL != want {
			t.Errorf("acme/%s resolved (%q, %v), want %q", name, got.Config.BaseURL, ok, want)
		}
	}
}

func TestA2AServerCard_RetiredActiveVersionDoesNotResolve(t *testing.T) {
	ss := tenantA2AServerCardStore{}
	setRetired := func(retired bool) {
		row := store.A2AServerCardDefRow{Retired: retired, Definition: json.RawMessage(`{"name":"card","description":"d"}`)}
		ss["acme/card"], ss["/base"] = row, row
	}
	resolves := func(tenant, name string) bool {
		_, ok := lookup.A2AServerCard(context.Background(), ss, &config.Config{}, tenant, name)
		return ok
	}

	setRetired(false)
	if !resolves("acme", "card") || !resolves("", "base") {
		t.Fatal("precondition: a live active version must resolve")
	}
	setRetired(true)
	if resolves("acme", "card") {
		t.Error("tenant server card whose active version is retired still resolves")
	}
	if resolves("", "base") {
		t.Error("shared server card whose active version is retired still resolves")
	}
	setRetired(false)
	if !resolves("acme", "card") || !resolves("", "base") {
		t.Error("un-retired server card does not resolve again")
	}
}

func TestA2AServerCard_RetiredTenantOverrideFallsThroughToNextTier(t *testing.T) {
	def := func(desc string, retired bool) store.A2AServerCardDefRow {
		return store.A2AServerCardDefRow{Retired: retired, Definition: json.RawMessage(`{"description":"` + desc + `"}`)}
	}
	ss := tenantA2AServerCardStore{
		"acme/yaml":   def("acme", true),
		"acme/shared": def("acme", true),
		"/shared":     def("shared", false),
	}
	cfg := &config.Config{A2AServerCards: map[string]config.A2AServerCard{"yaml": {Description: "yaml"}}}
	for name, want := range map[string]string{"yaml": "yaml", "shared": "shared"} {
		got, ok := lookup.A2AServerCard(context.Background(), ss, cfg, "acme", name)
		if !ok || got.Description != want {
			t.Errorf("acme/%s resolved (%q, %v), want %q", name, got.Description, ok, want)
		}
	}
}

func TestA2AAgent_RetiredActiveVersionDoesNotResolve(t *testing.T) {
	ss := tenantA2AAgentStore{}
	setRetired := func(retired bool) {
		row := store.A2AAgentDefRow{Retired: retired, Definition: json.RawMessage(`{"endpoint":"https://peer.example/a2a"}`)}
		ss["acme/peer"], ss["/base"] = row, row
	}
	resolves := func(tenant, name string) bool {
		_, ok := lookup.A2AAgent(context.Background(), ss, &config.Config{}, tenant, name)
		return ok
	}

	setRetired(false)
	if !resolves("acme", "peer") || !resolves("", "base") {
		t.Fatal("precondition: a live active version must resolve")
	}
	setRetired(true)
	if resolves("acme", "peer") {
		t.Error("tenant A2A peer whose active version is retired still resolves (still dialable)")
	}
	if resolves("", "base") {
		t.Error("shared A2A peer whose active version is retired still resolves (still dialable)")
	}
	setRetired(false)
	if !resolves("acme", "peer") || !resolves("", "base") {
		t.Error("un-retired A2A peer does not resolve again")
	}
}

func TestA2AAgent_RetiredTenantOverrideFallsThroughToNextTier(t *testing.T) {
	def := func(endpoint string, retired bool) store.A2AAgentDefRow {
		return store.A2AAgentDefRow{Retired: retired, Definition: json.RawMessage(`{"endpoint":"` + endpoint + `"}`)}
	}
	ss := tenantA2AAgentStore{
		"acme/yaml":   def("https://acme.example", true),
		"acme/shared": def("https://acme.example", true),
		"/shared":     def("https://shared.example", false),
	}
	cfg := &config.Config{A2AAgents: map[string]config.A2AAgent{"yaml": {Endpoint: "https://yaml.example"}}}
	for name, want := range map[string]string{"yaml": "https://yaml.example", "shared": "https://shared.example"} {
		got, ok := lookup.A2AAgent(context.Background(), ss, cfg, "acme", name)
		if !ok || got.Endpoint != want {
			t.Errorf("acme/%s resolved (%q, %v), want %q", name, got.Endpoint, ok, want)
		}
	}
}
