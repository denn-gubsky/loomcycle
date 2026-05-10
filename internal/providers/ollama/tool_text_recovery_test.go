package ollama

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Five tests pin the qwen3 tool-call-as-text recovery contract:
//
//   (a) text is exact tool-call JSON + req.Tools set + no structured
//       envelope → synthesised EventToolCall, original text events
//       still emitted (transcript fidelity).
//   (b) plain prose answer → no synthesis (don't false-positive).
//   (c) JSON-ish text but doesn't match tool-call shape → no synthesis.
//   (d) structured tool_call already emitted → no synthesis (don't
//       double-emit even if text content also looks parseable).
//   (e) req.Tools empty → recovery disabled (an agent whose final
//       answer IS a JSON object — ats-filter, injection-judge —
//       must not accidentally trip the recovery).

func TestToolTextRecovery_SynthesizesCallFromText(t *testing.T) {
	// qwen3's classic regression: emits a structured tool call on
	// iter 1 (off-screen for this test), then in iter 2 writes the
	// next call as content text. The driver should detect and
	// synthesise EventToolCall so the loop iterates instead of
	// terminating with the JSON dump as the final answer.
	frames := []string{
		`{"model":"qwen3:14b","message":{"role":"assistant","content":"{\"name\": \"mcp__jobs__getApplication\", \"arguments\": {\"id\": \"app-abc\"}}"},"done":false}` + "\n",
		`{"model":"qwen3:14b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":1000,"eval_count":50}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "qwen3:14b",
		Tools: []providers.ToolSpec{{Name: "mcp__jobs__getApplication", InputSchema: json.RawMessage(`{}`)}},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "look up app-abc"}}},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var sawText, sawSynthCall bool
	var done providers.Event
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			sawText = true
		case providers.EventToolCall:
			sawSynthCall = true
			if ev.ToolUse == nil || ev.ToolUse.Name != "mcp__jobs__getApplication" {
				t.Errorf("synthesised tool_call = %+v, want name=mcp__jobs__getApplication", ev.ToolUse)
			}
			var args map[string]any
			_ = json.Unmarshal(ev.ToolUse.Input, &args)
			if args["id"] != "app-abc" {
				t.Errorf("synthesised arguments = %v, want id=app-abc", args)
			}
		case providers.EventDone:
			done = ev
		}
	}
	if !sawText {
		t.Error("expected text events to still emit (transcript fidelity)")
	}
	if !sawSynthCall {
		t.Fatal("expected synthesised EventToolCall — qwen3 recovery didn't fire")
	}
	// stopReason should be remapped to tool_use (was 'stop' on the wire
	// because qwen3 thought it was done; recovery knows otherwise).
	if done.StopReason != "tool_use" {
		t.Errorf("done.StopReason = %q, want tool_use (recovery should remap)", done.StopReason)
	}
}

func TestToolTextRecovery_SynthesizesArrayOfCalls(t *testing.T) {
	// qwen3 occasionally batches multiple calls into a single text
	// block as a JSON array. Recovery should handle the array form.
	frames := []string{
		`{"model":"qwen3:14b","message":{"role":"assistant","content":"[{\"name\": \"a\", \"arguments\": {\"x\": 1}}, {\"name\": \"b\", \"arguments\": {\"y\": 2}}]"},"done":true,"done_reason":"stop"}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "qwen3:14b",
		Tools:    []providers.ToolSpec{{Name: "a"}, {Name: "b"}},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	var calls []string
	for ev := range ch {
		if ev.Type == providers.EventToolCall && ev.ToolUse != nil {
			calls = append(calls, ev.ToolUse.Name)
		}
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Errorf("synthesised calls = %v, want [a b]", calls)
	}
}

func TestToolTextRecovery_StripsMarkdownFence(t *testing.T) {
	// qwen3's chat template sometimes wraps the tool-call JSON in a
	// ```json ... ``` markdown fence. Recovery should strip and
	// parse cleanly.
	frames := []string{
		"{\"model\":\"qwen3:14b\",\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"name\\\": \\\"foo\\\", \\\"arguments\\\": {}}\\n```\"},\"done\":true,\"done_reason\":\"stop\"}\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "qwen3:14b",
		Tools:    []providers.ToolSpec{{Name: "foo"}},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	var sawSynth bool
	for ev := range ch {
		if ev.Type == providers.EventToolCall && ev.ToolUse != nil && ev.ToolUse.Name == "foo" {
			sawSynth = true
		}
	}
	if !sawSynth {
		t.Error("expected recovery to strip markdown fence and parse the inner JSON")
	}
}

func TestToolTextRecovery_DoesNotFalsePositiveOnProse(t *testing.T) {
	// Plain prose answer must NOT trigger the recovery — agents
	// without tools (or with structured-JSON-output that's NOT a
	// tool-call shape) should pass through cleanly.
	frames := []string{
		`{"model":"qwen3:14b","message":{"role":"assistant","content":"Here is your answer: 42."},"done":true,"done_reason":"stop"}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "qwen3:14b",
		Tools:    []providers.ToolSpec{{Name: "foo"}},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	var sawSynth bool
	var done providers.Event
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			sawSynth = true
		}
		if ev.Type == providers.EventDone {
			done = ev
		}
	}
	if sawSynth {
		t.Error("recovery false-positived on plain prose")
	}
	// stopReason should remain "end_turn" (no recovery → no remap).
	if done.StopReason != "end_turn" {
		t.Errorf("done.StopReason = %q, want end_turn", done.StopReason)
	}
}

func TestToolTextRecovery_NoSynthWhenStructuredCallAlreadyEmitted(t *testing.T) {
	// qwen3 emits BOTH a structured envelope AND text content (the
	// tool_call as text alongside the proper one). Don't double-emit.
	// Recovery is gated on !hadToolCalls.
	frames := []string{
		`{"model":"qwen3:14b","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"foo","arguments":{}}}]},"done":false}` + "\n",
		`{"model":"qwen3:14b","message":{"role":"assistant","content":"{\"name\": \"foo\", \"arguments\": {}}"},"done":true,"done_reason":"stop"}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "qwen3:14b",
		Tools:    []providers.ToolSpec{{Name: "foo"}},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	var toolCallCount int
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			toolCallCount++
		}
	}
	if toolCallCount != 1 {
		t.Errorf("EventToolCall count = %d, want 1 (no double-emit)", toolCallCount)
	}
}

func TestToolTextRecovery_DisabledWhenNoToolsRequested(t *testing.T) {
	// req.Tools empty → wantTools=false → recovery disabled. An
	// agent whose final answer IS a JSON tool-call-shaped object
	// (vanishingly rare but possible) must not have its answer
	// hijacked into a synthesised tool call.
	frames := []string{
		`{"model":"qwen3:14b","message":{"role":"assistant","content":"{\"name\": \"foo\", \"arguments\": {}}"},"done":true,"done_reason":"stop"}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model: "qwen3:14b",
		// Tools intentionally empty — recovery must NOT fire.
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	var text strings.Builder
	var sawSynth bool
	for ev := range ch {
		if ev.Type == providers.EventText {
			text.WriteString(ev.Text)
		}
		if ev.Type == providers.EventToolCall {
			sawSynth = true
		}
	}
	if sawSynth {
		t.Error("recovery fired on no-tools request — must be disabled")
	}
	if !strings.Contains(text.String(), `"name": "foo"`) {
		t.Errorf("text passthrough corrupted: %q", text.String())
	}
}

// ---- Pure-function tests for the parser (no streaming infra) ----

func TestTryParseToolCallsFromText_BareObject(t *testing.T) {
	calls := tryParseToolCallsFromText(`{"name":"foo","arguments":{"x":1}}`)
	if len(calls) != 1 || calls[0].Name != "foo" {
		t.Errorf("parse = %+v, want one call name=foo", calls)
	}
}

func TestTryParseToolCallsFromText_RejectsMissingName(t *testing.T) {
	if got := tryParseToolCallsFromText(`{"arguments":{}}`); got != nil {
		t.Errorf("parse = %v, want nil (no name field)", got)
	}
}

func TestTryParseToolCallsFromText_RejectsArbitraryJSON(t *testing.T) {
	// JSON that's not a tool-call shape must not parse — agents that
	// emit structured-but-different JSON (verdicts, scores, etc.)
	// shouldn't trip the recovery.
	if got := tryParseToolCallsFromText(`{"verdict":"keep","score":0.8}`); got != nil {
		t.Errorf("parse = %v, want nil (not a tool-call shape)", got)
	}
}

func TestTryParseToolCallsFromText_RejectsTrailingProse(t *testing.T) {
	// We require the ENTIRE trimmed content to deserialise. A
	// tool-call JSON followed by a sentence of prose is ambiguous —
	// don't recover.
	if got := tryParseToolCallsFromText(`{"name":"foo","arguments":{}} and then I'll explain`); got != nil {
		t.Errorf("parse = %v, want nil (trailing prose disqualifies)", got)
	}
}

func TestTryParseToolCallsFromText_DefaultsEmptyArguments(t *testing.T) {
	// Missing arguments field → default to {} so the loop doesn't
	// fail to dispatch the tool. Same default the live path uses.
	calls := tryParseToolCallsFromText(`{"name":"foo"}`)
	if len(calls) != 1 {
		t.Fatalf("parse = %+v, want one call", calls)
	}
	if string(calls[0].Input) != "{}" {
		t.Errorf("default arguments = %q, want {}", string(calls[0].Input))
	}
}
