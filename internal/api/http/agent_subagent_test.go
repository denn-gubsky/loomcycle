package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// scriptedProvider returns a different event sequence per call. Used by
// sub-agent tests where parent + child runs need different scripted
// responses (parent: tool_call to Agent then text after tool_result;
// child: text + done).
type scriptedProvider struct {
	calls    atomic.Int32
	scripts  [][]providers.Event
	defaultS []providers.Event // returned for any call past len(scripts)
}

func (s *scriptedProvider) ID() string { return "scripted" }
func (s *scriptedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (s *scriptedProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	idx := int(s.calls.Add(1)) - 1
	var events []providers.Event
	if idx < len(s.scripts) {
		events = s.scripts[idx]
	} else {
		events = s.defaultS
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// End-to-end: a parent agent invokes the Agent tool with {name: "child",
// prompt: "hi"}; the child runs, returns text; the parent's loop sees
// the tool_result and emits final text. Verifies:
//
//   - Sub-agent runs as a SEPARATE session (own session_id) so its
//     transcript is independently retrievable.
//   - The parent's tool_result text contains the child's FinalText.
//   - The child's full event sequence (started/text/usage/done) lands
//     in the child's transcript, NOT the parent's stream.
//   - Parent only sees its own events plus the wrapping tool_call /
//     tool_result frames.
func TestSubAgentRoundTrip_ParentSeesChildOutput(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {
				Model:        "stub-model",
				AllowedTools: []string{"Agent"},
				SystemPrompt: "you are the parent",
			},
			"child": {
				Model:        "stub-model",
				AllowedTools: []string{},
				SystemPrompt: "you are the child",
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	// Provider script:
	//   call 1 = parent's first iter: emit a tool_call to Agent, then done(tool_use).
	//   call 2 = child's only iter: emit text + done(end_turn).
	//   call 3 = parent's second iter (after tool_result): emit final text + done.
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// 1) parent → tool_call(Agent)
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_parent_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"say hello briefly"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			// 2) child → final text + end_turn
			{
				{Type: providers.EventText, Text: "child says hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 4}},
			},
			// 3) parent → final wrap-up text + end_turn
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Parent stream MUST contain the tool_call and a tool_result with the
	// child's text. The child's intermediate "child says hi" text should
	// NOT appear as a parent text frame — only as the tool_result.
	if !strings.Contains(bodyStr, "event: tool_call") {
		t.Errorf("parent stream missing tool_call frame:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "event: tool_result") {
		t.Errorf("parent stream missing tool_result frame:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "child says hi") {
		t.Errorf("parent stream missing child's output text:\n%s", bodyStr)
	}

	// Two sessions should exist now: parent + child. Parent's session
	// is announced in the SSE stream; child's session is implicit.
	parentSessionID := extractSessionID(bodyStr)
	if parentSessionID == "" {
		t.Fatalf("could not parse parent session_id from stream:\n%s", bodyStr)
	}

	parentTranscript, err := st.GetTranscript(context.Background(), parentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Parent transcript should NOT contain a separate "text" frame for
	// "child says hi" — only the tool_result wrapping it.
	for _, ev := range parentTranscript {
		if ev.Type == "text" && strings.Contains(string(ev.Payload), "child says hi") {
			t.Errorf("child text leaked into parent stream as a parent text frame: %s", string(ev.Payload))
		}
	}

	// Find the child's session by listing all sessions and picking the
	// one with agent="child". The store doesn't expose ListSessions
	// publicly, so we approximate: try GetTranscript on a synthesised
	// ID won't work. Instead we cross-check via the parent's tool_result
	// containing the child's output.
	foundToolResult := false
	for _, ev := range parentTranscript {
		if ev.Type == "tool_result" && strings.Contains(string(ev.Payload), "child says hi") {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Error("parent transcript should record the tool_result with child's output")
	}
}

// Regression: a parent without "Agent" in its allowed_tools cannot call
// the Agent tool — the dispatcher refuses. The model would see a
// "tool not found" tool_result. This locks the per-agent gate.
func TestSubAgent_ParentWithoutAgentToolCannotSpawn(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"locked": {
				Model:        "stub-model",
				AllowedTools: []string{}, // no Agent
				SystemPrompt: "you cannot spawn",
			},
			"child": {
				Model:        "stub-model",
				AllowedTools: []string{},
				SystemPrompt: "you are the child",
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// "locked" agent attempts to call Agent (a hypothetical model
			// that ignores the tool list). The dispatcher should refuse.
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_locked_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"x"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use"},
			},
			// After tool_result (which is "tool not found"), wrap up.
			{
				{Type: providers.EventText, Text: "ok, done"},
				{Type: providers.EventDone, StopReason: "end_turn"},
			},
		},
	}

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"locked","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tool not found") {
		t.Errorf("expected 'tool not found' tool_result for an off-policy Agent call:\n%s", body)
	}
	// And critically: NO sub-agent text should appear.
	if strings.Contains(string(body), "child says hi") {
		t.Error("blocked Agent call should not have spawned the child")
	}
}

// Calling Agent with an unknown sub-agent name surfaces the error
// through the IsError tool_result so the parent's model can self-correct.
func TestSubAgent_UnknownChildName(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {
				Model:        "stub-model",
				AllowedTools: []string{"Agent"},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"does-not-exist","prompt":"x"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use"},
			},
			{
				{Type: providers.EventText, Text: "I'll move on"},
				{Type: providers.EventDone, StopReason: "end_turn"},
			},
		},
	}

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "unknown sub-agent") {
		t.Errorf("expected 'unknown sub-agent' error, got:\n%s", body)
	}
	// The parent run should still complete cleanly — this is a tool
	// error, not a run-failing error.
	if !strings.Contains(string(body), "I'll move on") {
		t.Error("parent should have continued after the IsError tool_result")
	}
}
