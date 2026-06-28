package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// fakeStream serves a canned NDJSON script as one /api/chat response. It also
// answers the driver's best-effort /api/ps context-window probe with an empty
// model list (→ window "unknown"/0), so streamEvents' usage stamp doesn't error.
func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		for _, f := range frames {
			fmt.Fprint(w, f)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
}

func TestStreamTextThenStop(t *testing.T) {
	frames := []string{
		`{"model":"llama3.1","message":{"role":"assistant","content":"hello "},"done":false}` + "\n",
		`{"model":"llama3.1","message":{"role":"assistant","content":"world"},"done":false}` + "\n",
		`{"model":"llama3.1","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":42,"eval_count":7}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "llama3.1",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var text strings.Builder
	var done providers.Event
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventDone:
			done = ev
		case providers.EventError:
			t.Fatalf("unexpected error: %s", ev.Error)
		}
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q, want %q", text.String(), "hello world")
	}
	if done.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn (normalised from 'stop')", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 42 || done.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", done.Usage)
	}
	if done.Usage.Model != "llama3.1" {
		t.Errorf("usage.Model = %q, want llama3.1", done.Usage.Model)
	}
}

func TestStreamToolCallOnFinalFrame(t *testing.T) {
	// Ollama doesn't index-stream tool_calls — they ship complete on the
	// frame where the model decides to invoke them (often the final one).
	frames := []string{
		`{"model":"llama3.1","message":{"role":"assistant","content":""},"done":false}` + "\n",
		`{"model":"llama3.1","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"Read","arguments":{"path":"/tmp/x"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":3}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{Model: "llama3.1"})

	var toolCall *providers.ToolUse
	var stop string
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			toolCall = ev.ToolUse
		}
		if ev.Type == providers.EventDone {
			stop = ev.StopReason
		}
	}
	if toolCall == nil {
		t.Fatal("no tool_call emitted")
	}
	if toolCall.Name != "Read" {
		t.Errorf("tool: %+v", toolCall)
	}
	var input struct{ Path string }
	if err := json.Unmarshal(toolCall.Input, &input); err != nil {
		t.Fatalf("tool input json: %v (raw %s)", err, string(toolCall.Input))
	}
	if input.Path != "/tmp/x" {
		t.Errorf("tool input path = %q", input.Path)
	}
	// Tool calls override done_reason to "tool_use" so the loop iterates.
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (overridden because tool_calls present)", stop)
	}
}

// Regression: Ollama may emit tool_calls on a non-final frame and then a
// separate done:true frame with no tool_calls. Stop reason must still be
// "tool_use" so the agent loop runs the tool we already emitted.
func TestStreamToolCallOnNonFinalFrame(t *testing.T) {
	frames := []string{
		`{"model":"llama3.1","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"Read","arguments":{"path":"/x"}}}]},"done":false}` + "\n",
		`{"model":"llama3.1","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":3}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{Model: "llama3.1"})

	var toolCalls int
	var stop string
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			toolCalls++
		}
		if ev.Type == providers.EventDone {
			stop = ev.StopReason
		}
	}
	if toolCalls != 1 {
		t.Errorf("got %d tool_calls, want 1", toolCalls)
	}
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (must persist across frames so the loop can run the tool emitted earlier)", stop)
	}
}

func TestRequestBodyShape(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	temp := 0.7
	ch, err := d.Call(context.Background(), providers.Request{
		Model:       "llama3.1",
		Temperature: &temp,
		MaxTokens:   100,
		System: []providers.ContentBlock{
			{Type: "text", Text: "you are helpful"},
		},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []providers.ContentBlock{
				{Type: "text", Text: "ok"},
				{Type: "tool_use", ToolUseID: "call_1", ToolName: "Read", ToolInput: json.RawMessage(`{"path":"/x"}`)},
			}},
			{Role: "user", Content: []providers.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Text: "file contents"},
			}},
		},
		Tools: []providers.ToolSpec{
			{Name: "Read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	body := string(captured)
	if !strings.Contains(body, `"role":"system"`) || !strings.Contains(body, `"you are helpful"`) {
		t.Errorf("system block missing/malformed:\n%s", body)
	}
	if !strings.Contains(body, `"tool_calls":[{"function":{"name":"Read","arguments":{"path":"/x"}}}]`) {
		t.Errorf("assistant tool_calls missing/malformed:\n%s", body)
	}
	if !strings.Contains(body, `"role":"tool"`) || !strings.Contains(body, `"content":"file contents"`) {
		t.Errorf("role:tool message missing:\n%s", body)
	}
	if !strings.Contains(body, `"temperature":0.7`) || !strings.Contains(body, `"num_predict":100`) {
		t.Errorf("options.temperature/num_predict missing:\n%s", body)
	}
	if !strings.Contains(body, `"tools":[{"type":"function","function":{"name":"Read"`) {
		t.Errorf("tools array missing/malformed:\n%s", body)
	}
}

// TestRequestBody_NumCtxOmittedByDefault pins that the wire body
// contains NO num_ctx field when the driver was constructed without
// WithNumCtx — preserves the v0.8.x default (Ollama applies its
// server-side num_ctx). The omitempty tag is load-bearing: a literal
// "num_ctx":0 would CLAMP every model's context to zero and break
// every Ollama deploy.
func TestRequestBody_NumCtxOmittedByDefault(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "llama3.1",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	if strings.Contains(string(captured), `"num_ctx"`) {
		t.Errorf("num_ctx appeared in body without WithNumCtx; body:\n%s", string(captured))
	}
}

// TestCapabilities_MaxContextTokensReflectsNumCtx pins that the context
// window the driver advertises equals the operator-pinned num_ctx — the
// value the interactive terminal's context gauge renders as used/max/%.
// Without WithNumCtx the per-model window is genuinely unknown (the driver
// is model-agnostic), so it stays 0 and the gauge shows only the absolute
// used size. Fail-before: Capabilities hardcoded MaxContextTokens: 0.
func TestCapabilities_MaxContextTokensReflectsNumCtx(t *testing.T) {
	if got := New("ollama-local", "", "", streamhttp.Options{}, nil).Capabilities().MaxContextTokens; got != 0 {
		t.Errorf("no num_ctx: MaxContextTokens = %d, want 0 (unknown)", got)
	}
	if got := New("ollama-local", "", "", streamhttp.Options{}, nil).WithNumCtx(32768).Capabilities().MaxContextTokens; got != 32768 {
		t.Errorf("num_ctx=32768: MaxContextTokens = %d, want 32768", got)
	}
}

// TestRequestBody_NumCtxPropagated pins the headline path: after
// WithNumCtx(N), every chat request carries options.num_ctx=N. The
// load-bearing assertion behind the 2026-05-15 employer-profiler
// truncation incident — without this, Ollama silently truncates at
// the server's 4096-token default.
func TestRequestBody_NumCtxPropagated(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil).WithNumCtx(32768)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "glm-4.7-flash:q4_K_M",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	if !strings.Contains(string(captured), `"num_ctx":32768`) {
		t.Errorf("num_ctx=32768 missing from body; body:\n%s", string(captured))
	}
	// Other options fields must remain absent (omitempty) when not
	// set by the caller — a bare WithNumCtx must not accidentally
	// pin temperature or num_predict to zero.
	if strings.Contains(string(captured), `"temperature"`) || strings.Contains(string(captured), `"num_predict"`) {
		t.Errorf("WithNumCtx leaked unrelated options fields; body:\n%s", string(captured))
	}
}

// TestRequestBody_NumGpuOmittedByDefault pins that no num_gpu field is sent
// when the driver was built without WithNumGpu. The omitempty tag is
// load-bearing: a literal "num_gpu":0 would force Ollama to run CPU-only.
func TestRequestBody_NumGpuOmittedByDefault(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "llama3.1",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	if strings.Contains(string(captured), `"num_gpu"`) {
		t.Errorf("num_gpu appeared in body without WithNumGpu; body:\n%s", string(captured))
	}
}

// TestRequestBody_NumGpuPropagated pins the headline path: after
// WithNumGpu(99), every chat request carries options.num_gpu=99 — the
// knob that forces GPU offload on boxes (e.g. APUs) where Ollama's
// auto-detection otherwise falls back to CPU.
func TestRequestBody_NumGpuPropagated(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil).WithNumGpu(99)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "qwen3.6:27b",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	if !strings.Contains(string(captured), `"num_gpu":99`) {
		t.Errorf("num_gpu=99 missing from body; body:\n%s", string(captured))
	}
	// A bare WithNumGpu must not leak unrelated options (omitempty).
	if strings.Contains(string(captured), `"temperature"`) || strings.Contains(string(captured), `"num_predict"`) {
		t.Errorf("WithNumGpu leaked unrelated options fields; body:\n%s", string(captured))
	}
}

// TestRequestBody_NumCtxCombinesWithTemperatureAndMaxTokens: when the
// caller passes Temperature + MaxTokens AND the driver was configured
// with WithNumCtx, all three end up in the single options object. Pins
// that the v0.8.x conditional that builds wireOptions correctly merges
// the per-request hints with the driver-level num_ctx.
func TestRequestBody_NumCtxCombinesWithTemperatureAndMaxTokens(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil).WithNumCtx(16384)
	temp := 0.3
	ch, err := d.Call(context.Background(), providers.Request{
		Model:       "glm-4.7-flash:q4_K_M",
		Temperature: &temp,
		MaxTokens:   2048,
		Messages:    []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	body := string(captured)
	for _, want := range []string{`"temperature":0.3`, `"num_predict":2048`, `"num_ctx":16384`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
}

// TestWithNumCtx_RejectsNonPositive: a zero or negative value passed
// to WithNumCtx must be treated as "no change", not as "explicitly
// disable" — explicitly disabling would be a footgun (the driver
// would always send "num_ctx":0 and break every model). Pins the
// idempotent guard inside WithNumCtx.
func TestWithNumCtx_RejectsNonPositive(t *testing.T) {
	d := New("", "", "http://localhost:11434", streamhttp.Options{}, nil)
	d.WithNumCtx(0)
	if d.numCtx != 0 {
		t.Errorf("WithNumCtx(0) modified numCtx to %d; want 0 unchanged", d.numCtx)
	}
	d.WithNumCtx(-1)
	if d.numCtx != 0 {
		t.Errorf("WithNumCtx(-1) modified numCtx to %d; want 0 unchanged", d.numCtx)
	}
	d.WithNumCtx(8192)
	if d.numCtx != 8192 {
		t.Errorf("WithNumCtx(8192) failed to set; numCtx = %d", d.numCtx)
	}
}

// Regression: cancelling ctx mid-stream must not leak the streaming goroutine
// when nobody drains the channel. See the Anthropic driver test for the
// rationale on why `runtime.Stack(all=true)` is the right signal here.
func TestCancellationDoesNotLeakGoroutine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		for i := 0; i < 30; i++ {
			fmt.Fprint(w, `{"model":"llama3.1","message":{"role":"assistant","content":"x"},"done":false}`+"\n")
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	_, err := d.Call(ctx, providers.Request{Model: "llama3.1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(300 * time.Millisecond)

	if isStreamEventsRunning(t) {
		t.Fatal("streamEvents goroutine still alive 300ms after ctx cancel — leaked")
	}
}

func isStreamEventsRunning(t *testing.T) bool {
	t.Helper()
	buf := make([]byte, 64*1024)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), ".streamEvents(")
}

func TestNon200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	_, err := d.Call(context.Background(), providers.Request{Model: "nope"})
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error doesn't mention status code: %v", err)
	}
}

// Regression: Ollama Cloud may emit a 429 with Retry-After. The driver
// retries with the same body. (Ollama OSS doesn't 429, but the wrap is
// shared transport-level code so we exercise it here defensively.)
func TestRetryOn429PreservesContext(t *testing.T) {
	var (
		mu      sync.Mutex
		bodies  [][]byte
		callNum int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" { // best-effort gauge probe — don't count
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		callNum++
		bodies = append(bodies, body)
		n := callNum
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"too many requests"}`))
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"model":"llama3.1","message":{"role":"assistant","content":"recovered"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"llama3.1","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{Model: "llama3.1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var text strings.Builder
	for ev := range ch {
		if ev.Type == providers.EventText {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "recovered" {
		t.Errorf("text = %q, want %q", text.String(), "recovered")
	}
	mu.Lock()
	defer mu.Unlock()
	if callNum != 2 {
		t.Fatalf("server got %d calls, want 2", callNum)
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("retry body differs from original")
	}
}

// v0.3.2: a 429 must fire EventRetry through req.OnEvent. Proves the
// driver wires req.OnEvent into ratelimit.Config.OnEvent — without
// that line, the EventRetry never reaches the caller.
func TestRetryEmitsEventToRequestOnEvent(t *testing.T) {
	var callNum int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callNum++
		n := callNum
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"too many requests"}`))
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"model":"llama3.1","message":{"role":"assistant","content":"ok"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"llama3.1","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`+"\n")
	}))
	defer srv.Close()

	var retries []providers.Event
	var rmu sync.Mutex
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "llama3.1",
		OnEvent: func(ev providers.Event) {
			rmu.Lock()
			defer rmu.Unlock()
			if ev.Type == providers.EventRetry {
				retries = append(retries, ev)
			}
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	rmu.Lock()
	defer rmu.Unlock()
	if len(retries) != 1 {
		t.Fatalf("got %d retry events, want 1", len(retries))
	}
	r := retries[0].Retry
	if r == nil {
		t.Fatal("Retry payload missing")
	}
	if r.Provider != "ollama" {
		t.Errorf("Provider = %q, want ollama", r.Provider)
	}
	if r.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", r.Attempt)
	}
}

func TestStopReasonWithoutToolCalls(t *testing.T) {
	// done_reason "length" should map to max_tokens.
	frames := []string{
		`{"model":"llama3.1","message":{"role":"assistant","content":"truncated…"},"done":true,"done_reason":"length","prompt_eval_count":1,"eval_count":1}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{Model: "x"})
	var stop string
	for ev := range ch {
		if ev.Type == providers.EventDone {
			stop = ev.StopReason
		}
	}
	if stop != "max_tokens" {
		t.Errorf("stop_reason = %q, want max_tokens", stop)
	}
}

// TestDriver_ID_RespectsConstructorArg pins the v0.8.3 split: one
// driver type, two registrations differentiated by providerID. The
// empty-string default falls through to "ollama" so anything that
// imported `ollama.New` before the signature widened keeps working.
func TestDriver_ID_RespectsConstructorArg(t *testing.T) {
	cases := []struct {
		giveID string
		wantID string
	}{
		{"ollama", "ollama"},
		{"ollama-local", "ollama-local"},
		{"", "ollama"}, // default
	}
	for _, tc := range cases {
		t.Run(tc.giveID, func(t *testing.T) {
			d := New(tc.giveID, "", "http://localhost:11434", streamhttp.Options{}, nil)
			if got := d.ID(); got != tc.wantID {
				t.Errorf("ID() = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestDriver_AuthHeaderEmittedOnlyWhenKeySet pins the split's auth
// shape: the hosted ollama.com registration sends Bearer; the local
// registration sends nothing (matches Ollama OSS's local-trust model
// — servers in the wild rely on the absence of an auth header to
// distinguish "loopback client" from "internet client").
func TestDriver_AuthHeaderEmittedOnlyWhenKeySet(t *testing.T) {
	cases := []struct {
		name       string
		providerID string
		apiKey     string
		wantHeader string // "" → expect no Authorization sent
	}{
		{"local: no auth", "ollama-local", "", ""},
		{"cloud: Bearer sent", "ollama", "tok-xyz", "Bearer tok-xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(200)
				fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`+"\n")
			}))
			defer srv.Close()
			d := New(tc.providerID, tc.apiKey, srv.URL, streamhttp.Options{}, nil)
			ch, err := d.Call(context.Background(), providers.Request{Model: "x"})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			for range ch { // drain
			}
			if got != tc.wantHeader {
				t.Errorf("Authorization header = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

// TestDriver_NonOKErrorUsesProviderID pins that error messages carry
// the provider id (not the hardcoded literal "ollama"), so the v0.8.2
// error classifier — anchored on the "<name> <code>:" prefix — works
// for both registrations.
func TestDriver_NonOKErrorUsesProviderID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, "backend unavailable")
	}))
	defer srv.Close()
	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	_, err := d.Call(context.Background(), providers.Request{Model: "x"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "ollama-local 503:") {
		t.Errorf("err = %q, want prefix \"ollama-local 503:\"", err.Error())
	}
}

// Regression: Ollama (both local /api/chat and ollama.com cloud) streams
// one token per chunk. Pre-fix the driver emitted one EventText per
// chunk, producing one events-table row per token and one Web UI card
// per token (job-searcher r_935214273f141fb9 on 2026-05-17 logged 317
// text events for a single run). The driver now coalesces consecutive
// content deltas into ≥64-byte EventText emissions; this test asserts
// that 14 single-token deltas (~70 chars total) emit ≤2 EventText
// events instead of 14, and that the joined text is byte-identical to
// the wire content.
func TestStreamCoalescesPerTokenContentDeltas(t *testing.T) {
	tokens := []string{
		"Now", " let", " me", " load", " the", " relevance",
		"-filter", "ing", " skill", " and", " then", " start",
		" searching", ".",
	}
	var frames []string
	for _, tok := range tokens {
		frames = append(frames,
			`{"model":"llama3.1","message":{"role":"assistant","content":`+jsonString(tok)+`},"done":false}`+"\n",
		)
	}
	frames = append(frames,
		`{"model":"llama3.1","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":14}`+"\n",
	)

	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{Model: "llama3.1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var joined strings.Builder
	textCount := 0
	for ev := range ch {
		if ev.Type == providers.EventText {
			textCount++
			joined.WriteString(ev.Text)
		}
	}

	want := strings.Join(tokens, "")
	if joined.String() != want {
		t.Errorf("joined text = %q, want %q", joined.String(), want)
	}
	// 14 tokens × ~5 chars = ~70 chars total. With a 64-byte threshold
	// the buffer should flush exactly once during the stream (when it
	// crosses 64) and once more at end-of-stream for the tail. So
	// textCount ∈ {1, 2}; anything ≥3 means the coalesce regressed.
	if textCount > 2 {
		t.Errorf("EventText count = %d, want ≤2 (coalesce regressed; pre-fix would emit %d)", textCount, len(tokens))
	}
	if textCount < 1 {
		t.Errorf("EventText count = %d, want ≥1 (text was silently dropped)", textCount)
	}
}

// Regression: a newline in a content delta must force a flush so
// paragraph breaks survive coalescing. Without this, a 60-byte buffer
// would swallow the newline boundary and concatenate two paragraphs
// into one EventText.
func TestStreamCoalesceFlushesOnNewline(t *testing.T) {
	frames := []string{
		`{"model":"x","message":{"role":"assistant","content":"first paragraph\n"},"done":false}` + "\n",
		`{"model":"x","message":{"role":"assistant","content":"second paragraph"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":2}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var events []string
	for ev := range ch {
		if ev.Type == providers.EventText {
			events = append(events, ev.Text)
		}
	}
	if len(events) != 2 {
		t.Fatalf("EventText count = %d, want 2 (newline must split); got events %#v", len(events), events)
	}
	if events[0] != "first paragraph\n" {
		t.Errorf("event[0] = %q, want %q (newline-bearing delta should flush including the newline)", events[0], "first paragraph\n")
	}
	if events[1] != "second paragraph" {
		t.Errorf("event[1] = %q, want %q (post-newline delta should land in a fresh buffer flushed at end-of-stream)", events[1], "second paragraph")
	}
}

// jsonString quotes a string as a JSON literal — handy for building
// stream frames inline above without dragging in encoding/json
// just to write a token.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestUsage_MaxContextTokensFromLoadedContext pins that the driver reports the
// model's ACTUAL loaded context window (read from /api/ps) on the usage event —
// so the UI gauge is truthful for local models with no explicit num_ctx. An
// operator num_ctx still wins (exact), and a not-yet-loaded model (absent from
// /api/ps) reports 0 ("unknown"). Fail-before: the driver never set
// usage.MaxContextTokens, so a direct Call always reported 0.
func TestUsage_MaxContextTokensFromLoadedContext(t *testing.T) {
	chatFrames := `{"model":"qwen3.6:27b","message":{"role":"assistant","content":"ok"},"done":false}` + "\n" +
		`{"model":"qwen3.6:27b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":2}` + "\n"
	newSrv := func(psBody string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ps" {
				fmt.Fprint(w, psBody)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprint(w, chatFrames)
		}))
	}
	runCall := func(t *testing.T, d *Driver) *providers.Usage {
		t.Helper()
		ch, err := d.Call(context.Background(), providers.Request{
			Model:    "qwen3.6:27b",
			Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		var u *providers.Usage
		for ev := range ch {
			if ev.Type == providers.EventDone {
				u = ev.Usage
			}
		}
		if u == nil {
			t.Fatal("no usage on done")
		}
		return u
	}

	t.Run("loaded context from /api/ps", func(t *testing.T) {
		srv := newSrv(`{"models":[{"name":"qwen3.6:27b","context_length":131072}]}`)
		defer srv.Close()
		if u := runCall(t, New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)); u.MaxContextTokens != 131072 {
			t.Errorf("MaxContextTokens = %d, want 131072 (from /api/ps)", u.MaxContextTokens)
		}
	})
	t.Run("explicit num_ctx wins", func(t *testing.T) {
		srv := newSrv(`{"models":[{"name":"qwen3.6:27b","context_length":131072}]}`)
		defer srv.Close()
		if u := runCall(t, New("ollama-local", "", srv.URL, streamhttp.Options{}, nil).WithNumCtx(32768)); u.MaxContextTokens != 32768 {
			t.Errorf("MaxContextTokens = %d, want 32768 (operator num_ctx wins)", u.MaxContextTokens)
		}
	})
	t.Run("model not loaded reports 0", func(t *testing.T) {
		srv := newSrv(`{"models":[]}`)
		defer srv.Close()
		if u := runCall(t, New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)); u.MaxContextTokens != 0 {
			t.Errorf("MaxContextTokens = %d, want 0 (model not in /api/ps)", u.MaxContextTokens)
		}
	})
}
