package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/search"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestWebSearch_PerAgentProvidersOverride: the per-agent search_providers list
// (RFC BB Phase 3, on ctx) overrides the global cascade — a provider absent from
// the list is skipped even when it's the global primary.
func TestWebSearch_PerAgentProvidersOverride(t *testing.T) {
	braveHit, serperHit := 0, 0
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		braveHit++
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"B","url":"https://b.example","description":"d"}]}}`))
	}))
	defer brave.Close()
	serper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serperHit++
		_, _ = w.Write([]byte(`{"organic":[{"title":"S","link":"https://s.example","snippet":"d"}]}`))
	}))
	defer serper.Close()
	reg, _ := search.BuildRegistry([]search.ProviderSpec{
		{ID: "brave", Options: []search.Option{search.WithEndpoint(brave.URL)}},
		{ID: "serper", Options: []search.Option{search.WithEndpoint(serper.URL)}},
	})
	// Global order is [brave, serper] — brave is primary.
	ws := &WebSearch{Registry: reg, Resolver: search.NewResolver([]string{"brave", "serper"}),
		HostKeys: map[string]string{"brave": "bk", "serper": "sk"}}

	// Per-agent list = [serper] only → brave is skipped entirely.
	ctx := tools.WithSearchProviders(context.Background(), []string{"serper"})
	res, err := ws.Execute(ctx, json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "s.example") {
		t.Errorf("expected serper's result (per-agent override), got %q", res.Text)
	}
	if braveHit != 0 {
		t.Errorf("brave (global primary, absent from per-agent list) should not be hit; braveHit=%d", braveHit)
	}
	if serperHit != 1 {
		t.Errorf("serper should be hit once; serperHit=%d", serperHit)
	}
}

// TestWebSearch_FallbackCircuit is the RFC BB headline: the primary provider
// errors, so WebSearch falls over to the next provider in the cascade and
// returns ITS results — and marks the failed provider stalled. Fail-before:
// change Execute's "continue" on a provider error to "return the error" and
// this test fails (it would surface brave's 500 instead of serper's results).
func TestWebSearch_FallbackCircuit(t *testing.T) {
	braveHit, serperHit := 0, 0
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		braveHit++
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer brave.Close()
	serper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serperHit++
		_, _ = w.Write([]byte(`{"organic":[{"title":"S","link":"https://s.example","snippet":"snip"}]}`))
	}))
	defer serper.Close()

	reg, err := search.BuildRegistry([]search.ProviderSpec{
		{ID: "brave", Options: []search.Option{search.WithEndpoint(brave.URL)}},
		{ID: "serper", Options: []search.Option{search.WithEndpoint(serper.URL)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := search.NewResolver([]string{"brave", "serper"})
	ws := &WebSearch{Registry: reg, Resolver: resolver, HostKeys: map[string]string{"brave": "bk", "serper": "sk"}}

	res, err := ws.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("fallback should have produced serper results, got error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "https://s.example") {
		t.Errorf("expected serper's result on the wire, got %q", res.Text)
	}
	if braveHit != 1 || serperHit != 1 {
		t.Errorf("expected brave once (failed) then serper once; braveHit=%d serperHit=%d", braveHit, serperHit)
	}
	if resolver.Available("brave") {
		t.Error("brave should be stalled after its failure")
	}
	if !resolver.Available("serper") {
		t.Error("serper should remain available after success")
	}
}

// TestWebSearch_AllProvidersFail: when every provider errors, the tool surfaces
// the last error (IsError) rather than a silent "no results".
func TestWebSearch_AllProvidersFail(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer fail.Close()
	reg, _ := search.BuildRegistry([]search.ProviderSpec{
		{ID: "brave", Options: []search.Option{search.WithEndpoint(fail.URL)}},
		{ID: "serper", Options: []search.Option{search.WithEndpoint(fail.URL)}},
	})
	ws := &WebSearch{Registry: reg, Resolver: search.NewResolver([]string{"brave", "serper"}), HostKeys: map[string]string{"brave": "k", "serper": "k"}}
	res, _ := ws.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "all providers failed") {
		t.Errorf("expected all-failed error, got %q", res.Text)
	}
}

// TestWebSearch_SkipsUnkeyedProvider: a keyed provider with no key is skipped
// (not a failure), and the cascade lands on the keyless provider that works.
func TestWebSearch_SkipsUnkeyedProvider(t *testing.T) {
	hit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		_, _ = w.Write([]byte(`{"results":[{"title":"SX","url":"https://sx.example","content":"c"}]}`))
	}))
	defer srv.Close()
	reg, _ := search.BuildRegistry([]search.ProviderSpec{
		{ID: "serper", Options: []search.Option{search.WithEndpoint(srv.URL)}}, // no key → skipped
		{ID: "searxng", BaseURL: srv.URL},                                      // keyless → runs
	})
	ws := &WebSearch{Registry: reg, Resolver: search.NewResolver([]string{"serper", "searxng"}), HostKeys: map[string]string{}}
	res, _ := ws.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
	if res.IsError {
		t.Fatalf("expected searxng result, got error %q", res.Text)
	}
	if !strings.Contains(res.Text, "sx.example") {
		t.Errorf("expected searxng result, got %q", res.Text)
	}
	if hit != 1 {
		t.Errorf("only searxng should have been hit (serper skipped, no key); hit=%d", hit)
	}
}
