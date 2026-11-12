package loop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/errkind"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// countingPauseGate counts how often the loop asks whether to pause.
type countingPauseGate struct{ asked atomic.Int32 }

func (g *countingPauseGate) PauseRequested() bool       { g.asked.Add(1); return false }
func (g *countingPauseGate) Park(context.Context) error { return nil }

// A cancelled run stops at the top of its iteration, as a stateful run stops
// at the top of its step: no heartbeat, no pause check and no model call
// after the cancel.
func TestRun_CancelledRunStopsBeforeHeartbeatPauseAndRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prov := &endTurnProvider{}
	gate := &countingPauseGate{}
	var beats atomic.Int32
	res, err := Run(ctx, RunOptions{
		Provider:    prov,
		Model:       "x",
		Dispatcher:  tools.NewDispatcher(nil),
		Segments:    userSeg("go"),
		PauseGate:   gate,
		OnHeartbeat: func() { beats.Add(1) },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.StopReason != "cancelled" {
		t.Errorf("stop reason = %q, want cancelled", res.StopReason)
	}
	if n := prov.calls(); n != 0 {
		t.Errorf("provider called %d time(s) after the cancel, want 0", n)
	}
	if n := beats.Load(); n != 0 {
		t.Errorf("heartbeat sent %d time(s) after the cancel, want 0", n)
	}
	if n := gate.asked.Load(); n != 0 {
		t.Errorf("pause asked %d time(s) after the cancel, want 0", n)
	}
}

// A stateful run records one loomcycle.iteration span per step, as the append
// loop does per iteration, ended however the step ends; the step's model call
// and its action's tool call are children of it.
func TestRunStateful_RecordsAnIterationSpanPerStep(t *testing.T) {
	exp, _ := traced(t)
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":2},"done":true,"final":"count is 2"}`,
	}}
	echo := &echoTool{reply: "observed"}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Tools: []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(), Context: statefulCtx(nil),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	iters := map[trace.SpanID]int64{}
	for _, sp := range exp.GetSpans() {
		if sp.Name != lcotel.SpanIteration {
			continue
		}
		for _, a := range sp.Attributes {
			if string(a.Key) == lcotel.AttrIteration {
				iters[sp.SpanContext.SpanID()] = a.Value.AsInt64()
			}
		}
	}
	if len(iters) != 2 {
		t.Fatalf("ended iteration spans = %d, want 2 (one per step)", len(iters))
	}
	for _, name := range []string{lcotel.SpanModelCall, lcotel.SpanToolCall} {
		var n int
		for _, sp := range exp.GetSpans() {
			if sp.Name != name {
				continue
			}
			n++
			if _, ok := iters[sp.Parent.SpanID()]; !ok {
				t.Errorf("%s span is not a child of an iteration span", name)
			}
		}
		if n == 0 {
			t.Errorf("no %s span recorded", name)
		}
	}
}

// spanAtParkGate requests one pause and records the span its Park ran under.
type spanAtParkGate struct {
	asked  atomic.Int32
	parked trace.SpanContext
}

func (g *spanAtParkGate) PauseRequested() bool { return g.asked.Add(1) == 1 }
func (g *spanAtParkGate) Park(ctx context.Context) error {
	g.parked = trace.SpanFromContext(ctx).SpanContext()
	return nil
}

// A stateful run parks for a runtime pause on its step's own ctx, inside the
// step's iteration span, as an append run parks on its iteration's ctx.
func TestRunStateful_ParksInsideTheIterationSpan(t *testing.T) {
	exp, _ := traced(t)
	prov := &statefulScriptProvider{scripts: []string{`{"patch":{"count":1},"done":true,"final":"count is 1"}`}}
	gate := &spanAtParkGate{}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Dispatcher: tools.NewDispatcher(nil),
		Segments: statefulTaskSegs(), Context: statefulCtx(nil), PauseGate: gate,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !gate.parked.IsValid() {
		t.Fatalf("the park ran under no span; want the step's iteration span")
	}
	for _, sp := range exp.GetSpans() {
		if sp.Name == lcotel.SpanIteration && sp.SpanContext.SpanID() == gate.parked.SpanID() {
			return
		}
	}
	t.Errorf("the park ran under span %s, which is not an iteration span", gate.parked.SpanID())
}

// stampProbeTool records, per call, what the loop stamped on the ctx a tool reads.
type stampProbeTool struct {
	mu      sync.Mutex
	iters   []int
	distill []tools.LastDistillValue
}

func (p *stampProbeTool) Name() string                 { return "Noop" }
func (p *stampProbeTool) Description() string          { return "records its ctx" }
func (p *stampProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *stampProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.iters = append(p.iters, IterationOf(ctx))
	p.distill = append(p.distill, tools.LastDistill(ctx))
	return tools.Result{Text: "ok"}, nil
}

// A stateful action's ctx carries the step's iteration and the run's last
// distillation decline, as an append tool's does. Σ eviction is this mode's
// distillation, so its decline — every key is core and the window is over the
// backstop — is what Context op=self reports.
func TestRunStateful_ActionCtxCarriesIterationAndLastDistill(t *testing.T) {
	prov := &growingStateProvider{inTok: 9000, maxCtx: 10000}
	probe := &stampProbeTool{}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Tools: []tools.Tool{probe},
		Dispatcher: tools.NewDispatcher([]tools.Tool{probe}),
		Segments:   statefulTaskSegs(), Context: statefulCtx(nil),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.iters) < 2 {
		t.Fatalf("actions run = %d, want at least 2", len(probe.iters))
	}
	for i, it := range probe.iters {
		if it != i {
			t.Errorf("action of step %d read iteration %d", i, it)
		}
	}
	if d := probe.distill[0]; d.Mode != "stateful" || d.Reason != providers.DistillDeclineSplitDeclined {
		t.Errorf("action of step 0 read last distill %+v, want the stateful eviction decline", d)
	}
}

// failSecondPlugin passes the first request and fails every later one.
type failSecondPlugin struct{ n atomic.Int32 }

func (p *failSecondPlugin) Name() string { return "fail-second" }
func (p *failSecondPlugin) Transform(_ context.Context, sys []providers.ContentBlock, msgs []providers.Message) ([]providers.ContentBlock, []providers.Message, error) {
	if p.n.Add(1) > 1 {
		return nil, nil, errors.New("transform refused")
	}
	return sys, msgs, nil
}

// readThenFail is a first turn that calls FakeRead and reports 7 input tokens.
func readThenFail() []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "t1", Name: "FakeRead", Input: json.RawMessage(`{}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 7, OutputTokens: 1}},
	}
}

// A run that fails on a context transform or a fatal provider error ends in
// one shape in both loops: stop reason "error", the usage spent so far kept,
// and the error event carrying the error's own text.
func TestRun_FailureEndsInTheErrorShape(t *testing.T) {
	cases := []struct {
		name    string
		second  [][]providers.Event // the provider's responses after the tool turn
		plugins []contextplugin.Plugin
		wantMsg string
	}{
		{name: "context transform", second: [][]providers.Event{{{Type: providers.EventDone, StopReason: "end_turn"}}},
			plugins: []contextplugin.Plugin{&failSecondPlugin{}}, wantMsg: "context transform: context plugin \"fail-second\": transform refused"},
		{name: "provider call", wantMsg: "no scripted response"},
		{name: "provider stream", second: [][]providers.Event{{{Type: providers.EventError, Error: "provider exploded"}}},
			wantMsg: "provider exploded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeTool{}
			prov := &fakeProvider{responses: append([][]providers.Event{readThenFail()}, tc.second...)}
			var lastErr string
			res, err := Run(context.Background(), RunOptions{
				Provider: prov, Model: "fake-model", Tools: []tools.Tool{ft},
				Dispatcher: tools.NewDispatcher([]tools.Tool{ft}), Segments: userSeg("go"),
				ContextPlugins: tc.plugins,
				OnEvent: func(ev providers.Event) {
					if ev.Type == providers.EventError {
						lastErr = ev.Error
					}
				},
			})
			if err == nil {
				t.Fatalf("Run: nil error, want the failure")
			}
			if res.StopReason != "error" {
				t.Errorf("stop reason = %q, want error", res.StopReason)
			}
			if res.Usage.InputTokens != 7 {
				t.Errorf("usage input = %d, want the 7 spent before the failure", res.Usage.InputTokens)
			}
			if lastErr != tc.wantMsg {
				t.Errorf("error event = %q, want %q", lastErr, tc.wantMsg)
			}
		})
	}

	t.Run("stateful provider call", func(t *testing.T) {
		var lastErr string
		res, err := Run(context.Background(), RunOptions{
			Provider: &fakeProvider{}, Model: "x", Dispatcher: tools.NewDispatcher(nil),
			Segments: statefulTaskSegs(), Context: statefulCtx(nil),
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventError {
					lastErr = ev.Error
				}
			},
		})
		if err == nil {
			t.Fatalf("Run: nil error, want the failure")
		}
		if res.StopReason != "error" {
			t.Errorf("stop reason = %q, want error", res.StopReason)
		}
		if lastErr != "no scripted response" {
			t.Errorf("error event = %q, want the provider's own text", lastErr)
		}
	})
}

// thinkingStatefulProvider streams a reasoning trace and some prose before
// its one emit_state call.
type thinkingStatefulProvider struct{ statefulScriptProvider }

func (p *thinkingStatefulProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	inner, err := p.statefulScriptProvider.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan providers.Event, 8)
	ch <- providers.Event{Type: providers.EventThinking, Text: "weighing the count"}
	ch <- providers.Event{Type: providers.EventText, Text: "prose the step discards"}
	for ev := range inner {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// A stateful step forwards the model's reasoning trace live, as the append
// loop does; its prose still is not (a step's prose is not an answer).
func TestRunStateful_ForwardsThinkingLive(t *testing.T) {
	prov := &thinkingStatefulProvider{statefulScriptProvider{scripts: []string{`{"patch":{"count":1},"done":true,"final":"count is 1"}`}}}
	var thinking, texts []string
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Dispatcher: tools.NewDispatcher(nil),
		Segments: statefulTaskSegs(), Context: statefulCtx(nil),
		OnEvent: func(ev providers.Event) {
			switch ev.Type {
			case providers.EventThinking:
				thinking = append(thinking, ev.Text)
			case providers.EventText:
				texts = append(texts, ev.Text)
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(thinking) != 1 || thinking[0] != "weighing the count" {
		t.Errorf("thinking events = %q, want the step's trace forwarded once", thinking)
	}
	if len(texts) != 1 || texts[0] != "count is 1" {
		t.Errorf("text events = %q, want only the turn's answer", texts)
	}
}

// heldStatefulProvider holds its stream open until the call's ctx ends,
// then closes it unfinished; started is closed once the call is out.
type heldStatefulProvider struct {
	statefulScriptProvider
	started chan struct{}
}

func (p *heldStatefulProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event)
	go func() {
		close(p.started)
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// A stateful run cancelled while its model call is out ends with the same
// "cancelled, nothing failed" error event an append run emits, so the two
// read alike in the transcript.
func TestRunStateful_CancelMidCallEmitsTheCancelledEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &heldStatefulProvider{started: make(chan struct{})}
	go func() { <-prov.started; cancel() }()
	var errs []providers.Event
	var mu sync.Mutex
	res, err := Run(ctx, RunOptions{
		Provider: prov, Model: "x", Dispatcher: tools.NewDispatcher(nil),
		Segments: statefulTaskSegs(), Context: statefulCtx(nil),
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventError {
				mu.Lock()
				errs = append(errs, ev)
				mu.Unlock()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.StopReason != "cancelled" {
		t.Errorf("stop reason = %q, want cancelled", res.StopReason)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 1 || errs[0].ErrorInfo == nil || errs[0].ErrorInfo.Category != errkind.CategoryCancelled {
		t.Fatalf("error events = %+v, want one classified cancelled", errs)
	}
}
