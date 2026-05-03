package openai

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
)

// fakeStream serves a canned SSE script as one Chat Completions response.
func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-key")
		}
		w.Header().Set("Content-Type", "text/event-stream")
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
		`data: {"choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"world"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":10}}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "gpt-4o-mini",
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
	if done.Usage.CacheReadTokens != 10 {
		t.Errorf("cache_read_tokens = %d, want 10", done.Usage.CacheReadTokens)
	}
}

func TestStreamToolCallAccumulation(t *testing.T) {
	// First delta carries id + function.name; subsequent deltas dribble out
	// the JSON arguments piecewise. Verify we accumulate into one tool_call.
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/x\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	ch, _ := d.Call(context.Background(), providers.Request{Model: "gpt-4o-mini"})

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
	if toolCall.ID != "call_1" || toolCall.Name != "Read" {
		t.Errorf("tool: %+v", toolCall)
	}
	var input struct{ Path string }
	if err := json.Unmarshal(toolCall.Input, &input); err != nil {
		t.Fatalf("tool input json: %v (raw %s)", err, string(toolCall.Input))
	}
	if input.Path != "/tmp/x" {
		t.Errorf("tool input path = %q", input.Path)
	}
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (normalised from 'tool_calls')", stop)
	}
}

func TestStreamMultipleToolCalls(t *testing.T) {
	// Two tool_calls in parallel (index 0 and 1), interleaved deltas.
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}},` +
			`{"index":1,"id":"call_b","type":"function","function":{"name":"Write","arguments":"{\"x\":1}"}}` +
			`]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("test-key", srv.URL, nil)
	ch, _ := d.Call(context.Background(), providers.Request{Model: "x"})

	var got []providers.ToolUse
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			got = append(got, *ev.ToolUse)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d tool_calls, want 2", len(got))
	}
	if got[0].ID != "call_a" || got[0].Name != "Read" {
		t.Errorf("tool 0: %+v", got[0])
	}
	if got[1].ID != "call_b" || got[1].Name != "Write" {
		t.Errorf("tool 1: %+v", got[1])
	}
}

// Regression: OpenAI may emit tool_calls at non-contiguous indices (e.g.
// 0 and 2 with a gap at 1). The accumulator must surface every present
// index in sorted order, not iterate 0..len(map) and drop anything past
// len.
func TestStreamToolCallNonContiguousIndices(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}},` +
			`{"index":2,"id":"call_c","type":"function","function":{"name":"Write","arguments":"{\"k\":1}"}}` +
			`]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("test-key", srv.URL, nil)
	ch, _ := d.Call(context.Background(), providers.Request{Model: "x"})

	var got []providers.ToolUse
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			got = append(got, *ev.ToolUse)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d tool_calls, want 2 (indices 0 and 2 must both be emitted despite the gap)", len(got))
	}
	if got[0].ID != "call_a" || got[0].Name != "Read" {
		t.Errorf("tool 0: %+v", got[0])
	}
	if got[1].ID != "call_c" || got[1].Name != "Write" {
		t.Errorf("tool 1 (was at index 2): %+v", got[1])
	}
}

func TestRequestBodyShape(t *testing.T) {
	// Verify the request marshalling: system block flattened, tool_use →
	// tool_calls on assistant message, tool_result → role:"tool" message.
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "gpt-4o-mini",
		System: []providers.ContentBlock{
			{Type: "text", Text: "you are helpful", Cacheable: true},
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
	if !strings.Contains(body, `"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read"`) {
		t.Errorf("assistant tool_calls missing/malformed:\n%s", body)
	}
	if !strings.Contains(body, `"role":"tool"`) || !strings.Contains(body, `"tool_call_id":"call_1"`) {
		t.Errorf("role:tool message missing:\n%s", body)
	}
	if !strings.Contains(body, `"include_usage":true`) {
		t.Errorf("stream_options.include_usage missing — usage tokens won't arrive:\n%s", body)
	}
	if !strings.Contains(body, `"tools":[{"type":"function","function":{"name":"Read"`) {
		t.Errorf("tools array missing/malformed:\n%s", body)
	}
}

// Regression: cancelling ctx mid-stream must not leak the streaming goroutine
// when nobody drains the channel. See the Anthropic driver test for the
// rationale on why `runtime.Stack(all=true)` is the right signal here (a
// "wait for channel close" test would silently pass under the bug because
// any drain unblocks the stuck producer).
func TestCancellationDoesNotLeakGoroutine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 30; i++ {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	d := New("test-key", srv.URL, nil)
	_, err := d.Call(ctx, providers.Request{Model: "x"})
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

// Regression: OpenAI 429 with x-ratelimit-reset-* headers (no Retry-After)
// triggers a retry that re-sends the same body. Specifically tests the
// header-fallback path: Retry-After absent, x-ratelimit-reset-tokens=0s.
func TestRetryOn429PreservesContext(t *testing.T) {
	var (
		mu      sync.Mutex
		bodies  [][]byte
		callNum int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		callNum++
		bodies = append(bodies, body)
		n := callNum
		mu.Unlock()

		if n == 1 {
			// 429 — no Retry-After, but x-ratelimit-reset-tokens=0s
			// exercises the OpenAI parser fallback path.
			w.Header().Set("X-Ratelimit-Reset-Requests", "100ms")
			w.Header().Set("X-Ratelimit-Reset-Tokens", "0s")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"recovered"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	ch, err := d.Call(context.Background(), providers.Request{Model: "gpt-4o-mini"})
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

// v0.3.2: a 429 must fire EventRetry through req.OnEvent so the loop's
// SSE consumer sees retry telemetry live during the sleep. This proves
// the driver wires req.OnEvent into ratelimit.Config.OnEvent — without
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
			w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var retries []providers.Event
	var rmu sync.Mutex
	d := New("test-key", srv.URL, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "gpt-4o-mini",
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
	if r.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", r.Provider)
	}
	if r.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", r.Attempt)
	}
}

func TestNon200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()
	d := New("bad-key", srv.URL, nil)
	_, err := d.Call(context.Background(), providers.Request{Model: "x"})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error doesn't mention status code: %v", err)
	}
}
