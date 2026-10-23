// An EXTERNAL test package, like the other loop_test files: the Anthropic
// driver lives in the internal/providers module, which cannot import the loop,
// so the tests that run the real driver under the real loop live here.
package loop_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Integration regression: a 429 mid-loop must not bubble out as an EventError.
// The retry happens inside the driver's Call(); the loop sees only the
// recovered 200 response. Run() should return cleanly with end_turn and the
// caller's OnEvent must NOT observe any error event.
//
// This is the load-bearing guarantee of v0.3.1: a transient rate limit
// preserves the run's context. The driver-level test (anthropic's
// TestRetryOn429PreservesContext) verifies the body is re-sent; this
// loop-level test verifies the agent loop is unaffected.
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

	d := anthropic.New("test-key", srv.URL, streamhttp.Options{}, nil)

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

	d := anthropic.New("test-key", srv.URL, streamhttp.Options{}, nil)

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
