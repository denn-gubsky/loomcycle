package vllm

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

func TestDriver_IDIsVllm(t *testing.T) {
	// The point of the wrapper: a distinct ID so the resolver dispatches
	// `provider: vllm` here and per-run cost accounting keys on it.
	if got := New("", "", streamhttp.Options{}, nil).ID(); got != "vllm" {
		t.Fatalf("ID() = %q, want %q", got, "vllm")
	}
}

func TestDriver_DefaultBaseURLIsLocalVllm(t *testing.T) {
	// An empty base URL must pre-bake vLLM's local endpoint, NOT fall
	// through to the inner openai driver's api.openai.com default.
	captured := make(chan string, 1)
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured <- req.URL.Host
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       nopBody("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"),
		}, nil
	})
	d := New("", "", streamhttp.Options{}, &http.Client{Transport: rt})
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "qwen3",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
	if host := <-captured; host != "localhost:8000" {
		t.Fatalf("default request host = %q, want localhost:8000", host)
	}
}

func TestDriver_KeyEnvNameDefault(t *testing.T) {
	// A tenant/user keys its own vLLM by VLLM_API_KEY, not OPENAI_API_KEY.
	if got := New("", "", streamhttp.Options{}, nil).KeyEnvName(); got != "VLLM_API_KEY" {
		t.Fatalf("KeyEnvName() = %q, want VLLM_API_KEY", got)
	}
}

func TestNewFromOptions_AppliesIDKeyAndCaps(t *testing.T) {
	// The registry factory forwards the config-declared id, api_key_env, and
	// capabilities override (e.g. the served model's max_context_tokens).
	win := 8192
	p, err := newFromOptions(providers.DriverOptions{
		ID:           "vllm-local",
		BaseURL:      "http://box:8000/v1",
		KeyEnvName:   "MY_VLLM_KEY",
		Capabilities: &providers.CapabilityPatch{MaxContextTokens: &win},
	})
	if err != nil {
		t.Fatalf("newFromOptions: %v", err)
	}
	d := p.(*Driver)
	if d.ID() != "vllm-local" {
		t.Errorf("ID() = %q, want vllm-local", d.ID())
	}
	if d.KeyEnvName() != "MY_VLLM_KEY" {
		t.Errorf("KeyEnvName() = %q, want MY_VLLM_KEY", d.KeyEnvName())
	}
	if got := d.Capabilities().MaxContextTokens; got != 8192 {
		t.Errorf("Capabilities().MaxContextTokens = %d, want 8192 (capsPatch override)", got)
	}
}

// ---- helpers ----

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type closerBody struct{ *strings.Reader }

func (closerBody) Close() error { return nil }

func nopBody(s string) closerBody { return closerBody{Reader: strings.NewReader(s)} }
