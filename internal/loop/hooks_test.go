package loop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// fakeWebFetch records its input and returns whatever Result is set
// at construction. Keeps the loop integration test independent of
// the real WebFetch implementation.
type fakeWebFetch struct {
	gotInput string
	result   tools.Result
}

func (t *fakeWebFetch) Name() string                 { return "WebFetch" }
func (t *fakeWebFetch) Description() string          { return "" }
func (t *fakeWebFetch) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *fakeWebFetch) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	t.gotInput = string(input)
	return t.result, nil
}

// TestLoop_HooksWrapTool_PostRewrite is the end-to-end integration:
// a Post-hook wraps the tool result in <untrusted> tags, which is
// the canonical use case for the seam (the v0.2 untrusted-content
// wrap pattern). Demonstrates that hookDispatcher threading through
// the loop's dispatchOneTool actually changes what the model sees on
// the next turn.
func TestLoop_HooksWrapTool_PostRewrite(t *testing.T) {
	// Set up a webhook that wraps every result in <untrusted>...</untrusted>.
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call hooks.PostHookCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		out := hooks.PostHookResult{
			Result: &hooks.ToolResult{
				Text:    "<untrusted>" + call.ToolResult.Text + "</untrusted>",
				IsError: call.ToolResult.IsError,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer hookSrv.Close()

	reg := hooks.NewRegistry()
	if _, err := reg.Register(&hooks.Hook{
		Owner: "test", Name: "wrap-untrusted", Phase: hooks.PhasePost,
		CallbackURL: hookSrv.URL,
		Agents:      []string{"company-researcher"},
		Tools:       []string{"WebFetch"},
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}
	dispatcher := hooks.NewDispatcher(reg, nil)

	// Provider scripts: iter 1 emits a WebFetch tool_call; iter 2
	// terminates. The TOOL gets the original input but the loop hands
	// the model a wrapped tool_result on iter 2. We assert by
	// inspecting the request the provider received on iter 2.
	prov := &scriptedProvider{toolCalls: []providers.ToolUse{
		{ID: "call_1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com"}`)},
	}}
	tool := &fakeWebFetch{result: tools.Result{Text: "page body bytes"}}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	_, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 4,
		AgentName:       "company-researcher",
		Hooks:           dispatcher,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The TOOL itself saw the original (un-rewritten) input.
	if !strings.Contains(tool.gotInput, "https://example.com") {
		t.Errorf("tool input = %q, want unchanged original URL", tool.gotInput)
	}

	// The MODEL saw the wrapped result on iter 2's request.
	if len(prov.requests) < 2 {
		t.Fatalf("expected 2 turns, got %d", len(prov.requests))
	}
	turn2 := prov.requests[1]
	var toolResultText string
	for _, msg := range turn2.Messages {
		for _, c := range msg.Content {
			if c.Type == "tool_result" {
				toolResultText = c.Text
			}
		}
	}
	if !strings.Contains(toolResultText, "<untrusted>page body bytes</untrusted>") {
		t.Errorf("model saw tool_result = %q, want <untrusted>...</untrusted> wrap", toolResultText)
	}
}

// TestLoop_HooksAgentFilter_Mismatch pins that a hook with a
// different agent selector does NOT fire. Defensive against the
// regression where the agent name accidentally falls out of scope
// in dispatchOneTool.
func TestLoop_HooksAgentFilter_Mismatch(t *testing.T) {
	hookCalls := 0
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookCalls++
		w.WriteHeader(204)
	}))
	defer hookSrv.Close()

	reg := hooks.NewRegistry()
	_, _ = reg.Register(&hooks.Hook{
		Owner: "test", Name: "qa-only", Phase: hooks.PhasePost,
		CallbackURL: hookSrv.URL,
		Agents:      []string{"qa-agent"},
		Tools:       []string{"WebFetch"},
	})

	prov := &scriptedProvider{toolCalls: []providers.ToolUse{
		{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	}}
	tool := &fakeWebFetch{result: tools.Result{Text: "ok"}}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	_, _ = Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 4,
		AgentName:       "company-researcher", // hook is qa-agent only
		Hooks:           hooks.NewDispatcher(reg, nil),
	})
	if hookCalls != 0 {
		t.Errorf("hook called %d times for non-matching agent; expected 0", hookCalls)
	}
}

// TestLoop_HooksPreDeny pins that a Pre-hook denial short-circuits
// the actual tool execution: the tool's Execute is never called, the
// model sees the synthetic result.
func TestLoop_HooksPreDeny(t *testing.T) {
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := hooks.PreHookResult{
			Deny: &hooks.ToolResult{IsError: true, Text: "denied: target host not on allowlist"},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer hookSrv.Close()

	reg := hooks.NewRegistry()
	_, _ = reg.Register(&hooks.Hook{
		Owner: "test", Name: "host-allow", Phase: hooks.PhasePre,
		CallbackURL: hookSrv.URL, Tools: []string{"WebFetch"},
	})

	prov := &scriptedProvider{toolCalls: []providers.ToolUse{
		{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://blocked.example/"}`)},
	}}
	tool := &fakeWebFetch{result: tools.Result{Text: "should never be reached"}}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	_, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 4,
		AgentName:       "any",
		Hooks:           hooks.NewDispatcher(reg, nil),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if tool.gotInput != "" {
		t.Errorf("tool was invoked despite deny; gotInput=%q (Pre deny must short-circuit)", tool.gotInput)
	}

	// Confirm the model saw the denial, not the original tool's text.
	if len(prov.requests) < 2 {
		t.Fatalf("expected 2 turns, got %d", len(prov.requests))
	}
	var seen string
	for _, c := range prov.requests[1].Messages[len(prov.requests[1].Messages)-1].Content {
		if c.Type == "tool_result" {
			seen = c.Text
		}
	}
	if !strings.Contains(seen, "denied") {
		t.Errorf("model saw tool_result = %q, want deny synthetic text", seen)
	}
}
