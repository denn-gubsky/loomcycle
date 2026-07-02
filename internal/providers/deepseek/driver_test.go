package deepseek

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// fakeStream serves a canned SSE script, mirroring the OpenAI
// driver's test fixture but asserting the DeepSeek-specific bits
// (Authorization header value, /chat/completions path, model name).
func fakeStream(t *testing.T, wantKey string, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer "+wantKey)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("URL path = %q, want suffix /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			fmt.Fprint(w, f)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
}

func TestDriver_IDIsDeepseek(t *testing.T) {
	// The whole point of the wrapper: a distinct ID so the
	// provider resolver dispatches `provider: deepseek` to this
	// driver and per-run cost accounting keys on it correctly.
	d := New("test-key", "", streamhttp.Options{}, nil)
	if got := d.ID(); got != "deepseek" {
		t.Fatalf("ID() = %q, want %q", got, "deepseek")
	}
}

func TestDriver_DefaultBaseURLIsDeepseek(t *testing.T) {
	// New("", "", streamhttp.Options{}, nil) must NOT fall through to api.openai.com.
	// Verify by stubbing the OpenAI default URL — if the wrapper
	// forgot to pre-bake the DeepSeek base, the request would
	// flow to OpenAI. Easiest check: call with an empty base URL
	// and a captured http.Client transport that records the host.
	captured := make(chan string, 1)
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured <- req.URL.Host
		// Return a minimal "stop" SSE so Call() completes
		// cleanly rather than hanging the test.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io_NopBody(
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: [DONE]\n\n",
			),
		}, nil
	})
	d := New("test-key", "", streamhttp.Options{}, &http.Client{Transport: rt})
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "deepseek-chat",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
	host := <-captured
	if host != "api.deepseek.com" {
		t.Fatalf("default request host = %q, want api.deepseek.com", host)
	}
}

func TestDriver_CustomBaseURLOverridesDefault(t *testing.T) {
	// Operators with a self-hosted OpenAI-compatible mirror
	// (e.g. vLLM serving a DeepSeek model) must be able to
	// override the public endpoint via baseURL. Verifies the
	// override threads through to the inner OpenAI driver.
	srv := fakeStream(t, "test-key", []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "deepseek-chat",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var text strings.Builder
	for ev := range ch {
		if ev.Type == providers.EventText {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q, want %q", text.String(), "hello")
	}
}

func TestDriver_CapabilitiesMostlyMatchOpenAI(t *testing.T) {
	// DeepSeek's V3 chat / coder models behave identically to
	// OpenAI Chat Completions for tool use + streaming. The one
	// deliberate divergence is SupportsThinking: DeepSeek's
	// reasoner / v4-pro variants are thinking-class models, even
	// though OpenAI's chat-class models aren't. We surface the
	// union (provider-max) at the Capabilities() level; per-call
	// decisions use IsThinkingModel(name).
	d := New("test-key", "", streamhttp.Options{}, nil)
	caps := d.Capabilities()
	if caps.NativePromptCache {
		t.Errorf("NativePromptCache = true, want false (DeepSeek auto-caches; no caller knob)")
	}
	if !caps.ParallelToolCalls {
		t.Errorf("ParallelToolCalls = false, want true")
	}
	if !caps.Streaming {
		t.Errorf("Streaming = false, want true")
	}
	if !caps.SupportsThinking {
		t.Errorf("SupportsThinking = false, want true (deepseek-v4-pro / deepseek-reasoner are thinking-class)")
	}
	// DeepSeek text models don't accept images. The inner OpenAI driver reports
	// SupportsVision=true (RFC AT); DeepSeek must override to false so the loop
	// gates an image to DeepSeek upstream instead of the inner OpenAI wire
	// builder producing an image_url DeepSeek 400s on.
	if caps.SupportsVision {
		t.Errorf("SupportsVision = true, want false (DeepSeek text models reject image input)")
	}
}

// TestIsThinkingModel covers the per-model affordance the driver
// uses internally for thinking-class decisions. Naming convention:
//
//	thinking-class: the v4 family (incl. -flash), *-pro, deepseek-reasoner, -r1
//	non-thinking:   deepseek-chat, deepseek-v3.2, deepseek-coder
func TestIsThinkingModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek-v4-pro", true},
		{"deepseek-v3-pro", true},
		{"deepseek-reasoner", true},
		{"deepseek-r1", true},
		{"deepseek-r1-distill", true},
		{"deepseek-chat", false},
		{"deepseek-v4-flash", true}, // v4 family IS thinking-mode (prod 2026-07-02)
		{"deepseek-v3.2", false},
		{"deepseek-coder", false},
		{"DeepSeek-V4-Pro", true},   // case-insensitive
		{"DeepSeek-V4-Flash", true}, // case-insensitive v4
		{"", false},
		{"unknown-model", false},
	}
	for _, tc := range cases {
		got := IsThinkingModel(tc.model)
		if got != tc.want {
			t.Errorf("IsThinkingModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestNonThinkingSibling pins the exp7 R2 downgrade mapping: EVERY thinking-class
// model maps to deepseek-chat (the -flash sibling is itself thinking-mode, so it
// is never a safe target); a non-thinking model yields ("", false).
func TestNonThinkingSibling(t *testing.T) {
	d := &Driver{}
	cases := []struct {
		model         string
		wantSibling   string
		wantDowngrade bool
	}{
		// ALL thinking models downgrade to deepseek-chat — the same-generation
		// -flash is NOT safe (v4-flash is itself thinking-mode; prod 2026-07-02).
		{"deepseek-v4-pro", "deepseek-chat", true},
		{"deepseek-v4-flash", "deepseek-chat", true},
		{"deepseek-v3-pro", "deepseek-chat", true},
		{"deepseek-reasoner", "deepseek-chat", true},
		{"deepseek-r1", "deepseek-chat", true},
		{"deepseek-r1-distill", "deepseek-chat", true},
		// Non-thinking models need no downgrade (incl. deepseek-chat itself →
		// no infinite downgrade).
		{"deepseek-chat", "", false},
		{"deepseek-v3.2", "", false},
		{"", "", false},
		{"unknown-model", "", false},
	}
	for _, tc := range cases {
		sib, dg := d.NonThinkingSibling(tc.model)
		if dg != tc.wantDowngrade || sib != tc.wantSibling {
			t.Errorf("NonThinkingSibling(%q) = (%q, %v), want (%q, %v)",
				tc.model, sib, dg, tc.wantSibling, tc.wantDowngrade)
		}
	}
}

// ---- helpers ----

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// io_NopBody returns an http.Response.Body equivalent to
// io.NopCloser(strings.NewReader(s)). Local helper so the test file
// doesn't need an io / strings import dance just to build a stub
// response.
func io_NopBody(s string) closerBody {
	return closerBody{Reader: strings.NewReader(s)}
}

type closerBody struct {
	*strings.Reader
}

func (closerBody) Close() error { return nil }
