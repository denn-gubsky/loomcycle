package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestOpenAICompat_NonStreaming_TextHappyPath — drop-in for OpenAI
// SDKs: send a chat.completion request, get a chat.completion
// response with the right object/choices/usage shape.
func TestOpenAICompat_NonStreaming_TextHappyPath(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventText, Text: "Hello "},
			{Type: providers.EventText, Text: "world"},
			{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}, StopReason: "end_turn"},
			{Type: providers.EventDone, StopReason: "end_turn"},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":50,
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out openaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object=%q; want chat.completion", out.Object)
	}
	if len(out.Choices) != 1 || out.Choices[0].Index != 0 {
		t.Fatalf("choices=%+v", out.Choices)
	}
	var content string
	if len(out.Choices[0].Message.Content) > 0 {
		_ = json.Unmarshal(out.Choices[0].Message.Content, &content)
	}
	if content != "Hello world" {
		t.Errorf("content=%q; want %q", content, "Hello world")
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason=%q; want stop", out.Choices[0].FinishReason)
	}
	if out.Usage.PromptTokens != 10 || out.Usage.CompletionTokens != 2 || out.Usage.TotalTokens != 12 {
		t.Errorf("usage=%+v", out.Usage)
	}
	if !strings.HasPrefix(out.ID, "llm_") {
		t.Errorf("id=%q; want llm_ prefix", out.ID)
	}
	if out.Model != "scripted-model" {
		t.Errorf("model=%q", out.Model)
	}
	if out.Created == 0 {
		t.Errorf("created should be a unix timestamp")
	}
}

// TestOpenAICompat_ToolCallResponse — assistant returns a tool_use
// content block; verify it surfaces in OpenAI's tool_calls shape
// (function envelope, arguments as JSON string).
func TestOpenAICompat_ToolCallResponse(t *testing.T) {
	input := json.RawMessage(`{"expr":"2+2"}`)
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "call_x", Name: "calc", Input: input}},
			{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 5, OutputTokens: 8}, StopReason: "tool_use"},
			{Type: providers.EventDone, StopReason: "tool_use"},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[{"role":"user","content":"compute"}],
		"tools":[{"type":"function","function":{"name":"calc","description":"math","parameters":{"type":"object"}}}],
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out openaiChatResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if len(out.Choices) != 1 {
		t.Fatalf("choices=%+v", out.Choices)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason=%q; want tool_calls", out.Choices[0].FinishReason)
	}
	tcs := out.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%+v", tcs)
	}
	if tcs[0].ID != "call_x" || tcs[0].Type != "function" || tcs[0].Function.Name != "calc" {
		t.Errorf("tool_call shape wrong: %+v", tcs[0])
	}
	if tcs[0].Function.Arguments != `{"expr":"2+2"}` {
		t.Errorf("arguments=%q; want JSON string of the input", tcs[0].Function.Arguments)
	}
}

// TestOpenAICompat_StreamingFramesAndDone — stream:true emits
// chat.completion.chunk frames with bare `data:` lines (no event:
// names) terminated by `data: [DONE]`.
func TestOpenAICompat_StreamingFramesAndDone(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventText, Text: "stream "},
			{Type: providers.EventText, Text: "ok"},
			{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 3, OutputTokens: 4}, StopReason: "end_turn"},
			{Type: providers.EventDone, StopReason: "end_turn"},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[{"role":"user","content":"hi"}],
		"stream":true,
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type=%q; want text/event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	got := string(raw)

	// Bare `data:` frames only — NO `event:` lines (OpenAI doesn't
	// use named SSE events; SDKs key off bare data: only).
	if strings.Contains(got, "event:") {
		t.Errorf("stream should not contain event: lines (OpenAI uses bare data:); got:\n%s", got)
	}
	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"role":"assistant"`,
		`"content":"stream "`,
		`"content":"ok"`,
		`"finish_reason":"stop"`,
		`"prompt_tokens":3`,
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q in:\n%s", want, got)
		}
	}
}

// TestOpenAICompat_StreamingToolCall — tool_call in stream becomes
// a chunk with delta.tool_calls[0].function carrying id/name/args.
func TestOpenAICompat_StreamingToolCall(t *testing.T) {
	input := json.RawMessage(`{"expr":"7"}`)
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "call_a", Name: "calc", Input: input}},
			{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 5, OutputTokens: 8}, StopReason: "tool_use"},
			{Type: providers.EventDone, StopReason: "tool_use"},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[{"role":"user","content":"compute"}],
		"stream":true,
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	got := string(raw)
	for _, want := range []string{
		`"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"7\"}"}}]`,
		`"finish_reason":"tool_calls"`,
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q in:\n%s", want, got)
		}
	}
}

// TestOpenAICompat_ToolMessageRoundTrip — request with role:"tool"
// + tool_call_id correlates back to a prior assistant turn's
// tool_calls[].id. The translator should pass it through to the
// provider as a tool_result content block.
func TestOpenAICompat_ToolMessageRoundTrip(t *testing.T) {
	rec := &recordingProvider{}
	srv, _ := makeServer(t, rec, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[
			{"role":"user","content":"calc"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_x","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"2+2\"}"}}]},
			{"role":"tool","tool_call_id":"call_x","content":"4"}
		],
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	if rec.last == nil {
		t.Fatal("provider never invoked")
	}
	if len(rec.last.Messages) != 3 {
		t.Fatalf("messages=%d; want 3", len(rec.last.Messages))
	}
	tr := rec.last.Messages[2]
	if len(tr.Content) != 1 || tr.Content[0].Type != "tool_result" {
		t.Fatalf("tool-result content=%+v", tr.Content)
	}
	if tr.Content[0].ToolUseID != "call_x" {
		t.Errorf("tool_use_id=%q; want call_x", tr.Content[0].ToolUseID)
	}
	if tr.Content[0].Text != "4" {
		t.Errorf("text=%q; want 4", tr.Content[0].Text)
	}
}

// TestOpenAICompat_ContentAsArrayOfTextParts — OpenAI's polymorphic
// content field: [{type:"text", text:"..."}] arrays must flatten
// into a single string the provider sees.
func TestOpenAICompat_ContentAsArrayOfTextParts(t *testing.T) {
	rec := &recordingProvider{}
	srv, _ := makeServer(t, rec, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hello "},
			{"type":"text","text":"world"}
		]}],
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	if rec.last == nil || len(rec.last.Messages) != 1 {
		t.Fatalf("provider request shape unexpected: %+v", rec.last)
	}
	if rec.last.Messages[0].Content[0].Text != "hello world" {
		t.Errorf("text=%q; want %q", rec.last.Messages[0].Content[0].Text, "hello world")
	}
}

// TestOpenAICompat_IgnoredFieldsDontFail — OpenAI consumers
// regularly send presence_penalty / top_p / n / seed / response_format;
// loomcycle accepts the request even though it doesn't apply them.
// Rejecting would break drop-in compatibility.
func TestOpenAICompat_IgnoredFieldsDontFail(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventText, Text: "ok"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"model":"scripted-model",
		"messages":[{"role":"user","content":"hi"}],
		"presence_penalty":0.5,
		"frequency_penalty":0.3,
		"top_p":0.9,
		"n":1,
		"seed":42,
		"response_format":{"type":"text"},
		"loomcycle_provider":"scripted"
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s; ignored OpenAI fields should not cause rejection", resp.StatusCode, string(raw))
	}
}

// TestOpenAICompat_OpenAIUserMapsToLoomcycleUserID — when the
// caller passes OpenAI's `user` field but not loomcycle_user_id, the
// shim maps it onto loomcycle's user_id so per-user quota tracking
// works for drop-in SDK callers.
func TestOpenAICompat_OpenAIUserMapsToLoomcycleUserID(t *testing.T) {
	rec := &recordingProvider{}
	// We can't easily inspect the user_id post-translation through
	// the provider (the user_id lives on the gateway dispatch, not
	// on providers.Request). Instead verify the translation function
	// directly.
	_ = rec
	_ = recordingProvider{}
	o := &openaiChatRequest{
		Model:    "x",
		Messages: []openaiChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		User:     "alice",
	}
	req, err := openaiRequestToLLMChatRequest(o)
	if err != nil {
		t.Fatal(err)
	}
	if req.UserID != "alice" {
		t.Errorf("user_id=%q; want alice (OpenAI's `user` field should map through)", req.UserID)
	}
	// Explicit loomcycle_user_id takes precedence.
	o.LoomcycleUserID = "bob"
	req, _ = openaiRequestToLLMChatRequest(o)
	if req.UserID != "bob" {
		t.Errorf("user_id=%q; want bob (loomcycle_user_id should override OpenAI's user)", req.UserID)
	}
}

// TestOpenAICompat_FinishReasonMapping — every stop_reason the
// provider can emit maps onto the right OpenAI finish_reason.
func TestOpenAICompat_FinishReasonMapping(t *testing.T) {
	cases := []struct{ stop, want string }{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"unknown", "stop"},
	}
	for _, c := range cases {
		got := stopReasonToFinishReason(c.stop)
		if got != c.want {
			t.Errorf("stop_reason=%q → finish_reason=%q; want %q", c.stop, got, c.want)
		}
	}
}

// TestOpenAICompat_AuthRequired — bearer is enforced on the new
// path same as everywhere else.
func TestOpenAICompat_AuthRequired(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "secret"
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
	// No Authorization header → expect 401.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d; want 401", resp.StatusCode)
	}
}

// TestOpenAICompat_ValidationEmptyMessages — empty messages array
// rejected with 400 (matches the native /v1/_llm/chat policy).
func TestOpenAICompat_ValidationEmptyMessages(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"model":"x","messages":[],"loomcycle_provider":"scripted"}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d; want 400", resp.StatusCode)
	}
}
