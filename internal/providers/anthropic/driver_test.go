package anthropic

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

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// fakeStream serves a canned SSE script as one Anthropic Messages response.
func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("missing x-api-key, got %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Errorf("missing anthropic-version header")
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
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello \"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":42,\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "claude-sonnet-4-6",
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
			t.Fatalf("unexpected error event: %s", ev.Error)
		}
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q, want %q", text.String(), "hello world")
	}
	if done.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 42 || done.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", done.Usage)
	}
}

// TestStreamUsageCarriesModel asserts the model alias from message_start
// flows through to the final Usage. Regression for the bug where every
// loomcycle-backed agent_runs row had cost_usd = 0 because the SSE
// usage event had Model="" — pricing keyed off model returned null,
// jobs-search-web wrote 0 to the column. The fix consumes message_start
// to capture model and stamps it on the message_delta-emitted Usage.
func TestStreamUsageCarriesModel(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-haiku-4-5-20251001\"}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "claude-haiku-4-5",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var done providers.Event
	for ev := range ch {
		if ev.Type == providers.EventDone {
			done = ev
		}
	}
	if done.Usage == nil {
		t.Fatal("done.Usage is nil")
	}
	// Wire model wins (message_start), not the request alias — so
	// downstream pricing matches what Anthropic actually billed.
	if done.Usage.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Usage.Model = %q, want %q", done.Usage.Model, "claude-haiku-4-5-20251001")
	}
}

func TestStreamToolUse(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Read\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"/tmp/x\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

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
	if toolCall.Name != "Read" || toolCall.ID != "toolu_1" {
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
		t.Errorf("stop_reason = %q", stop)
	}
}

// Regression: cancelling ctx mid-stream must not leak the streaming goroutine
// even when the consumer never drains the channel. We verify by inspecting
// `runtime.Stack(all=true)` and looking for our function name; if it's still
// running after a generous post-cancel grace period, the goroutine is stuck.
//
// Important: a naive "wait for channel close" test won't catch this — once
// you start draining, the buffer frees up and the stuck producer unblocks
// regardless of the bug. The leak only manifests when nobody drains.
func TestCancellationDoesNotLeakGoroutine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 30; i++ {
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	_, err := d.Call(ctx, providers.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Let the producer fill the 16-event buffer and block on the next send.
	time.Sleep(100 * time.Millisecond)
	cancel()
	// Generous grace window for the goroutine to observe ctx and exit.
	time.Sleep(300 * time.Millisecond)

	if isStreamEventsRunning(t) {
		t.Fatal("streamEvents goroutine still alive 300ms after ctx cancel — leaked")
	}
}

// isStreamEventsRunning dumps all goroutine stacks and reports whether any
// stack contains our streamEvents function. Bytes scanned, no allocs in the
// hot path; safe for tests.
func isStreamEventsRunning(t *testing.T) bool {
	t.Helper()
	buf := make([]byte, 64*1024)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), ".streamEvents(")
}

// Regression: a 429 from Anthropic must trigger a retry, not fail the run.
// The driver's retry-wrapper re-sends the exact same body bytes — preserving
// the full conversation context (messages, tools, system, cache_control)
// across the rate limit. We assert the second request carries the same
// body the first one did, AND the second request's stream is what the
// caller eventually consumes.
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
			// First call: 429 with a 0-second retry-after so the test runs fast.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		// Second call: stream a normal response.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "claude-sonnet-4-6",
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
	if text.String() != "recovered" {
		t.Errorf("text = %q, want %q (second-call stream not delivered)", text.String(), "recovered")
	}

	mu.Lock()
	defer mu.Unlock()
	if callNum != 2 {
		t.Fatalf("server got %d calls, want 2 (no retry)", callNum)
	}
	// Body bytes must be identical across attempts — that's how context
	// is preserved.
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("retry sent different body:\n  call 1: %s\n  call 2: %s", string(bodies[0]), string(bodies[1]))
	}
}

// Integration regression: a 429 mid-loop must not bubble out as an EventError.
// The retry happens inside the driver's Call(); the loop sees only the
// recovered 200 response. Run() should return cleanly with end_turn and the
// caller's OnEvent must NOT observe any error event.
//
// This is the load-bearing guarantee of v0.3.1: a transient rate limit
// preserves the run's context. The driver-level test verifies the body
// is re-sent; this loop-level test verifies the agent loop is unaffected.
func TestRetryOn429IsInvisibleToLoop(t *testing.T) {
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
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered after 429\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)

	var observedEvents []providers.EventType
	res, err := loop.Run(context.Background(), loop.RunOptions{
		Provider: d,
		Model:    "claude-sonnet-4-6",
		Segments: []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
		},
		OnEvent: func(ev providers.Event) {
			observedEvents = append(observedEvents, ev.Type)
		},
	})
	if err != nil {
		t.Fatalf("Run returned error after 429-then-200: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
	for _, evType := range observedEvents {
		if evType == providers.EventError {
			t.Errorf("loop emitted EventError despite driver-level retry recovery")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if callNum != 2 {
		t.Errorf("server got %d calls, want 2 (driver should have retried)", callNum)
	}
}

// v0.3.2: a 429 must surface a typed EventRetry to the loop's OnEvent
// hook, with provider/attempt/wait_ms/reason populated. This is the full
// pipeline: driver 429 → ratelimit Config.OnEvent → req.OnEvent (loop's
// emit) → opts.OnEvent (caller). The caller (e.g. the SSE handler) needs
// the WaitMs to render "waiting on rate limit" UI.
func TestRetryEmitsTypedEventToLoop(t *testing.T) {
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
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)

	var retryEvents []providers.Event
	_, err := loop.Run(context.Background(), loop.RunOptions{
		Provider: d,
		Model:    "claude-sonnet-4-6",
		Segments: []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
		},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventRetry {
				retryEvents = append(retryEvents, ev)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(retryEvents) != 1 {
		t.Fatalf("got %d retry events, want 1 (one 429 → one retry)", len(retryEvents))
	}
	r := retryEvents[0].Retry
	if r == nil {
		t.Fatal("EventRetry.Retry is nil")
	}
	if r.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", r.Provider)
	}
	if r.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", r.Attempt)
	}
	// Retry-After: 0 honoured.
	if r.WaitMs != 0 {
		t.Errorf("WaitMs = %d, want 0", r.WaitMs)
	}
	if r.Reason == "" {
		t.Errorf("Reason is empty, want non-empty")
	}
}

func TestRequestBodyShape(t *testing.T) {
	// Verify the request marshalling: cache_control on Cacheable system block,
	// proper tool_use / tool_result encoding.
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "claude-sonnet-4-6",
		System: []providers.ContentBlock{
			{Type: "text", Text: "you are helpful", Cacheable: true},
		},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []providers.ContentBlock{
				{Type: "tool_use", ToolUseID: "toolu_1", ToolName: "Read", ToolInput: json.RawMessage(`{"path":"/x"}`)},
			}},
			{Role: "user", Content: []providers.ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_1", Text: "file contents"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
	body := string(captured)
	if !strings.Contains(body, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("cache_control missing on system block:\n%s", body)
	}
	if !strings.Contains(body, `"tool_use_id":"toolu_1"`) {
		t.Errorf("tool_result tool_use_id missing:\n%s", body)
	}
	if !strings.Contains(body, `"stream":true`) {
		t.Errorf("stream flag missing:\n%s", body)
	}
}

// TestBuildRequestBody_MaxTokensDefault verifies the driver's default
// max_tokens. 4096 was the historical value but caused mid-output
// truncation for batch agents (~12k chars output ≈ 4k tokens, hitting
// the cap before the closing `}` of a verdicts JSON). Bumped to 8192
// 2026-05-05; haiku-4-5/sonnet-4-6 both support up to 64k output so
// 8k is still conservative. Revert this constant to fail this test.
func TestBuildRequestBody_MaxTokensDefault(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model:    "claude-haiku-4-5",
		System:   []providers.ContentBlock{{Type: "text", Text: "you are a test"}},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var parsed struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.MaxTokens != 8192 {
		t.Errorf("default max_tokens: got %d, want 8192 (raised from 4096 to give batch-output agents headroom)", parsed.MaxTokens)
	}
}

// TestBuildRequestBody_MaxTokensOverride verifies that req.MaxTokens
// flows through to the wire request when the caller (loop.RunOptions
// → HTTP server, populated from agentDef.MaxTokens) sets it.
func TestBuildRequestBody_MaxTokensOverride(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model:     "claude-haiku-4-5",
		MaxTokens: 16384,
		System:    []providers.ContentBlock{{Type: "text", Text: "x"}},
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "y"}}}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var parsed struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.MaxTokens != 16384 {
		t.Errorf("override max_tokens: got %d, want 16384", parsed.MaxTokens)
	}
}
