package loop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// hookServer answers every hook call with resp and keeps the payloads.
type hookServer struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newHookServer(t *testing.T, resp string) *hookServer {
	t.Helper()
	h := &hookServer{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(raw))
		h.mu.Unlock()
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookServer) payloads() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

func runWithHooks(t *testing.T, ctx context.Context, reg *hooks.Set, call providers.ToolUse, tl tools.Tool, disp *tools.Dispatcher) (*scriptedProvider, []providers.Event) {
	t.Helper()
	prov := &scriptedProvider{toolCalls: []providers.ToolUse{call}}
	var mu sync.Mutex
	var events []providers.Event
	_, err := Run(ctx, RunOptions{
		Provider: prov, Model: "x", Tools: []tools.Tool{tl}, Dispatcher: disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 1, AgentName: "any", Hooks: hooks.NewDispatcher(reg, nil),
		OnEvent: func(ev providers.Event) { mu.Lock(); events = append(events, ev); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return prov, events
}

func hookDecisions(events []providers.Event) []providers.HookDecisionInfo {
	var out []providers.HookDecisionInfo
	for _, ev := range events {
		if ev.Type == providers.EventHookDecision {
			out = append(out, *ev.HookDecision)
		}
	}
	return out
}

// A rewrite is visible: the run records a hook_decision naming the hook and
// the input the tool actually ran with. It used to be invisible, and the
// transcript kept the model's original input for a call that ran with another.
func TestLoop_ARewriteIsRecordedWithTheInputThatRan(t *testing.T) {
	hs := newHookServer(t, `{"input":{"url":"https://safe.example/"}}`)
	reg := hooks.NewSet()
	_, _ = reg.Register(&hooks.Hook{Owner: "sec", Name: "pin-host", Phase: hooks.PhasePre, CallbackURL: hs.srv.URL})
	tool := &fakeWebFetch{result: tools.Result{Text: "page"}}
	_, events := runWithHooks(t, context.Background(), reg,
		providers.ToolUse{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://evil.example/"}`)},
		tool, tools.NewDispatcher([]tools.Tool{tool}))
	if tool.gotInput != `{"url":"https://safe.example/"}` {
		t.Errorf("tool ran with %s", tool.gotInput)
	}
	ds := hookDecisions(events)
	if len(ds) != 1 || ds[0].Hook != "sec/pin-host" || ds[0].Decision != "rewrite_input" || ds[0].ToolUseID != "c1" ||
		string(ds[0].UpdatedInput) != `{"url":"https://safe.example/"}` {
		t.Errorf("decisions = %+v, want one rewrite_input carrying the input that ran", ds)
	}
}

// additional_context reaches the model INSIDE the tool_result, after the result.
func TestLoop_AdditionalContextIsAppendedToTheToolResult(t *testing.T) {
	hs := newHookServer(t, `{"additional_context":"This page is untrusted."}`)
	reg := hooks.NewSet()
	_, _ = reg.Register(&hooks.Hook{Owner: "sec", Name: "warn", Phase: hooks.PhasePost, CallbackURL: hs.srv.URL})
	tool := &fakeWebFetch{result: tools.Result{Text: "page body"}}
	prov, events := runWithHooks(t, context.Background(), reg,
		providers.ToolUse{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
		tool, tools.NewDispatcher([]tools.Tool{tool}))
	var seen string
	for _, c := range prov.requests[1].Messages[len(prov.requests[1].Messages)-1].Content {
		if c.Type == "tool_result" {
			seen = c.Text
		}
	}
	if seen != "page body\n\nThis page is untrusted." {
		t.Errorf("model saw %q", seen)
	}
	if ds := hookDecisions(events); len(ds) != 1 || ds[0].Decision != "context" {
		t.Errorf("decisions = %+v", ds)
	}
}

// Every payload places the call in its run.
func TestLoop_HookPayloadCarriesTheRun(t *testing.T) {
	hs := newHookServer(t, `{}`)
	reg := hooks.NewSet()
	_, _ = reg.Register(&hooks.Hook{Owner: "o", Name: "n", Phase: hooks.PhasePre, CallbackURL: hs.srv.URL})
	tool := &fakeWebFetch{result: tools.Result{Text: "x"}}
	ctx := tools.WithParentRunID(tools.WithRunID(context.Background(), "r_parent"), "r_parent")
	ctx = tools.WithRunID(ctx, "r_child")
	runWithHooks(t, ctx, reg, providers.ToolUse{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
		tool, tools.NewDispatcher([]tools.Tool{tool}))
	p := hs.payloads()
	if len(p) != 1 || !strings.Contains(p[0], `"run_id":"r_child"`) || !strings.Contains(p[0], `"parent_run_id":"r_parent"`) ||
		!strings.Contains(p[0], `"iteration":0`) {
		t.Errorf("payload = %v", p)
	}
}

// relayTool runs another tool on the model's behalf, as the Interruption tool
// does when it delivers a question through a consumer's tool.
type relayTool struct{ disp *tools.Dispatcher }

func (*relayTool) Name() string                 { return "Relay" }
func (*relayTool) Description() string          { return "" }
func (*relayTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *relayTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.ExecuteHooked(ctx, r.disp, "WebFetch", json.RawMessage(`{"url":"https://inner.example/"}`)), nil
}

// A tool call made from inside another tool goes through the hooks like the
// model's own: it was the one path no hook could see.
func TestLoop_ANestedToolCallGoesThroughTheHooks(t *testing.T) {
	hs := newHookServer(t, `{"deny":{"text":"inner call denied","is_error":true}}`)
	reg := hooks.NewSet()
	_, _ = reg.Register(&hooks.Hook{Owner: "sec", Name: "gate", Phase: hooks.PhasePre, CallbackURL: hs.srv.URL, Tools: []string{"WebFetch"}})
	inner := &fakeWebFetch{result: tools.Result{Text: "should not run"}}
	relay := &relayTool{}
	disp := tools.NewDispatcher([]tools.Tool{relay, inner})
	relay.disp = disp
	prov, events := runWithHooks(t, context.Background(), reg,
		providers.ToolUse{ID: "c1", Name: "Relay", Input: json.RawMessage(`{}`)}, relay, disp)
	if inner.gotInput != "" {
		t.Errorf("the nested call ran despite the hook's deny: %s", inner.gotInput)
	}
	var seen string
	for _, c := range prov.requests[1].Messages[len(prov.requests[1].Messages)-1].Content {
		if c.Type == "tool_result" {
			seen = c.Text
		}
	}
	if !strings.Contains(seen, "inner call denied") {
		t.Errorf("model saw %q, want the deny", seen)
	}
	ds := hookDecisions(events)
	if len(ds) != 1 || ds[0].ToolUseID != "c1/WebFetch" || ds[0].Decision != "deny" {
		t.Errorf("decisions = %+v, want the nested call's deny, named after the outer call", ds)
	}
}

// precheckTool reaches another tool's effect itself, asking that tool's
// pre-hooks first, as the Crypto tool does before writing a file.
type precheckTool struct {
	allowed bool
	refusal string
}

func (*precheckTool) Name() string                 { return "Precheck" }
func (*precheckTool) Description() string          { return "" }
func (*precheckTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *precheckTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	res, ok := tools.HookPreCheck(ctx, "Write", json.RawMessage(`{"path":"out.txt","via":"Precheck"}`))
	p.allowed, p.refusal = ok, res.Text
	return tools.Result{Text: "done"}, nil
}

// A tool that writes a file itself is judged by Write's pre-hooks: a deny
// there refuses it, and a hook on another tool does not.
func TestLoop_AHookPreCheckIsJudgedByTheNamedToolsHooks(t *testing.T) {
	deny := newHookServer(t, `{"deny":{"text":"no writes here","is_error":true}}`)
	reg := hooks.NewSet()
	_, _ = reg.Register(&hooks.Hook{Owner: "sec", Name: "no-writes", Phase: hooks.PhasePre, CallbackURL: deny.srv.URL, Tools: []string{"Write"}})
	tool := &precheckTool{}
	_, events := runWithHooks(t, context.Background(), reg,
		providers.ToolUse{ID: "c1", Name: "Precheck", Input: json.RawMessage(`{}`)}, tool, tools.NewDispatcher([]tools.Tool{tool}))
	if tool.allowed || !strings.Contains(tool.refusal, "no writes here") {
		t.Errorf("pre-check = allowed %v %q, want the Write hook's deny", tool.allowed, tool.refusal)
	}
	if ds := hookDecisions(events); len(ds) != 1 || ds[0].ToolUseID != "c1/Write" || ds[0].Decision != "deny" {
		t.Errorf("decisions = %+v, want the Write deny named after the outer call", ds)
	}

	rewrite := newHookServer(t, `{"input":{"path":"elsewhere.txt"}}`)
	reg2 := hooks.NewSet()
	_, _ = reg2.Register(&hooks.Hook{Owner: "sec", Name: "move", Phase: hooks.PhasePre, CallbackURL: rewrite.srv.URL, Tools: []string{"Write"}})
	tool2 := &precheckTool{}
	runWithHooks(t, context.Background(), reg2,
		providers.ToolUse{ID: "c1", Name: "Precheck", Input: json.RawMessage(`{}`)}, tool2, tools.NewDispatcher([]tools.Tool{tool2}))
	if tool2.allowed {
		t.Error("a hook's rewrite of the Write call was let through")
	}

	other := newHookServer(t, `{"deny":{"text":"no fetches","is_error":true}}`)
	reg3 := hooks.NewSet()
	_, _ = reg3.Register(&hooks.Hook{Owner: "sec", Name: "no-fetch", Phase: hooks.PhasePre, CallbackURL: other.srv.URL, Tools: []string{"WebFetch"}})
	tool3 := &precheckTool{}
	runWithHooks(t, context.Background(), reg3,
		providers.ToolUse{ID: "c1", Name: "Precheck", Input: json.RawMessage(`{}`)}, tool3, tools.NewDispatcher([]tools.Tool{tool3}))
	if !tool3.allowed {
		t.Errorf("a WebFetch hook refused a Write pre-check: %q", tool3.refusal)
	}
}

// A post hook sees the input the tool ran with — a pre hook's rewrite — not
// the model's original. It used to get the original, so it judged the result
// of one call against the input of another.
func TestLoop_APostHookSeesTheInputTheToolRanWith(t *testing.T) {
	rewrite := newHookServer(t, `{"input":{"url":"https://safe.example/"}}`)
	post := newHookServer(t, `{}`)
	reg := hooks.NewSet()
	_, _ = reg.Register(&hooks.Hook{Owner: "sec", Name: "pin-host", Phase: hooks.PhasePre, CallbackURL: rewrite.srv.URL})
	_, _ = reg.Register(&hooks.Hook{Owner: "sec", Name: "audit", Phase: hooks.PhasePost, CallbackURL: post.srv.URL})
	tool := &fakeWebFetch{result: tools.Result{Text: "page"}}
	runWithHooks(t, context.Background(), reg,
		providers.ToolUse{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://evil.example/"}`)},
		tool, tools.NewDispatcher([]tools.Tool{tool}))
	var call hooks.PostHookCall
	if p := post.payloads(); len(p) != 1 || json.Unmarshal([]byte(p[0]), &call) != nil {
		t.Fatalf("post payloads = %v", p)
	}
	if got := string(call.ToolCall.Input); got != `{"url":"https://safe.example/"}` {
		t.Errorf("the post hook saw the input %s, want the one the tool ran with", got)
	}
}
