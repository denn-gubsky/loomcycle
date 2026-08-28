package llamacpp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

func TestDriver_IDIsLlamacpp(t *testing.T) {
	if got := New("", "", streamhttp.Options{}, nil).ID(); got != "llamacpp" {
		t.Fatalf("ID() = %q, want %q", got, "llamacpp")
	}
}

func TestDriver_DefaultBaseURLIsLocalLlamaServer(t *testing.T) {
	// An empty base URL must pre-bake llama-server's local endpoint (:8080),
	// NOT fall through to the inner openai driver's api.openai.com default.
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
		Model:    "qwen3-8b",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
	if host := <-captured; host != "localhost:8080" {
		t.Fatalf("default request host = %q, want localhost:8080", host)
	}
}

func TestDriver_KeyEnvNameDefault(t *testing.T) {
	if got := New("", "", streamhttp.Options{}, nil).KeyEnvName(); got != "LLAMACPP_API_KEY" {
		t.Fatalf("KeyEnvName() = %q, want LLAMACPP_API_KEY", got)
	}
}

func TestNewFromOptions_AppliesIDKeyAndCaps(t *testing.T) {
	win := 4096
	p, err := newFromOptions(providers.DriverOptions{
		ID:           "llamacpp-local",
		KeyEnvName:   "MY_LLAMA_KEY",
		Capabilities: &providers.CapabilityPatch{MaxContextTokens: &win},
	})
	if err != nil {
		t.Fatalf("newFromOptions: %v", err)
	}
	d := p.(*Driver)
	if d.ID() != "llamacpp-local" {
		t.Errorf("ID() = %q, want llamacpp-local", d.ID())
	}
	if d.KeyEnvName() != "MY_LLAMA_KEY" {
		t.Errorf("KeyEnvName() = %q, want MY_LLAMA_KEY", d.KeyEnvName())
	}
	if got := d.Capabilities().MaxContextTokens; got != 4096 {
		t.Errorf("Capabilities().MaxContextTokens = %d, want 4096 (capsPatch override)", got)
	}
}

// ---- helpers ----

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type closerBody struct{ *strings.Reader }

func (closerBody) Close() error { return nil }

func nopBody(s string) closerBody { return closerBody{Reader: strings.NewReader(s)} }
