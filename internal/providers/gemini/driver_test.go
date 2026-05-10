package gemini

import (
	"bytes"
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

// fakeStream serves a canned SSE script as one streamGenerateContent
// response. Asserts the x-goog-api-key header lands and that the
// model name is in the URL path (Gemini reads it from there, not
// from the body).
func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("x-goog-api-key = %q, want test-key", got)
		}
		// URL shape: /v1beta/models/{model}:streamGenerateContent
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("path = %q, want suffix :streamGenerateContent", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("query alt = %q, want sse", r.URL.Query().Get("alt"))
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
		`data: {"candidates":[{"content":{"parts":[{"text":"hello "}],"role":"model"}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[{"text":"world"}],"role":"model"}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":7,"totalTokenCount":49},"modelVersion":"gemini-2.0-flash-001"}` + "\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "gemini-2.0-flash",
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
		t.Errorf("text = %q, want hello world", text.String())
	}
	if done.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn (mapped from STOP)", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 42 || done.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", done.Usage)
	}
	// modelVersion from chunk envelope MUST land on Usage.Model so
	// downstream cost rollups can attribute. Same regression class
	// as the v0.6.0 OpenAI driver fix.
	if done.Usage.Model != "gemini-2.0-flash-001" {
		t.Errorf("Usage.Model = %q, want gemini-2.0-flash-001 (modelVersion from chunk)", done.Usage.Model)
	}
}

func TestStreamFunctionCall(t *testing.T) {
	frames := []string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Berlin"}}}],"role":"model"}}]}` + "\n\n",
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":10}}` + "\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model: "gemini-2.0-flash",
		Tools: []providers.ToolSpec{
			{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "weather"}}}},
	})

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
	if toolCall.Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", toolCall.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(toolCall.Input, &args); err != nil {
		t.Fatalf("Input not JSON: %v (raw %s)", err, toolCall.Input)
	}
	if args["city"] != "Berlin" {
		t.Errorf("args = %v, want city=Berlin", args)
	}
	// finishReason was STOP, but pending function calls force the
	// stop_reason remap to tool_use so the loop iterates.
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (function call must override STOP)", stop)
	}
}

// TestRequestShape pins the wire body against Gemini's expected
// schema: contents (with role="model" not "assistant"),
// systemInstruction, tools.functionDeclarations, generationConfig.
// The model name must NOT appear in the body — Gemini reads it from
// the URL path. A regression that smuggled the model into the body
// would make the request 4xx on every Vertex AI gateway.
func TestRequestShape(t *testing.T) {
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, `data: {"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	temp := 0.5
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:       "gemini-2.5-flash",
		Temperature: &temp,
		MaxTokens:   8192,
		Effort:      "high",
		System:      []providers.ContentBlock{{Type: "text", Text: "you are concise"}},
		Tools: []providers.ToolSpec{
			{Name: "calc", Description: "do math", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "hello"}}},
		},
	})
	for range ch {
	}

	body := <-captured
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("body not JSON: %v (raw %s)", err, body)
	}
	// No "model" field in the body.
	if bytes.Contains(body, []byte(`"model":`)) {
		t.Errorf("body carries a model field — Gemini expects model in the URL only:\n%s", body)
	}
	// systemInstruction populated.
	if w.SystemInstruction == nil || len(w.SystemInstruction.Parts) == 0 || w.SystemInstruction.Parts[0].Text != "you are concise" {
		t.Errorf("systemInstruction = %+v, want one text part", w.SystemInstruction)
	}
	// Roles translated: assistant → model.
	if len(w.Contents) != 2 {
		t.Fatalf("contents = %d entries, want 2", len(w.Contents))
	}
	if w.Contents[0].Role != "user" || w.Contents[1].Role != "model" {
		t.Errorf("roles = [%q, %q], want [user, model] (assistant → model translation)", w.Contents[0].Role, w.Contents[1].Role)
	}
	// Tools wrapped in functionDeclarations.
	if len(w.Tools) != 1 || len(w.Tools[0].FunctionDeclarations) != 1 || w.Tools[0].FunctionDeclarations[0].Name != "calc" {
		t.Errorf("tools = %+v, want one functionDeclaration name=calc", w.Tools)
	}
	// generationConfig with thinkingBudget=8192-1024=7168 (effort=high
	// clamped to maxTokens-1024 since 8192 == maxTokens).
	if w.GenerationConfig == nil {
		t.Fatal("generationConfig missing")
	}
	if w.GenerationConfig.ThinkingConfig == nil {
		t.Fatal("thinkingConfig missing for effort=high")
	}
	if w.GenerationConfig.ThinkingConfig.ThinkingBudget != 7168 {
		t.Errorf("thinkingBudget = %d, want 7168 (clamped from 8192 against maxTokens=8192)",
			w.GenerationConfig.ThinkingConfig.ThinkingBudget)
	}
}

// TestEffortBudget pins the effort → thinkingBudget translation.
// Mirrors the same effort vocabulary the loop uses for Anthropic so
// operator semantics stay consistent across providers.
func TestEffortBudget(t *testing.T) {
	cases := []struct {
		effort    string
		maxTokens int
		want      int
	}{
		{"", 0, -1},
		{"low", 0, 0},
		{"medium", 0, 2048},
		{"high", 0, 8192},
		{"high", 16384, 8192},
		{"high", 8192, 7168}, // clamps to maxTokens-1024
		{"high", 1500, 0},    // would clamp below 1024 → drop
		{"medium", 4096, 2048},
		{"medium", 1024, 768}, // clamps to maxTokens-256
		{"medium", 256, 0},    // budget would be 0 after clamp
		{"unknown", 0, -1},
	}
	for _, tc := range cases {
		got := geminiEffortBudget(tc.effort, tc.maxTokens)
		if got != tc.want {
			t.Errorf("geminiEffortBudget(%q, %d) = %d, want %d", tc.effort, tc.maxTokens, got, tc.want)
		}
	}
}

// TestFunctionResponseRoundtrip pins that a tool_result content
// block translates to a functionResponse part with the loomcycle
// text wrapped under {"content": "..."}. Required by Gemini's
// schema — bare strings aren't accepted in functionResponse.response.
func TestFunctionResponseRoundtrip(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "gemini-2.0-flash",
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "do it"}}},
			{Role: "assistant", Content: []providers.ContentBlock{
				{Type: "tool_use", ToolName: "get_weather", ToolUseID: "x", ToolInput: json.RawMessage(`{"city":"Berlin"}`)},
			}},
			{Role: "user", Content: []providers.ContentBlock{
				{Type: "tool_result", ToolName: "get_weather", ToolUseID: "x", Text: "sunny, 22°C"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(w.Contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(w.Contents))
	}
	// 3rd message: user role (loomcycle role) carrying a
	// functionResponse part. Gemini interprets it correctly because
	// of the part shape; role stays "user" on the wire.
	last := w.Contents[2]
	if len(last.Parts) != 1 || last.Parts[0].FunctionResponse == nil {
		t.Fatalf("third content lacks functionResponse: %+v", last)
	}
	fr := last.Parts[0].FunctionResponse
	if fr.Name != "get_weather" {
		t.Errorf("functionResponse.name = %q, want get_weather", fr.Name)
	}
	var resp map[string]string
	if err := json.Unmarshal(fr.Response, &resp); err != nil {
		t.Fatalf("functionResponse.response not JSON: %v (raw %s)", err, fr.Response)
	}
	if resp["content"] != "sunny, 22°C" {
		t.Errorf("functionResponse.response.content = %q, want sunny, 22°C", resp["content"])
	}
}

// --- probe / list models ---

func TestListModels_StripsModelsPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("x-goog-api-key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{"name":"models/gemini-2.5-flash"},
				{"name":"models/gemini-2.0-flash"},
				{"name":"models/gemini-1.5-pro-001"}
			]
		}`))
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"gemini-2.5-flash", "gemini-2.0-flash", "gemini-1.5-pro-001"}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d (got %v)", len(models), len(want), models)
	}
	for i, m := range models {
		if m != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, m, want[i])
		}
	}
}

func TestProbe_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"API key invalid"}}`))
	}))
	defer srv.Close()

	d := New("bad-key", srv.URL, streamhttp.Options{}, nil)
	if err := d.Probe(context.Background()); err == nil {
		t.Fatal("Probe with 401 didn't surface an error")
	}
}
