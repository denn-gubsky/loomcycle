package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		// Hybrid: thinks only with an effort hint (live 2026-10-02), so not
		// thinking-class — thinkingMode adds the effort half.
		{"deepseek-flash", false},
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

// TestNonThinkingSibling pins the exp7 R2 downgrade mapping: EVERY call that
// would run in thinking mode maps to deepseek-chat (the -flash sibling of the v4
// line was itself thinking-mode, so it is never a safe target); a call that
// would not think yields ("", false). The hybrid deepseek-flash thinks only with
// an effort hint, so the effort decides whether it is downgraded.
func TestNonThinkingSibling(t *testing.T) {
	d := &Driver{}
	cases := []struct {
		model, effort string
		wantSibling   string
		wantDowngrade bool
	}{
		// ALL thinking models downgrade to deepseek-chat — the same-generation
		// -flash is NOT safe (v4-flash is itself thinking-mode; prod 2026-07-02).
		{"deepseek-v4-pro", "", "deepseek-chat", true},
		{"deepseek-v4-flash", "", "deepseek-chat", true},
		{"deepseek-v3-pro", "", "deepseek-chat", true},
		{"deepseek-reasoner", "", "deepseek-chat", true},
		{"deepseek-r1", "", "deepseek-chat", true},
		{"deepseek-r1-distill", "", "deepseek-chat", true},
		// The hybrid: an effort hint turns thinking on, so it must downgrade;
		// without one it already answers without thinking.
		{"deepseek-flash", "high", "deepseek-chat", true},
		{"deepseek-flash", "", "", false},
		// Non-thinking models need no downgrade without an effort hint (incl.
		// deepseek-chat itself).
		{"deepseek-chat", "", "", false},
		{"deepseek-v3.2", "", "", false},
		{"", "", "", false},
		{"unknown-model", "", "", false},
	}
	for _, tc := range cases {
		sib, dg := d.NonThinkingSibling(tc.model, tc.effort)
		if dg != tc.wantDowngrade || sib != tc.wantSibling {
			t.Errorf("NonThinkingSibling(%q, effort %q) = (%q, %v), want (%q, %v)",
				tc.model, tc.effort, sib, dg, tc.wantSibling, tc.wantDowngrade)
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

// sentToolChoice runs one Call against a server that records the request body,
// and returns the tool_choice DeepSeek would have received ("" when omitted).
func sentToolChoice(t *testing.T, model, effort string, tc providers.ToolChoice) string {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:      model,
		Effort:     effort,
		Messages:   []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:      []providers.ToolSpec{{Name: "emit_state", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: tc,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
	var sent struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	return string(sent.ToolChoice)
}

// DeepSeek 400s a required or named tool_choice in thinking mode ("Thinking
// mode does not support this tool_choice"), before the model runs. The stateful
// loop forces its state tool on every step, so chat/medium on deepseek-v4-flash
// failed at step 0. In thinking mode the forced choice must not be sent.
func TestDriver_ThinkingModeNeverSendsAForcedToolChoice(t *testing.T) {
	named := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	required := providers.ToolChoice{Mode: providers.ToolChoiceRequired}
	for _, c := range []struct {
		name, model, effort string
		tc                  providers.ToolChoice
	}{
		{"v4 model, named", "deepseek-v4-flash", "", named},
		{"v4 model, required", "deepseek-v4-flash", "", required},
		{"reasoner, named", "deepseek-reasoner", "", named},
		{"chat model with an effort hint, named", "deepseek-chat", "high", named},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sentToolChoice(t, c.model, c.effort, c.tc); got != "" {
				t.Errorf("tool_choice sent in thinking mode: %s", got)
			}
		})
	}
}

// Outside thinking mode the choice is still enforced on the wire, and "none" is
// accepted in every mode, so neither is dropped.
func TestDriver_ToolChoiceIsKeptWhereDeepSeekAcceptsIt(t *testing.T) {
	named := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	if got := sentToolChoice(t, "deepseek-chat", "", named); !strings.Contains(got, `"emit_state"`) {
		t.Errorf("deepseek-chat without effort: tool_choice = %q, want the named tool", got)
	}
	if got := sentToolChoice(t, "deepseek-v4-flash", "", providers.ToolChoice{Mode: providers.ToolChoiceNone}); got != `"none"` {
		t.Errorf("v4 model, none: tool_choice = %q, want \"none\"", got)
	}
}

// The loop learns from EnforcesToolChoice that the choice is only asked for, so
// the run reports it instead of claiming a guarantee it does not have.
func TestDriver_EnforcesToolChoiceFollowsThinkingMode(t *testing.T) {
	d := New("k", "", streamhttp.Options{}, nil)
	named := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	if providers.EnforcesToolChoice(d, "deepseek-v4-flash", "", named) {
		t.Error("v4 model: a forced choice is dropped, so it must not report enforced")
	}
	if !providers.EnforcesToolChoice(d, "deepseek-chat", "", named) {
		t.Error("deepseek-chat without effort: the choice is sent, so it is enforced")
	}
	if !providers.EnforcesToolChoice(d, "deepseek-v4-flash", "", providers.ToolChoice{Mode: providers.ToolChoiceNone}) {
		t.Error("none is accepted in thinking mode, so it is enforced")
	}
}
