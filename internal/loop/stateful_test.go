package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
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

// The model can PROPOSE a state_schema (RFC CR model-proposed→adopted): it is
// recorded on the transcript marker + returned in RunResult, but is INERT (does
// not change validation this run). Surfaced only when it differs from the active
// schema.
func TestRun_Stateful_ProposeSchema(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":1},"propose_schema":{"type":"object","properties":{"count":{"type":"integer"}}},"done":true,"final":"done"}`,
	}}
	var markerProposed map[string]any
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil), // no active schema
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextState && ev.ContextState.ProposedSchema != nil {
				markerProposed = ev.ContextState.ProposedSchema
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ProposedSchema == nil {
		t.Fatal("RunResult.ProposedSchema not set")
	}
	if _, ok := res.ProposedSchema["properties"]; !ok {
		t.Errorf("proposed schema missing properties: %v", res.ProposedSchema)
	}
	if markerProposed == nil {
		t.Error("the context_state marker did not carry the proposed schema (no transcript audit)")
	}
	// Inert: the proposal did NOT become the active schema (a wrong-typed patch
	// would still have been accepted this run since no schema was active — proven
	// by the run completing without a validation error, which it did).
}

// A proposal that merely restates the ALREADY-ADOPTED schema is suppressed (not
// surfaced as noise).
func TestRun_Stateful_ProposeSchema_DedupWhenSame(t *testing.T) {
	active := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}}
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"n":1},"propose_schema":{"type":"object","properties":{"n":{"type":"integer"}}},"done":true,"final":"done"}`,
	}}
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(active),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ProposedSchema != nil {
		t.Errorf("a proposal identical to the active schema must be suppressed, got %v", res.ProposedSchema)
	}
}

func TestSchemasDiffer(t *testing.T) {
	s := func(j string) map[string]any {
		var m map[string]any
		_ = json.Unmarshal([]byte(j), &m)
		return m
	}
	if schemasDiffer(s(`{"type":"object","properties":{"a":1}}`), s(`{"properties":{"a":1},"type":"object"}`)) {
		t.Error("key order must not matter (json.Marshal sorts keys)")
	}
	if !schemasDiffer(s(`{"type":"object"}`), nil) {
		t.Error("a non-empty proposal differs from a nil active schema")
	}
	if !schemasDiffer(s(`{"type":"object"}`), s(`{"type":"array"}`)) {
		t.Error("different schemas must differ")
	}
}

// proseThenComplyProvider answers the first `proseTurns` calls the way a local
// model answers a conversational question — with prose and no tool call — then
// complies. It records every request's messages so a test can see what the
// retry actually fed back.
type proseThenComplyProvider struct {
	mu         sync.Mutex
	turn       int
	proseTurns int
	prose      string
	thinking   string
	script     string
	requests   [][]providers.Message
}

func (p *proseThenComplyProvider) ID() string                                   { return "prose-then-comply" }
func (p *proseThenComplyProvider) Probe(context.Context) error                  { return nil }
func (p *proseThenComplyProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *proseThenComplyProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *proseThenComplyProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req.Messages)
	i := p.turn
	p.turn++
	p.mu.Unlock()

	ch := make(chan providers.Event, 3)
	if i < p.proseTurns {
		if p.prose != "" {
			ch <- providers.Event{Type: providers.EventText, Text: p.prose}
		}
		if p.thinking != "" {
			ch <- providers.Event{Type: providers.EventThinking, Text: p.thinking}
		}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
			Usage: &providers.Usage{InputTokens: 4287, OutputTokens: 557}}
	} else {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "t", Name: emitStateToolName, Input: json.RawMessage(p.script)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use",
			Usage: &providers.Usage{InputTokens: 4300, OutputTokens: 60}}
	}
	close(ch)
	return ch, nil
}
func (p *proseThenComplyProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.turn }
func (p *proseThenComplyProvider) reqs() [][]providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]providers.Message(nil), p.requests...)
}

func statefulRun(t *testing.T, prov providers.Provider, cx *config.Context) (RunResult, error, []providers.Event) {
	t.Helper()
	var evs []providers.Event
	var mu sync.Mutex
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Segments: []PromptSegment{{Role: "user",
			Content: []PromptContentBlock{{Type: "trusted-text", Text: "Hello. What can you do?"}}}},
		Context: cx,
		OnEvent: func(ev providers.Event) { mu.Lock(); evs = append(evs, ev); mu.Unlock() },
	})
	mu.Lock()
	defer mu.Unlock()
	return res, err, append([]providers.Event(nil), evs...)
}

// ⚠️ A MODEL THAT ANSWERED THE WRONG WAY IS NOT A BROKEN PROVIDER.
//
// `on_invalid_patch` / `max_patch_retries` covered a patch that ARRIVED and
// failed validation. A model that replied in prose — the ordinary behaviour of
// a local model handed "Hello. What can you do?" — got no retry at all: the run
// died on step zero, 557 output tokens were discarded unread, and the operator
// was told only "model did not call emit_state".
//
// Both are the same condition from the runtime's side: the output was not
// usable and the model is the one who can fix it.
func TestRun_Stateful_AMissingEmitStateIsRetried(t *testing.T) {
	prov := &proseThenComplyProvider{
		proseTurns: 1,
		prose:      "Hello! I can search the web, read files and answer questions.",
		script:     `{"patch":{"note":"greeted"},"done":true,"final":"hi"}`,
	}
	res, err, _ := statefulRun(t, prov, statefulCtx(nil))
	if err != nil {
		t.Fatalf("a prose first turn killed the run: %v", err)
	}
	if prov.calls() != 2 {
		t.Errorf("provider called %d time(s), want 2 — the missing tool call was not retried", prov.calls())
	}
	if res.FinalText != "hi" {
		t.Errorf("final = %q, want %q", res.FinalText, "hi")
	}

	// The retry must SHOW the model what it said, or it has no idea what to
	// correct — the whole reason the text is no longer discarded.
	second := prov.reqs()[1]
	var sawAssistant, sawInstruction bool
	for _, m := range second {
		for _, c := range m.Content {
			if m.Role == "assistant" && strings.Contains(c.Text, "I can search the web") {
				sawAssistant = true
			}
			if m.Role == "user" && strings.Contains(c.Text, "`emit_state` tool call") {
				sawInstruction = true
			}
		}
	}
	if !sawAssistant {
		t.Errorf("the retry did not feed back the model's own reply: %+v", second)
	}
	if !sawInstruction {
		t.Errorf("the retry did not say what was required: %+v", second)
	}
}

// The budget is respected and the terminal error is diagnosable. Before this,
// the operator got "model did not call emit_state" and nothing else — not how
// many attempts, not what the model produced instead, not what to change.
func TestRun_Stateful_AMissingEmitStateExhaustsTheBudgetAndSaysWhatItGot(t *testing.T) {
	prov := &proseThenComplyProvider{
		proseTurns: 99, // never complies
		prose:      "Hello! I can search the web, read files and answer questions.",
		script:     `{"done":true}`,
	}
	cx := statefulCtx(nil)
	two := 2
	cx.MaxPatchRetries = &two
	_, err, evs := statefulRun(t, prov, cx)
	if err == nil {
		t.Fatal("a model that never calls emit_state must still fail the run")
	}
	if prov.calls() != 3 {
		t.Errorf("provider called %d time(s), want 3 (1 + max_patch_retries 2)", prov.calls())
	}
	var errText string
	for _, ev := range evs {
		if ev.Type == providers.EventError {
			errText = ev.Error
		}
	}
	for _, want := range []string{"after 3 attempt(s)", "prose instead", "I can search the web", "max_patch_retries"} {
		if !strings.Contains(errText, want) {
			t.Errorf("the terminal error does not mention %q — an operator cannot act on it:\n%s", want, errText)
		}
	}
}

// ⚠️ AN EMPTY ASSISTANT TURN 400s THE NEXT CALL, and a thinking model's whole
// output lands somewhere the text accumulator never looked — the same shape as
// the silent recap failure. So a reasoning-only reply must be REPORTED and
// never REPLAYED.
func TestRun_Stateful_AThinkingOnlyReplyIsNotReplayedAsAnEmptyTurn(t *testing.T) {
	prov := &proseThenComplyProvider{
		proseTurns: 1,
		thinking:   "the user greeted me, I should introduce myself",
		script:     `{"patch":{"n":1},"done":true,"final":"ok"}`,
	}
	if _, err, _ := statefulRun(t, prov, statefulCtx(nil)); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for _, m := range prov.reqs()[1] {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.Content {
			if strings.TrimSpace(c.Text) == "" {
				t.Errorf("an EMPTY assistant turn was replayed — the next provider call 400s on it")
			}
			if strings.Contains(c.Text, "I should introduce myself") {
				t.Errorf("a reasoning trace was replayed as an assistant turn; it is not one")
			}
		}
	}

	// And a reasoning-only reply must still be distinguishable from silence.
	prov2 := &proseThenComplyProvider{proseTurns: 99, thinking: "thinking hard", script: `{}`}
	cx := statefulCtx(nil)
	zero := 0
	cx.MaxPatchRetries = &zero
	_, err, evs := statefulRun(t, prov2, cx)
	if err == nil {
		t.Fatal("expected failure")
	}
	var errText string
	for _, ev := range evs {
		if ev.Type == providers.EventError {
			errText = ev.Error
		}
	}
	if !strings.Contains(errText, "only a reasoning trace") {
		t.Errorf("a thinking-only reply reads as silence:\n%s", errText)
	}
}

// toolChoiceRecordingProvider captures what the loop asked for on the wire.
type toolChoiceRecordingProvider struct {
	mu      sync.Mutex
	choices []providers.ToolChoice
	script  string
}

func (p *toolChoiceRecordingProvider) ID() string                                   { return "tc-rec" }
func (p *toolChoiceRecordingProvider) Probe(context.Context) error                  { return nil }
func (p *toolChoiceRecordingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *toolChoiceRecordingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsToolChoice: true}
}
func (p *toolChoiceRecordingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.choices = append(p.choices, req.ToolChoice)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "t", Name: emitStateToolName, Input: json.RawMessage(p.script)}}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// THE CROSSING (RFC DG). The drivers' own tests prove each maps ToolChoice onto
// its wire; this proves the stateful loop actually ASKS. A mapping nothing sets
// is as inert as no mapping at all, and that gap is invisible from either side.
func TestRun_Stateful_AsksForTheEmitStateToolOnTheWire(t *testing.T) {
	prov := &toolChoiceRecordingProvider{script: `{"patch":{"n":1},"done":true,"final":"ok"}`}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Segments: []PromptSegment{{Role: "user",
			Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		Context: statefulCtx(nil),
		OnEvent: func(providers.Event) {},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.choices) == 0 {
		t.Fatal("provider was never called")
	}
	want := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: emitStateToolName}
	if prov.choices[0] != want {
		t.Errorf("first step asked for %+v, want %+v — the emit_state contract is still "+
			"prompt-only on the wire", prov.choices[0], want)
	}
}

// ⚠️ "THE MODEL IGNORED THE CONSTRAINT" AND "THERE WAS NO CONSTRAINT" READ
// IDENTICALLY WITHOUT THIS, and they call for opposite next moves: replace the
// model, or move the agent to a provider that has a tool_choice at all. Ollama
// has none, so a forced request there degrades silently by design — this is the
// line that stops the degradation being invisible when it finally costs a run.
func TestRun_Stateful_TheErrorSaysWhenTheToolCouldNotBeForced(t *testing.T) {
	for _, tc := range []struct {
		name       string
		forceable  bool
		wantInText bool
	}{
		{"a provider that cannot force says so", false, true},
		{"a provider that can force stays quiet", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &unforceableProvider{canForce: tc.forceable}
			cx := statefulCtx(nil)
			zero := 0
			cx.MaxPatchRetries = &zero
			_, err, evs := statefulRun(t, prov, cx)
			if err == nil {
				t.Fatal("expected the run to fail")
			}
			var errText string
			for _, ev := range evs {
				if ev.Type == providers.EventError {
					errText = ev.Error
				}
			}
			got := strings.Contains(errText, "NOT enforced")
			if got != tc.wantInText {
				t.Errorf("mentions the unenforced call = %v, want %v:\n%s", got, tc.wantInText, errText)
			}
		})
	}
}

// unforceableProvider never calls emit_state, and reports whether it has a
// tool_choice on the wire.
type unforceableProvider struct{ canForce bool }

func (p *unforceableProvider) ID() string {
	if p.canForce {
		return "openai"
	}
	return "ollama-local"
}
func (p *unforceableProvider) Probe(context.Context) error                  { return nil }
func (p *unforceableProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *unforceableProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsToolChoice: p.canForce}
}
func (p *unforceableProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "Hello! I can help with that."}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// actionScriptProvider emits a scripted emit_state per step and records the
// observation it was fed on the following one, so a test can read what the
// runtime told the model after a bad action.
type actionScriptProvider struct {
	mu       sync.Mutex
	scripts  []string
	turn     int
	observed []string
}

func (p *actionScriptProvider) ID() string                                   { return "action-script" }
func (p *actionScriptProvider) Probe(context.Context) error                  { return nil }
func (p *actionScriptProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *actionScriptProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *actionScriptProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		for _, c := range m.Content {
			p.observed = append(p.observed, c.Text)
		}
	}
	i := p.turn
	p.turn++
	p.mu.Unlock()

	ch := make(chan providers.Event, 2)
	if i < len(p.scripts) {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: fmt.Sprintf("t%d", i), Name: emitStateToolName,
				Input: json.RawMessage(p.scripts[i])}}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}
func (p *actionScriptProvider) sawObservation(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range p.observed {
		if strings.Contains(o, sub) {
			return true
		}
	}
	return false
}

// ⚠️ THE ACTION NAME WENT STRAIGHT TO THE DISPATCHER, UNCHECKED — and the worst
// case of that is the model naming `emit_state`, which is not a tool at all but
// the channel it is already speaking through. Reported from a live chat: the
// operator saw `emit_state` / input {} / "tool not found: emit_state", an answer
// that says the name was wrong and not one word about which names are right.
func TestRun_Stateful_NamingEmitStateAsAnActionIsExplained(t *testing.T) {
	echo := &echoTool{reply: "observed"}
	prov := &actionScriptProvider{scripts: []string{
		`{"reasoning":"confused","patch":{},"action":{"tool":"emit_state","input":{}}}`,
		`{"patch":{"n":1},"done":true,"final":"done"}`,
	}}
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent:    func(providers.Event) {},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop = %q; a bad action must not end the run", res.StopReason)
	}
	// The dispatcher must never have been asked — emit_state is not a tool, and
	// "tool not found" is the answer this change exists to stop producing.
	if echo.callCount() != 0 {
		t.Errorf("a real tool ran for a bogus action name")
	}
	if !prov.sawObservation("is not an action") {
		t.Errorf("the model was not told what emit_state is; observations: %v", prov.observed)
	}
	if !prov.sawObservation("`Echo`") {
		t.Errorf("the correction does not name the tools the agent MAY use — an error that " +
			"says only what was wrong leaves the model guessing")
	}
}

// The general case: any name the agent was not offered is refused HERE, where
// the offered set is known, rather than by a dispatcher that knows every tool
// in the process and so can only say "not found".
func TestRun_Stateful_AnUnofferedActionNamesTheAlternatives(t *testing.T) {
	echo := &echoTool{reply: "observed"}
	prov := &actionScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Bash","input":{}}}`,
		`{"patch":{"n":1},"done":true,"final":"done"}`,
	}}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent:    func(providers.Event) {},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if echo.callCount() != 0 {
		t.Errorf("an unoffered action reached the dispatcher")
	}
	if !prov.sawObservation("no tool named `Bash`") || !prov.sawObservation("`Echo`") {
		t.Errorf("the refusal does not name the bad tool AND the available ones: %v", prov.observed)
	}
}

// Non-vacuity for both: a VALID action still dispatches. A guard that refused
// everything would pass the two tests above and break the loop entirely.
func TestRun_Stateful_AValidActionStillRuns(t *testing.T) {
	echo := &echoTool{reply: "observed"}
	prov := &actionScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"n":1},"done":true,"final":"done"}`,
	}}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent:    func(providers.Event) {},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if echo.callCount() != 1 {
		t.Errorf("a valid action ran %d time(s), want 1 — the guard refuses everything", echo.callCount())
	}
}

// statefulInteractive runs a stateful agent with a steer queue, feeding the
// scripted emit_state replies in order. Returns once the run ends.
func statefulInteractive(t *testing.T, prov providers.Provider, q chan steer.Message,
	park chan struct{}, extra func(*RunOptions)) (RunResult, error) {
	t.Helper()
	opts := RunOptions{
		Provider: prov, Model: "x",
		Segments:    statefulTaskSegs(),
		Context:     statefulCtx(nil),
		Interactive: true,
		SteerQueue:  q,
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventAwaitingInput {
				select {
				case park <- struct{}{}:
				default:
				}
			}
		},
	}
	if extra != nil {
		extra(&opts)
	}
	return Run(context.Background(), opts)
}

// ⚠️ THE REPORTED BUG, AND THE ASSERTION THAT MATTERS IS Σ — not that a second
// answer arrived. runStateful reached the model's `done` and RETURNED, so a
// terminal chat stopped after every answer whatever `interactive` said.
//
// A loop that parked but restarted from an empty Σ would pass "two answers
// arrived" and be useless: the whole point of carrying state across the park is
// that turn 2 knows what turn 1 established. So this reads the FED PROMPT.
func TestRun_Stateful_ParksAndCarriesStateAcrossTheOperatorTurn(t *testing.T) {
	prov := &actionScriptProvider{scripts: []string{
		`{"reasoning":"first","patch":{"topic":"DDRAM"},"done":true,"final":"answer one"}`,
		`{"reasoning":"second","patch":{"followup":"yes"},"done":true,"final":"answer two"}`,
	}}
	q := make(chan steer.Message, 4)
	park := make(chan struct{}, 8)

	done := make(chan RunResult, 1)
	go func() {
		res, err := statefulInteractive(t, prov, q, park, nil)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()

	waitPark := func(what string) {
		t.Helper()
		select {
		case <-park:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the run never parked — it ended after its answer", what)
		}
	}
	waitPark("first turn")
	q <- steer.Message{Text: "and what about refresh cycles?"}
	waitPark("second turn")
	close(q) // queue closed → the run ends

	select {
	case res := <-done:
		if res.StopReason != "end_turn" {
			t.Errorf("stop = %q, want end_turn", res.StopReason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not end after the queue closed")
	}

	// Σ FROM TURN ONE MUST BE IN TURN TWO'S PROMPT. This is the assertion a
	// park-that-forgets would fail and "two answers arrived" would not.
	if !prov.sawObservation(`"topic":"DDRAM"`) {
		t.Errorf("turn 2 was fed a state without turn 1's patch — the park restarted from an "+
			"empty Σ; prompts: %v", prov.observed)
	}
	// And the operator's message reached the model AS AN OBSERVATION, marked.
	if !prov.sawObservation("operator: and what about refresh cycles?") {
		t.Errorf("the operator's message never reached the model as a marked observation: %v",
			prov.observed)
	}
}

// ⚠️ NON-VACUITY, and the failure this change could plausibly introduce: an
// AUTONOMOUS stateful run must still END at `done`. A park that fired on every
// run would hang every scheduled, webhook and sub-agent stateful run in the
// deployment, and the test above would still pass.
func TestRun_Stateful_AnAutonomousRunStillEndsAtDone(t *testing.T) {
	prov := &actionScriptProvider{scripts: []string{
		`{"patch":{"n":1},"done":true,"final":"done"}`,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := Run(ctx, RunOptions{
		Provider: prov, Model: "x",
		Segments: statefulTaskSegs(),
		Context:  statefulCtx(nil),
		OnEvent:  func(providers.Event) {},
	})
	if err != nil {
		t.Fatalf("an autonomous stateful run did not end on its own: %v", err)
	}
	if res.StopReason != "end_turn" || res.FinalText != "done" {
		t.Errorf("stop=%q final=%q, want end_turn/done", res.StopReason, res.FinalText)
	}
}

// The prompt paragraph describing `operator:` observations is for interactive
// runs ONLY — an autonomous run cannot receive one, and a prompt that documents
// impossible inputs teaches nothing and is paid for on every call.
func TestStatefulSystem_TheOperatorParagraphIsInteractiveOnly(t *testing.T) {
	base := []providers.ContentBlock{{Type: "text", Text: "you are an agent"}}
	withIt := buildStatefulSystem(base, nil, nil, true)
	without := buildStatefulSystem(base, nil, nil, false)
	has := func(bs []providers.ContentBlock) bool {
		for _, b := range bs {
			if strings.Contains(b.Text, statefulOperatorPrefix) {
				return true
			}
		}
		return false
	}
	if !has(withIt) {
		t.Error("an interactive stateful prompt never explains what an `operator:` observation is")
	}
	if has(without) {
		t.Error("an autonomous stateful prompt carries a paragraph about an input it cannot receive")
	}
}

// ⚠️ EACH PARK COSTS AN ITERATION, and runStateful derived its own cap from
// opts.MaxIterations — missing the lift Run applies to every interactive run. A
// parked chat on the default 16 died after a handful of exchanges reporting
// max_iterations, which is an answer about the wrong thing.
func TestRun_Stateful_InteractiveGetsTheLiftedIterationCap(t *testing.T) {
	var scripts []string
	for i := 0; i < 20; i++ {
		scripts = append(scripts, `{"patch":{"n":1},"done":true,"final":"ok"}`)
	}
	prov := &actionScriptProvider{scripts: scripts}
	q := make(chan steer.Message, 32)
	park := make(chan struct{}, 32)

	done := make(chan RunResult, 1)
	go func() {
		res, _ := statefulInteractive(t, prov, q, park, nil)
		done <- res
	}()
	for i := 0; i < 18; i++ {
		select {
		case <-park:
		case <-time.After(3 * time.Second):
			t.Fatalf("the run stopped parking after %d turns — the default 16-iteration cap "+
				"still applies to an interactive stateful run", i)
		}
		q <- steer.Message{Text: "again"}
	}
	close(q)
	select {
	case res := <-done:
		if res.StopReason == "max_iterations" {
			t.Errorf("an interactive stateful chat hit max_iterations after %d turns", res.Iterations)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not end")
	}
}

// sigmaCapturingProvider records the Σ it was FED on each step, which is the
// only place a resumed run's recovered state is observable.
type sigmaCapturingProvider struct {
	mu      sync.Mutex
	fed     []string
	scripts []string
	turn    int
}

func (p *sigmaCapturingProvider) ID() string                                   { return "sigma-cap" }
func (p *sigmaCapturingProvider) Probe(context.Context) error                  { return nil }
func (p *sigmaCapturingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *sigmaCapturingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *sigmaCapturingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		for _, c := range m.Content {
			p.fed = append(p.fed, c.Text)
		}
	}
	i := p.turn
	p.turn++
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if i < len(p.scripts) {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "t", Name: emitStateToolName, Input: json.RawMessage(p.scripts[i])}}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}
func (p *sigmaCapturingProvider) firstFed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.fed) == 0 {
		return ""
	}
	return p.fed[0]
}

// ⚠️ THE DRIFT TEST. Σ reaches a waking run by two routes — the goroutine held
// it across a live park, or it was rebuilt from the transcript after a pause,
// snapshot restore or replica move. Two paths is the cheaper design and the
// risk is that they disagree, so the disagreement is what gets asserted rather
// than hoped about.
//
// A resumed run that started from an empty Σ would continue the conversation
// having forgotten every fact it established, which is worse than refusing to
// resume: it looks like it worked.
func TestRun_Stateful_AResumedRunStartsFromTheSameStateAParkWouldHaveHeld(t *testing.T) {
	established := map[string]any{"goal": "explain DDRAM", "decisions": "capacitors, refreshed"}

	// Route 1: the live park. Turn 1 sets Σ; turn 2 is fed what the goroutine held.
	live := &actionScriptProvider{scripts: []string{
		`{"patch":{"goal":"explain DDRAM","decisions":"capacitors, refreshed"},"done":true,"final":"one"}`,
		`{"patch":{},"done":true,"final":"two"}`,
	}}
	q := make(chan steer.Message, 4)
	park := make(chan struct{}, 8)
	// ⚠️ AWAITED, NOT FIRED AND FORGOTTEN. A run left in parkForInput outlives
	// the test that started it, and parkHeartbeatInterval is a package-level var
	// another test MUTATES — so a leaked park reads it while that test writes
	// it, and the race surfaces in the innocent test rather than this one.
	liveDone := make(chan struct{})
	go func() { defer close(liveDone); _, _ = statefulInteractive(t, live, q, park, nil) }()
	select {
	case <-park:
	case <-time.After(3 * time.Second):
		t.Fatal("the live run never parked")
	}
	q <- steer.Message{Text: "go on"}
	select {
	case <-park:
	case <-time.After(3 * time.Second):
		t.Fatal("the live run never re-parked")
	}
	close(q)
	select {
	case <-liveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the live run did not finish after its queue closed")
	}

	// Route 2: the resume. InitialState is what statefulSigmaFromTranscript
	// recovers from the last context_state marker.
	resumed := &sigmaCapturingProvider{scripts: []string{`{"patch":{},"done":true,"final":"resumed"}`}}
	if _, err := Run(context.Background(), RunOptions{
		Provider: resumed, Model: "x",
		Segments:     statefulTaskSegs(),
		Context:      statefulCtx(nil),
		InitialState: established,
		OnEvent:      func(providers.Event) {},
	}); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	// The resumed run's FIRST fed state must carry what the live run's SECOND
	// turn was fed. Compared on the prompt, because that is the only thing the
	// model actually sees — a Σ correct in a struct and absent from the prompt
	// is the failure this whole phase exists to prevent.
	for _, want := range []string{`"goal":"explain DDRAM"`, `"decisions":"capacitors, refreshed"`} {
		if !live.sawObservation(want) {
			t.Errorf("the LIVE park did not carry %s into the next turn: %v", want, live.observed)
		}
		if !strings.Contains(resumed.firstFed(), want) {
			t.Errorf("the RESUMED run was not fed %s — it starts from an empty Σ and has "+
				"forgotten what the run established:\n%s", want, resumed.firstFed())
		}
	}
}

// Non-vacuity: a run with no InitialState still starts empty. A seed that
// leaked in from somewhere would make the test above pass for the wrong reason.
func TestRun_Stateful_WithoutInitialStateTheRunStartsEmpty(t *testing.T) {
	prov := &sigmaCapturingProvider{scripts: []string{`{"patch":{},"done":true,"final":"ok"}`}}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Segments: statefulTaskSegs(),
		Context:  statefulCtx(nil),
		OnEvent:  func(providers.Event) {},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(prov.firstFed(), "Current state:\n{}") {
		t.Errorf("a fresh stateful run was fed a non-empty state:\n%s", prov.firstFed())
	}
}

// usageProvider reports real token counts on every stateful step.
type usageProvider struct {
	mu      sync.Mutex
	turn    int
	scripts []string
	in, out int
	maxCtx  int
}

func (p *usageProvider) ID() string                                   { return "usage-prov" }
func (p *usageProvider) Probe(context.Context) error                  { return nil }
func (p *usageProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *usageProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *usageProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	i := p.turn
	p.turn++
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if i < len(p.scripts) {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "t", Name: emitStateToolName, Input: json.RawMessage(p.scripts[i])}}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use",
		Usage: &providers.Usage{InputTokens: p.in, OutputTokens: p.out}}
	close(ch)
	return ch, nil
}

// ⚠️ THIS LOOP NEVER REPORTED A SINGLE TOKEN IT SPENT, and it is not a display
// bug. EventUsage is what three subsystems key off — the UI gauge, the RFC AV
// per-call ledger (recordCallUsage), and the RFC AW budget counters
// (limits.Add) — and a stateful run emitted none of them. So a mode:stateful
// agent spent tokens that counted against NO per-scope budget, and an operator
// who set a hard limit was not protected from it.
//
// Reported as "tokens: 0 in / 0 out" on a RUNNING interactive chat, which is
// the visible corner of it.
func TestRun_Stateful_ReportsPerCallUsage(t *testing.T) {
	echo := &echoTool{reply: "observed"}
	prov := &usageProvider{in: 4287, out: 557, maxCtx: 40_000, scripts: []string{
		`{"patch":{"n":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"n":2},"done":true,"final":"done"}`,
	}}
	var usages []*providers.Usage
	var mu sync.Mutex
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventUsage && ev.Usage != nil {
				mu.Lock()
				usages = append(usages, ev.Usage)
				mu.Unlock()
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()

	// ONE PER MODEL CALL, not one per run: the ledger is per-call by design, and
	// a single summary row would misattribute a mid-run provider fallback.
	if len(usages) != 2 {
		t.Fatalf("got %d usage events for a 2-step run, want 2 — the ledger, the budget "+
			"counters and the gauge all key off this event", len(usages))
	}
	for i, u := range usages {
		if u.InputTokens != 4287 || u.OutputTokens != 557 {
			t.Errorf("usage[%d] = %d in / %d out, want the provider's own counts", i, u.InputTokens, u.OutputTokens)
		}
		// The gauge needs a denominator and the ledger needs to know which key
		// paid — both ride this event in the append loop, so both must here.
		if u.MaxContextTokens != 40_000 {
			t.Errorf("usage[%d] carries window %d, want the effective 40000 — the gauge has "+
				"no denominator without it", i, u.MaxContextTokens)
		}
		if u.Provider != "usage-prov" {
			t.Errorf("usage[%d] provider = %q, want the serving provider — the ledger records "+
				"which key paid across a mid-run fallback", i, u.Provider)
		}
	}
}

// blockingStatefulProvider holds each call open for `hold` before answering,
// and records how many heartbeats fired WHILE it was held — the pulse that
// keeps a slow step from being reaped, as opposed to the one between steps.
type blockingStatefulProvider struct {
	hold      time.Duration
	beats     *atomicCounter
	mu        sync.Mutex
	duringMin int
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) inc()     { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) get() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

func (p *blockingStatefulProvider) ID() string                                   { return "blocking" }
func (p *blockingStatefulProvider) Probe(context.Context) error                  { return nil }
func (p *blockingStatefulProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *blockingStatefulProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *blockingStatefulProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	before := p.beats.get()
	select {
	case <-time.After(p.hold):
	case <-ctx.Done():
	}
	p.mu.Lock()
	p.duringMin = p.beats.get() - before
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "t0", Name: emitStateToolName,
		Input: json.RawMessage(`{"patch":{"n":1},"done":true,"final":"ok"}`)}}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// ⚠️ A WORKING STATEFUL RUN SENT NO HEARTBEAT AT ALL. Run's lifetime ticker sat
// below the stateful branch, so the only pulse a stateful run ever sent came
// from parkForInput — and the sweeper fails a running row whose heartbeat is
// NULL ten minutes after it started. A slow local model reaches that on one
// step. The assertion is on the pulse DURING the call, which the between-step
// pulse cannot satisfy.
func TestRun_Stateful_HeartbeatsWhileAStepIsInFlight(t *testing.T) {
	orig := parkHeartbeatInterval
	parkHeartbeatInterval = 10 * time.Millisecond
	defer func() { parkHeartbeatInterval = orig }()

	beats := &atomicCounter{}
	prov := &blockingStatefulProvider{hold: 150 * time.Millisecond, beats: beats}
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Segments:    statefulTaskSegs(),
		Context:     statefulCtx(nil),
		OnHeartbeat: beats.inc,
		OnEvent:     func(providers.Event) {},
	})
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("run: stop=%q err=%v", res.StopReason, err)
	}
	prov.mu.Lock()
	during := prov.duringMin
	prov.mu.Unlock()
	if during == 0 {
		t.Errorf("no heartbeat fired during a 150ms step with a 10ms interval — the sweeper "+
			"would fail this run as heartbeat_timeout while it is working (total beats %d)", beats.get())
	}
}
