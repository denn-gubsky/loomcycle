package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// TestLLMGateway_NonStreaming — happy path: POST /v1/_llm/chat with a
// simple text exchange returns the aggregated response shape including
// content blocks, stop reason, and usage.
func TestLLMGateway_NonStreaming(t *testing.T) {
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

	body := `{"messages":[{"role":"user","content":"hi"}],"max_tokens":50,"provider":"scripted","model":"scripted-model"}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out llmChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Provider != "scripted" {
		t.Errorf("provider=%q want scripted", out.Provider)
	}
	if out.Model != "scripted-model" {
		t.Errorf("model=%q want scripted-model", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" {
		t.Fatalf("content=%+v; want single text block", out.Content)
	}
	if out.Content[0].Text != "Hello world" {
		t.Errorf("text=%q want %q", out.Content[0].Text, "Hello world")
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q want end_turn", out.StopReason)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 2 {
		t.Errorf("usage=%+v", out.Usage)
	}
	if !strings.HasPrefix(out.RequestID, "req_") {
		t.Errorf("request_id=%q; want req_ prefix", out.RequestID)
	}
	if !strings.HasPrefix(out.ID, "llm_") {
		t.Errorf("id=%q; want llm_ prefix", out.ID)
	}
}

// TestLLMGateway_NonStreaming_ToolCall — assistant returns a tool_use
// content block; verify it surfaces in the response.
func TestLLMGateway_NonStreaming_ToolCall(t *testing.T) {
	input := json.RawMessage(`{"expr":"2+2"}`)
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "call_abc", Name: "calculator", Input: input}},
			{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 5, OutputTokens: 8}, StopReason: "tool_use"},
			{Type: providers.EventDone, StopReason: "tool_use"},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"messages":[{"role":"user","content":"compute 2+2"}],
		"tools":[{"name":"calculator","description":"math","input_schema":{"type":"object"}}],
		"provider":"scripted","model":"scripted-model"
	}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out llmChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content=%+v; want single tool_use block", out.Content)
	}
	if out.Content[0].ID != "call_abc" || out.Content[0].Name != "calculator" {
		t.Errorf("tool_use=%+v", out.Content[0])
	}
	if string(out.Content[0].Input) != `{"expr":"2+2"}` {
		t.Errorf("input=%s", string(out.Content[0].Input))
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason=%q want tool_use", out.StopReason)
	}
}

// TestLLMGateway_Streaming — stream:true emits provider_chosen first,
// then content_block_start/delta/stop, then message_delta + done.
func TestLLMGateway_Streaming(t *testing.T) {
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

	body := `{"messages":[{"role":"user","content":"hi"}],"stream":true,"provider":"scripted","model":"scripted-model"}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
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
	for _, want := range []string{
		"event: provider_chosen",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: done",
		`"provider":"scripted"`,
		`"text":"stream "`,
		`"text":"ok"`,
		`"input_tokens":3`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q in:\n%s", want, got)
		}
	}
}

// TestLLMGateway_Streaming_ToolUseOnly — regression for the
// pre-fix bug where a tool_use-only response (no preceding text)
// landed at index 1 instead of 0, leaving index 0 empty and tripping
// Anthropic-compatible adapter reassembly. Verifies the first tool_use
// block lands at index 0.
func TestLLMGateway_Streaming_ToolUseOnly(t *testing.T) {
	input := json.RawMessage(`{"expr":"2+2"}`)
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "call_y", Name: "calc", Input: input}},
			{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 5, OutputTokens: 8}, StopReason: "tool_use"},
			{Type: providers.EventDone, StopReason: "tool_use"},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"messages":[{"role":"user","content":"calc 2+2"}],"stream":true,"provider":"scripted","model":"scripted-model"}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	got := string(raw)
	// The first content_block_start must carry index 0 — Anthropic
	// adapters reconstruct from contiguous indices starting at 0.
	wantStart := `event: content_block_start
data: {"index":0,"block":{"type":"tool_use","id":"call_y","name":"calc","input":{"expr":"2+2"}}}`
	if !strings.Contains(got, wantStart) {
		t.Errorf("expected tool_use at index 0, got:\n%s", got)
	}
	if !strings.Contains(got, `event: content_block_stop
data: {"index":0}`) {
		t.Errorf("expected content_block_stop at index 0, got:\n%s", got)
	}
}

// TestLLMGateway_TierUnavailable_Returns503 — regression for the
// gatewayResolveStatus bug where ErrTierUnavailable returned 400
// (non-retryable) instead of 503 (retryable). n8n's
// LoomCycleChatModel branches retry on HTTP status code; surfacing
// transient tier-exhaustion as 503 is load-bearing.
func TestLLMGateway_TierUnavailable_Returns503(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	// A resolver constructed with an empty tier map returns
	// ErrTierUnavailable for any tier-driven request — exactly the
	// behaviour the handler must surface as 503.
	srv.SetResolver(resolve.NewResolver(nil, map[string][]resolve.Candidate{}))
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"messages":[{"role":"user","content":"hi"}],"tier":"middle"}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s; want 503", resp.StatusCode, string(raw))
	}
}

// TestLLMGateway_RoutingPrecedence_ExplicitPinSkipsResolver — when
// both provider and model are explicit, the handler skips the
// resolver path entirely. Verified by setting the resolver to nil;
// the call should still succeed because the explicit-pin branch
// short-circuits.
func TestLLMGateway_RoutingPrecedence_ExplicitPinSkipsResolver(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventText, Text: "ok"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		}},
	}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	// Resolver is nil by default in the fixture — confirms explicit
	// pin doesn't reach into the resolver.
	if srv.resolver != nil {
		t.Fatalf("test fixture changed: resolver should be nil by default")
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"messages":[{"role":"user","content":"hi"}],"provider":"scripted","model":"scripted-model"}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
}

// TestLLMGateway_RoutingPrecedence_NoResolverNoPinRefuses — when
// neither explicit pin nor a resolver is configured, the gateway
// refuses with 400 (rather than silently picking a default).
func TestLLMGateway_RoutingPrecedence_NoResolverNoPinRefuses(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s; want 400", resp.StatusCode, string(raw))
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "resolver not configured") {
		t.Errorf("body=%s; want resolver-not-configured message", string(raw))
	}
}

// TestLLMGateway_Validation_EmptyMessages — the handler rejects a
// request with no messages.
func TestLLMGateway_Validation_EmptyMessages(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"messages":[],"provider":"scripted","model":"scripted-model"}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s; want 400", resp.StatusCode, string(raw))
	}
}

// TestLLMGateway_Validation_BadJSON — the handler rejects malformed
// JSON bodies with a 400.
func TestLLMGateway_Validation_BadJSON(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json",
		bytes.NewReader([]byte(`{not valid`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d; want 400", resp.StatusCode)
	}
}

// TestLLMGateway_ToolMessageRoundTrip — a request including a
// role:"tool" message after an assistant tool_use translates correctly
// into a tool_result content block on the providers.Request the driver
// receives. We capture the driver-side request via a recordingProvider.
func TestLLMGateway_ToolMessageRoundTrip(t *testing.T) {
	rec := &recordingProvider{}
	srv, _ := makeServer(t, rec, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{
		"messages":[
			{"role":"user","content":"calc 2+2"},
			{"role":"assistant","tool_calls":[{"id":"call_x","name":"calculator","input":{"expr":"2+2"}}]},
			{"role":"tool","tool_call_id":"call_x","content":"4"}
		],
		"provider":"scripted","model":"scripted-model"
	}`
	resp, err := http.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
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
	if tr.Role != "user" {
		t.Errorf("tool-result message role=%q; want user", tr.Role)
	}
	if len(tr.Content) != 1 || tr.Content[0].Type != "tool_result" {
		t.Fatalf("tool-result content=%+v", tr.Content)
	}
	if tr.Content[0].ToolUseID != "call_x" {
		t.Errorf("tool_use_id=%q want call_x", tr.Content[0].ToolUseID)
	}
	if tr.Content[0].Text != "4" {
		t.Errorf("text=%q want 4", tr.Content[0].Text)
	}
}

// recordingProvider captures the last Request it received so a test
// can assert on the translated shape.
type recordingProvider struct {
	last *providers.Request
}

func (p *recordingProvider) ID() string                    { return "recording" }
func (p *recordingProvider) Probe(_ context.Context) error { return nil }
func (p *recordingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"scripted-model"}, nil
}
func (p *recordingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *recordingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	r := req
	p.last = &r
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

var _ providers.Provider = (*recordingProvider)(nil)
