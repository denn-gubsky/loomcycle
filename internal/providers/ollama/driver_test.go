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
)

// fakeStream serves a canned NDJSON script as one /api/chat response.
func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	d := New(srv.URL, nil)
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
	d := New(srv.URL, nil)
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
	d := New(srv.URL, nil)
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
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"x","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New(srv.URL, nil)
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
	d := New(srv.URL, nil)
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
	d := New(srv.URL, nil)
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

	d := New(srv.URL, nil)
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
	d := New(srv.URL, nil)
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
	d := New(srv.URL, nil)
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
