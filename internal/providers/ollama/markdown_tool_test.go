package ollama

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Tests for the v0.7.x Ollama markdown-form tool-call parser
// (PR #26 follow-up). The base JSON-shape recovery already covered
// `{"name":"...","arguments":{...}}` and the array form; this batch
// pins the bracket-marker shape some chat templates produce instead:
//
//	[tool_use: name]
//	{args}
//
//	[tool_use: name {args}]
//
//	[tool_use: name]                  # no args, defaults to {}
//
// Plus the negative cases that must NOT trip the recovery (prose
// containing the word "tool_use", malformed brackets, ambiguous
// double-args, invalid identifier names).

// ---- pure-function tests ----

func TestParseMarkdownToolCall_PostBracketArgs(t *testing.T) {
	got := parseMarkdownToolCall("[tool_use: search_web]\n{\"query\": \"weather\"}")
	if got == nil {
		t.Fatal("returned nil; expected a parsed call")
	}
	if got.Name != "search_web" {
		t.Errorf("Name = %q, want search_web", got.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(got.Input, &args); err != nil {
		t.Fatalf("Input is not valid JSON: %v (raw %s)", err, got.Input)
	}
	if args["query"] != "weather" {
		t.Errorf("args = %v, want query=weather", args)
	}
}

func TestParseMarkdownToolCall_InlineArgs(t *testing.T) {
	got := parseMarkdownToolCall(`[tool_use: search_web {"query": "weather"}]`)
	if got == nil {
		t.Fatal("returned nil")
	}
	if got.Name != "search_web" {
		t.Errorf("Name = %q, want search_web", got.Name)
	}
	if !strings.Contains(string(got.Input), `"query": "weather"`) {
		t.Errorf("Input = %s, want inline args round-trip", got.Input)
	}
}

func TestParseMarkdownToolCall_NoArgsDefaultsEmptyObject(t *testing.T) {
	// `[tool_use: name]` with nothing after the bracket should default
	// to {} so the dispatcher can call the tool without exploding on
	// a missing input.
	got := parseMarkdownToolCall("[tool_use: ping]")
	if got == nil {
		t.Fatal("returned nil; bare bracket form should parse")
	}
	if got.Name != "ping" {
		t.Errorf("Name = %q, want ping", got.Name)
	}
	if string(got.Input) != "{}" {
		t.Errorf("Input = %s, want {} default", got.Input)
	}
}

func TestParseMarkdownToolCall_RejectsAmbiguousDoubleArgs(t *testing.T) {
	// Inline AND post-bracket args is an ambiguous shape — reject
	// rather than guess which one the model meant.
	got := parseMarkdownToolCall(`[tool_use: name {"x":1}]
{"y":2}`)
	if got != nil {
		t.Errorf("returned %+v, want nil (ambiguous double args must reject)", got)
	}
}

func TestParseMarkdownToolCall_RejectsMissingClosingBracket(t *testing.T) {
	if got := parseMarkdownToolCall("[tool_use: name {\"x\":1}"); got != nil {
		t.Errorf("returned %+v, want nil (missing ])", got)
	}
}

func TestParseMarkdownToolCall_RejectsInvalidIdentifier(t *testing.T) {
	cases := []string{
		"[tool_use: 99name]", // can't start with digit
		"[tool_use: name with spaces]{\"x\":1}",
		"[tool_use: name.with.dots]",
		"[tool_use: ]",
	}
	for _, in := range cases {
		if got := parseMarkdownToolCall(in); got != nil {
			t.Errorf("input %q parsed to %+v, want nil", in, got)
		}
	}
}

func TestParseMarkdownToolCall_RejectsInvalidJSONArgs(t *testing.T) {
	cases := []string{
		`[tool_use: name not json]`,
		`[tool_use: name {bad json}]`,
		"[tool_use: name]\nplain prose",
	}
	for _, in := range cases {
		if got := parseMarkdownToolCall(in); got != nil {
			t.Errorf("input %q parsed to %+v, want nil", in, got)
		}
	}
}

// ---- end-to-end through tryParseToolCallsFromText ----

func TestTryParseToolCallsFromText_MarkdownPostBracket(t *testing.T) {
	calls := tryParseToolCallsFromText("[tool_use: search_web]\n{\"query\":\"x\"}")
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "search_web" {
		t.Errorf("Name = %q, want search_web", calls[0].Name)
	}
}

func TestTryParseToolCallsFromText_MarkdownInline(t *testing.T) {
	calls := tryParseToolCallsFromText(`[tool_use: get_weather {"city":"Berlin"}]`)
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("got %+v, want one call name=get_weather", calls)
	}
}

func TestTryParseToolCallsFromText_DoesNotFalsePositiveOnProseMentioningToolUse(t *testing.T) {
	// "I would call tool_use here" — common phrasing in agent
	// reasoning. Must NOT synthesize a call. The strict-prefix-match
	// (`[tool_use:`) on the trimmed body is what guards this.
	cases := []string{
		"I would call tool_use here, but I'll think first.",
		"To answer your question I'd use the tool_use feature.",
		"Use [tool_use: name] format — placeholder example.",
	}
	for _, prose := range cases {
		if calls := tryParseToolCallsFromText(prose); len(calls) > 0 {
			t.Errorf("input %q false-positived to %+v", prose, calls)
		}
	}
}

// TestStreamMarkdownFormToolCall is the end-to-end through the
// streaming path — confirms the recovery actually wires through the
// driver to an EventToolCall the same way the JSON shape does (PR #26).
func TestStreamMarkdownFormToolCall(t *testing.T) {
	frames := []string{
		`{"model":"hermes3:latest","message":{"role":"assistant","content":"[tool_use: search_web]\n{\"query\":\"weather\"}"},"done":true,"done_reason":"stop"}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "hermes3:latest",
		Tools: []providers.ToolSpec{{Name: "search_web", InputSchema: json.RawMessage(`{}`)}},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "what is the weather"}}},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var sawToolCall bool
	var done providers.Event
	for ev := range ch {
		switch ev.Type {
		case providers.EventToolCall:
			sawToolCall = true
			if ev.ToolUse == nil || ev.ToolUse.Name != "search_web" {
				t.Errorf("synthesised tool_call = %+v, want name=search_web", ev.ToolUse)
			}
		case providers.EventDone:
			done = ev
		}
	}
	if !sawToolCall {
		t.Fatal("EventToolCall not synthesised; markdown-form recovery did not fire end-to-end")
	}
	// Recovery should remap done.StopReason to tool_use so the loop
	// runs another iteration — same contract as the JSON-shape path.
	if done.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", done.StopReason)
	}
}
