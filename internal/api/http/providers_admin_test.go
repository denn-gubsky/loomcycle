package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// listModelsProvider is a minimal providers.Provider stub for the
// /v1/_providers/{id}/models handler tests. We only need ID +
// ListModels here; Call / Probe / Capabilities are stub-only.
type listModelsProvider struct {
	id     string
	models []string
	err    error
	// delay simulates a slow upstream so the DurationMs field has
	// something non-zero to assert.
	delay time.Duration
}

func (p *listModelsProvider) ID() string                         { return p.id }
func (p *listModelsProvider) Capabilities() providers.Capabilities { return providers.Capabilities{} }
func (p *listModelsProvider) Probe(_ context.Context) error      { return nil }
func (p *listModelsProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	return nil, errors.New("not used in this test")
}
func (p *listModelsProvider) ListModels(ctx context.Context) ([]string, error) {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.models, nil
}

// routingResolver dispatches Get by provider id. Lets one test exercise
// "unknown" vs "configured" vs "not configured" branches in a single
// fixture rather than per-test stubs.
type routingResolver struct {
	known map[string]providers.Provider
	// notConfigured names providers we recognise but the operator
	// didn't wire (mimics the real "X provider not configured" path).
	notConfigured map[string]bool
}

func (r *routingResolver) Get(id string) (providers.Provider, error) {
	if p, ok := r.known[id]; ok {
		return p, nil
	}
	if r.notConfigured[id] {
		return nil, errors.New(id + " provider not configured (set FOO_API_KEY)")
	}
	return nil, errors.New("unknown provider " + id)
}

func makeServerForProvidersAdmin(t *testing.T, pr ProviderResolver) *Server {
	t.Helper()
	hookReg := hooks.NewRegistry()
	return &Server{
		cfg:            &config.Config{},
		providers:      pr,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 8, 1000),
	}
}

// TestProviderModels_ReturnsLiveList pins the happy path: the URL
// names a configured provider, ListModels round-trips successfully,
// and the response carries the provider id, fetched_at timestamp,
// duration, and the model list.
func TestProviderModels_ReturnsLiveList(t *testing.T) {
	prov := &listModelsProvider{
		id:     "anthropic",
		models: []string{"claude-sonnet-4-6", "claude-haiku-4-5", "claude-opus-4-7"},
		delay:  2 * time.Millisecond,
	}
	srv := makeServerForProvidersAdmin(t, &routingResolver{
		known: map[string]providers.Provider{"anthropic": prov},
	})

	req := httptest.NewRequest("GET", "/v1/_providers/anthropic/models", nil)
	req.SetPathValue("id", "anthropic")
	rec := httptest.NewRecorder()
	srv.handleProviderModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var resp providerModelsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if resp.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", resp.Provider)
	}
	if resp.FetchedAt.IsZero() {
		t.Error("fetched_at zero — handler did not stamp it")
	}
	if len(resp.Models) != 3 {
		t.Errorf("Models len = %d, want 3 (%v)", len(resp.Models), resp.Models)
	}
	// DurationMs should reflect the 2ms delay (give a generous
	// tolerance for CI jitter — the contract is "non-trivial when
	// the upstream is slow", not "exact").
	if resp.DurationMs < 1 {
		t.Errorf("DurationMs = %d, want >= 1 given the 2ms upstream delay", resp.DurationMs)
	}
}

// TestProviderModels_EmptyModelsListIsNotNil pins the wire-contract
// invariant from the Provider interface: a reachable provider with
// zero models returns an empty SLICE, not null. Operators reading
// the response can distinguish "reachable, no models" from
// "failed". A 200 with `"models": null` would be ambiguous.
func TestProviderModels_EmptyModelsListIsNotNil(t *testing.T) {
	prov := &listModelsProvider{id: "openai", models: nil}
	srv := makeServerForProvidersAdmin(t, &routingResolver{
		known: map[string]providers.Provider{"openai": prov},
	})

	req := httptest.NewRequest("GET", "/v1/_providers/openai/models", nil)
	req.SetPathValue("id", "openai")
	rec := httptest.NewRecorder()
	srv.handleProviderModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Use the raw body to check the JSON representation — a Go nil
	// slice would marshal to null; we want [].
	body := rec.Body.String()
	if !strings.Contains(body, `"models": []`) && !strings.Contains(body, `"models":[]`) {
		t.Errorf("body should contain `models: []` (not null), got: %s", body)
	}
}

// TestProviderModels_UnknownProvider404 pins the URL-typo path: an
// id that isn't a registered driver returns 404 with the
// provider_unknown code, so callers can distinguish "you misspelled
// the provider" from "the provider is down".
func TestProviderModels_UnknownProvider404(t *testing.T) {
	srv := makeServerForProvidersAdmin(t, &routingResolver{
		known: map[string]providers.Provider{},
	})

	req := httptest.NewRequest("GET", "/v1/_providers/typo/models", nil)
	req.SetPathValue("id", "typo")
	rec := httptest.NewRecorder()
	srv.handleProviderModels(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider_unknown"`) {
		t.Errorf("body should contain code provider_unknown, got: %s", rec.Body.String())
	}
}

// TestProviderModels_NotConfigured503 pins the operator-fix path:
// a known driver that wasn't wired (no API key) returns 503 with
// the provider_unavailable code. Distinct from 404 because the fix
// is "set the env var", not "fix the URL".
func TestProviderModels_NotConfigured503(t *testing.T) {
	srv := makeServerForProvidersAdmin(t, &routingResolver{
		notConfigured: map[string]bool{"deepseek": true},
	})

	req := httptest.NewRequest("GET", "/v1/_providers/deepseek/models", nil)
	req.SetPathValue("id", "deepseek")
	rec := httptest.NewRecorder()
	srv.handleProviderModels(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider_unavailable"`) {
		t.Errorf("body should contain code provider_unavailable, got: %s", rec.Body.String())
	}
}

// TestProviderModels_ListModelsFails502 pins the upstream-failure
// path: the provider is configured but the live ListModels call
// fails (network, auth rejection, 5xx). 502 Bad Gateway is the
// correct mapping — loomcycle reached its dependency successfully
// in process, but the upstream itself errored.
func TestProviderModels_ListModelsFails502(t *testing.T) {
	prov := &listModelsProvider{id: "ollama", err: errors.New("dial tcp: connection refused")}
	srv := makeServerForProvidersAdmin(t, &routingResolver{
		known: map[string]providers.Provider{"ollama": prov},
	})

	req := httptest.NewRequest("GET", "/v1/_providers/ollama/models", nil)
	req.SetPathValue("id", "ollama")
	rec := httptest.NewRecorder()
	srv.handleProviderModels(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider_list_failed"`) {
		t.Errorf("body should contain code provider_list_failed, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("error message should surface the upstream's reason, got: %s", rec.Body.String())
	}
}

// TestProviderModels_EmptyIDPath400 pins the defensive path: a
// caller bypassing the mux (calling the handler directly with no
// path value) gets 400 rather than a runtime panic on Get("").
// In production the mux gates this — the route pattern requires
// {id} — but the handler also defends.
func TestProviderModels_EmptyIDPath400(t *testing.T) {
	srv := makeServerForProvidersAdmin(t, &routingResolver{})

	req := httptest.NewRequest("GET", "/v1/_providers//models", nil)
	// Deliberately do NOT set the path value.
	rec := httptest.NewRecorder()
	srv.handleProviderModels(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

