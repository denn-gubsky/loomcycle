package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// echoTool is a stand-in action tool: it returns a fixed observation and counts
// its calls (so a test can assert the stateful loop actually dispatched actions).
type echoTool struct {
	mu       sync.Mutex
	calls    int
	reply    string
	sawState map[string]any // the live Σ this tool observed on its last call
}

func (e *echoTool) Name() string                 { return "Echo" }
func (e *echoTool) Description() string          { return "echoes a fixed observation" }
func (e *echoTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e *echoTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	e.mu.Lock()
	e.calls++
	if h := tools.ExecutionState(ctx); h != nil {
		e.sawState = h.Sigma // the live Σ (what Context op=state would read)
	}
	e.mu.Unlock()
	return tools.Result{Text: e.reply}, nil
}
func (e *echoTool) callCount() int { e.mu.Lock(); defer e.mu.Unlock(); return e.calls }

// statefulScriptProvider returns a scripted emit_state tool_use per Call and
// records each call's fed Messages, so a test can assert the prompt stays flat.
type statefulScriptProvider struct {
	mu       sync.Mutex
	scripts  []string
	turn     int
	requests [][]providers.Message
}

func (p *statefulScriptProvider) ID() string                                   { return "stateful-script" }
func (p *statefulScriptProvider) Probe(context.Context) error                  { return nil }
func (p *statefulScriptProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *statefulScriptProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *statefulScriptProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req.Messages)
	i := p.turn
	p.turn++
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if i < len(p.scripts) {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: fmt.Sprintf("t%d", i), Name: emitStateToolName, Input: json.RawMessage(p.scripts[i])}}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 5}}
	close(ch)
	return ch, nil
}
func (p *statefulScriptProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.turn }

func statefulCtx(schema map[string]any) *config.Context {
	m := config.ContextModeStateful
	c := &config.Context{Mode: &m}
	c.StateSchema = schema
	return c
}

func statefulTaskSegs() []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "count to 2"}}}}
}

func TestContextStatefulMode(t *testing.T) {
	if contextStatefulMode(nil) || contextStatefulMode(&config.Context{}) {
		t.Error("nil / empty context is not stateful")
	}
	if contextStatefulMode(recapMode(-1, "")) {
		t.Error("recap mode is not stateful")
	}
	if !contextStatefulMode(statefulCtx(nil)) {
		t.Error("mode=stateful should be stateful")
	}
}

// The core: a stateful run evolves Σ through patches, dispatches each named
// action to produce the next observation, finishes on done, and returns the final
// Σ + answer. Crucially the fed prompt is FLAT — exactly one message (Σ + O) each
// step, never a growing history.
func TestRun_Stateful_EvolvesStateAndDispatches(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"reasoning":"begin","patch":{"count":0},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":2},"done":true,"final":"count is 2"}`,
	}}
	echo := &echoTool{reply: "observed"}
	var states []map[string]any
	var finalText string
	opts := RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent: func(ev providers.Event) {
			switch ev.Type {
			case providers.EventContextState:
				states = append(states, ev.ContextState.State)
			case providers.EventText:
				finalText += ev.Text
			}
		},
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop reason = %q, want end_turn", res.StopReason)
	}
	if res.FinalText != "count is 2" {
		t.Errorf("final text = %q, want %q", res.FinalText, "count is 2")
	}
	if c, _ := res.State["count"].(float64); c != 2 {
		t.Errorf("final Σ count = %v, want 2", res.State["count"])
	}
	if len(states) != 3 {
		t.Fatalf("got %d context_state markers, want 3", len(states))
	}
	if c, _ := states[0]["count"].(float64); c != 0 {
		t.Errorf("step 0 Σ count = %v, want 0", states[0]["count"])
	}
	if echo.callCount() != 2 {
		t.Errorf("Echo dispatched %d times, want 2 (steps 0 and 1)", echo.callCount())
	}
	// FLAT prompt: every step fed exactly one message (Σ + O), never a history.
	for i, msgs := range prov.requests {
		if len(msgs) != 1 {
			t.Errorf("step %d fed %d messages, want 1 (flat Σ+O)", i, len(msgs))
		}
		body := msgs[0].Content[0].Text
		if !strings.Contains(body, "Current state") || !strings.Contains(body, "observation") {
			t.Errorf("step %d message is not the Σ+O shape: %q", i, body)
		}
	}
}

// An invalid patch is rolled back and retried: the model is shown its rejected
// emit_state + the reason and re-emits, within max_patch_retries.
func TestRun_Stateful_InvalidPatchRetries(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"count": map[string]any{"type": "integer"}},
	}
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":"not-an-int"}}`,               // rejected (string for integer)
		`{"patch":{"count":0},"done":true,"final":"ok"}`, // corrected + done
	}}
	cx := statefulCtx(schema)
	cx.MaxPatchRetries = cptr(1)
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    cx,
	})
	if err != nil {
		t.Fatalf("Run should have recovered via retry: %v", err)
	}
	if res.StopReason != "end_turn" || res.FinalText != "ok" {
		t.Errorf("after retry: stop=%q final=%q, want end_turn/ok", res.StopReason, res.FinalText)
	}
	if prov.calls() != 2 {
		t.Errorf("provider called %d times, want 2 (reject + corrected)", prov.calls())
	}
	if c, _ := res.State["count"].(float64); c != 0 {
		t.Errorf("Σ count = %v, want the corrected 0", res.State["count"])
	}
}

// on_invalid_patch=fail ends the run on the first invalid patch (no retry).
func TestRun_Stateful_InvalidPatchFail(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"count": map[string]any{"type": "integer"}},
	}
	prov := &statefulScriptProvider{scripts: []string{`{"patch":{"count":"bad"}}`}}
	cx := statefulCtx(schema)
	fail := "fail"
	cx.OnInvalidPatch = &fail
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    cx,
	})
	if err == nil {
		t.Fatal("on_invalid_patch=fail must return an error on an invalid patch")
	}
	if res.StopReason != "invalid_patch" {
		t.Errorf("stop reason = %q, want invalid_patch", res.StopReason)
	}
	if prov.calls() != 1 {
		t.Errorf("provider called %d times, want 1 (no retry on fail)", prov.calls())
	}
}

// max_iterations bounds a stateful run that never finishes.
func TestRun_Stateful_MaxIterations(t *testing.T) {
	// A script that always emits a valid patch + an action, never done.
	loopStep := `{"patch":{"n":1},"action":{"tool":"Echo","input":{}}}`
	scripts := make([]string, 10)
	for i := range scripts {
		scripts[i] = loopStep
	}
	prov := &statefulScriptProvider{scripts: scripts}
	echo := &echoTool{reply: "again"}
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:         []tools.Tool{echo},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{echo}),
		Segments:      statefulTaskSegs(),
		Context:       statefulCtx(nil),
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "max_iterations" {
		t.Errorf("stop reason = %q, want max_iterations", res.StopReason)
	}
	if res.Iterations != 3 {
		t.Errorf("iterations = %d, want 3", res.Iterations)
	}
}

func TestParseEmitState(t *testing.T) {
	es, err := parseEmitState(json.RawMessage(`{"reasoning":"r","patch":{"a":1},"action":{"tool":"Echo","input":{"x":2}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if es.Reasoning != "r" || es.Action == nil || es.Action.Tool != "Echo" {
		t.Errorf("parsed wrong: %+v", es)
	}
	if v, _ := es.Patch["a"].(float64); v != 1 {
		t.Errorf("patch a = %v, want 1", es.Patch["a"])
	}
	// A missing patch defaults to an empty (non-nil) map — a no-change step is legal.
	es2, err := parseEmitState(json.RawMessage(`{"done":true,"final":"x"}`))
	if err != nil || es2.Patch == nil {
		t.Errorf("missing patch should default to empty map, got %+v err=%v", es2, err)
	}
	// Malformed JSON errors.
	if _, err := parseEmitState(json.RawMessage(`{not json`)); err == nil {
		t.Error("malformed emit_state should error")
	}
}

func TestStatefulUserMessage_OnlySigmaAndObs(t *testing.T) {
	m := statefulUserMessage(map[string]any{"count": 3}, "the latest thing")
	if m.Role != "user" || len(m.Content) != 1 {
		t.Fatalf("want one user content block, got %+v", m)
	}
	body := m.Content[0].Text
	if !strings.Contains(body, `"count":3`) {
		t.Errorf("Σ not rendered: %q", body)
	}
	if !strings.Contains(body, "the latest thing") {
		t.Errorf("observation not rendered: %q", body)
	}
}

// The dispatched action sees the LIVE Σ via ctx (the data path Context op=state
// relies on): after step 0's patch merges count=5, the Echo action observes
// Σ={count:5}.
func TestRun_Stateful_ActionSeesLiveState(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":5},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{},"done":true,"final":"fin"}`,
	}}
	echo := &echoTool{reply: "ok"}
	_, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	echo.mu.Lock()
	saw := echo.sawState
	echo.mu.Unlock()
	if c, _ := saw["count"].(float64); c != 5 {
		t.Errorf("action saw Σ count = %v, want the live 5", saw["count"])
	}
}
