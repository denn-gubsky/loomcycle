package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// choosingProvider is a fakeProvider whose target can be held to a tool choice.
type choosingProvider struct{ fakeProvider }

func (p *choosingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsToolChoice: true}
}

func toolCall(id string) providers.Event {
	return providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "FakeRead", Input: json.RawMessage(`{}`)}}
}

func usage(in, out int) *providers.Usage { return &providers.Usage{InputTokens: in, OutputTokens: out} }

// fanOutOnTheLastIteration scripts max_iterations=2 where iteration 2 calls two
// tools, then the closing turn's answer.
func fanOutOnTheLastIteration() [][]providers.Event {
	return [][]providers.Event{
		{toolCall("t1"), {Type: providers.EventDone, StopReason: "tool_use", Usage: usage(10, 1)}},
		{toolCall("t2"), toolCall("t3"), {Type: providers.EventDone, StopReason: "tool_use", Usage: usage(20, 2)}},
		{{Type: providers.EventText, Text: "answer from both results"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: usage(40, 7)}},
	}
}

func runCapped(t *testing.T, prov providers.Provider, tool *fakeTool, events *[]providers.Event) RunResult {
	t.Helper()
	res, err := Run(context.Background(), RunOptions{
		Provider:      prov,
		Model:         "fake-model",
		Tools:         []tools.Tool{tool},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{tool}),
		MaxIterations: 2,
		// fakeTool records calls unguarded; the fan-out's two calls run in turn.
		ToolParallelism: 1,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		OnEvent:         func(ev providers.Event) { *events = append(*events, ev) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// A run whose last allowed iteration fans out to tools answers from their
// results in one closing call: its text is the run's answer, the stop reason
// is still max_iterations, the call is billed and on the stream like any
// other, and it sees the results with tools disabled.
func TestRun_ClosingTurnAnswersFromTheLastToolResults(t *testing.T) {
	prov := &choosingProvider{fakeProvider{responses: fanOutOnTheLastIteration()}}
	tool := &fakeTool{}
	var events []providers.Event
	res := runCapped(t, prov, tool, &events)

	if res.StopReason != "max_iterations" {
		t.Errorf("stop reason = %q, want max_iterations", res.StopReason)
	}
	if res.FinalText != "answer from both results" {
		t.Errorf("final text = %q, want the closing turn's answer", res.FinalText)
	}
	if len(prov.calls) != 3 || len(tool.calls) != 3 {
		t.Fatalf("model calls = %d, tool calls = %d; want 3 and 3", len(prov.calls), len(tool.calls))
	}
	if res.Usage.InputTokens != 70 || res.Usage.OutputTokens != 10 {
		t.Errorf("usage = %+v, want the closing call counted (70 in / 10 out)", res.Usage)
	}
	usageEvents, closingText := 0, false
	for _, ev := range events {
		if ev.Type == providers.EventUsage {
			usageEvents++
		}
		if ev.Type == providers.EventText && ev.Text == "answer from both results" {
			closingText = true
		}
	}
	if usageEvents != 3 || !closingText {
		t.Errorf("usage events = %d, closing text on the stream = %v; want 3 and true", usageEvents, closingText)
	}

	closing := prov.calls[2]
	if closing.ToolChoice.Mode != providers.ToolChoiceNone || len(closing.Tools) == 0 {
		t.Errorf("closing call tool_choice=%q tools=%d, want none with the tool list kept", closing.ToolChoice.Mode, len(closing.Tools))
	}
	results := 0
	for _, m := range closing.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" && (c.ToolUseID == "t2" || c.ToolUseID == "t3") {
				results++
			}
		}
	}
	if results != 2 {
		t.Errorf("closing call carried %d of the last round's 2 tool results", results)
	}
	last := closing.Messages[len(closing.Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content[0].Text, "tools are now disabled") {
		t.Errorf("closing call's last message = %+v, want the closing note", last)
	}
}

// A target that cannot be held to "none" gets no tool list on the closing call.
func TestRun_ClosingTurnWithholdsToolsWhenNoneIsNotEnforced(t *testing.T) {
	prov := &fakeProvider{responses: fanOutOnTheLastIteration()}
	var events []providers.Event
	runCapped(t, prov, &fakeTool{}, &events)
	if len(prov.calls) != 3 {
		t.Fatalf("model calls = %d, want 3", len(prov.calls))
	}
	if c := prov.calls[2]; len(c.Tools) != 0 || c.ToolChoice.Forces() {
		t.Errorf("closing call tools=%d tool_choice=%q, want the list withheld", len(c.Tools), c.ToolChoice.Mode)
	}
}

// Tool calls the closing turn makes anyway are never run, and the run still
// ends at the cap.
func TestRun_ClosingTurnToolCallsAreNotRun(t *testing.T) {
	script := fanOutOnTheLastIteration()
	script[2] = []providers.Event{{Type: providers.EventText, Text: "one more look"}, toolCall("t4"), {Type: providers.EventDone, StopReason: "tool_use"}}
	prov := &fakeProvider{responses: script}
	tool := &fakeTool{}
	var events []providers.Event
	res := runCapped(t, prov, tool, &events)
	if res.StopReason != "max_iterations" || len(tool.calls) != 3 || len(prov.calls) != 3 {
		t.Errorf("stop=%q tool calls=%d model calls=%d; want max_iterations, 3, 3", res.StopReason, len(tool.calls), len(prov.calls))
	}
}

// A last iteration that answers instead of calling tools ends the run as
// before: no closing call.
func TestRun_NoClosingTurnWhenTheLastIterationAnswered(t *testing.T) {
	script := fanOutOnTheLastIteration()
	script[1] = []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn"}}
	prov := &fakeProvider{responses: script}
	var events []providers.Event
	res := runCapped(t, prov, &fakeTool{}, &events)
	if res.StopReason != "end_turn" || len(prov.calls) != 2 {
		t.Errorf("stop=%q model calls=%d; want end_turn and 2", res.StopReason, len(prov.calls))
	}
}

// Only the last iteration the cap allows earns the closing turn, and never an
// iteration-unbounded run's, whose cap is a runaway ceiling.
func TestGrantsClosingTurn_OnlyTheCappedLastIteration(t *testing.T) {
	cases := []struct {
		iter, cap int
		unbounded bool
		want      bool
	}{
		{1, 2, false, true},
		{0, 2, false, false},
		{0, 1, false, true},
		{maxIterationsHardCeiling - 1, maxIterationsHardCeiling, true, false},
		{1, 2, true, false},
	}
	for _, c := range cases {
		if got := grantsClosingTurn(c.iter, c.cap, c.unbounded); got != c.want {
			t.Errorf("grantsClosingTurn(%d, %d, %v) = %v, want %v", c.iter, c.cap, c.unbounded, got, c.want)
		}
	}
}

// closingTurnGated is a startReviewRun mutation: a capped fan-out run that
// gets the closing turn, with the given gates.
func closingTurnGated(d *hooks.Dispatcher, review bool, steerQueue bool) func(*RunOptions) {
	return func(o *RunOptions) {
		tool := &fakeTool{}
		o.Provider = &fakeProvider{responses: fanOutOnTheLastIteration()}
		o.Tools, o.Dispatcher = []tools.Tool{tool}, tools.NewDispatcher([]tools.Tool{tool})
		o.MaxIterations, o.ToolParallelism = 2, 1
		o.AgentName, o.Hooks, o.Review = "writer", d, review
		if !steerQueue {
			o.SteerQueue = nil
		}
	}
}

// The closing turn's answer is a finished answer, so the gates on an answer
// decide on it, with no iteration left: an agent_stop block fails the run, a
// hold on a run nothing can deliver a verdict to ends it rejected, and a
// review holds it — approved, the run ends at its cap on it; sent back, it
// ends rejected. Before, the closing answer skipped them all and was handed
// on as a clean result.
func TestRun_ClosingTurnAnswerGoesThroughTheAnswerGates(t *testing.T) {
	t.Run("agent_stop block", func(t *testing.T) {
		h := newScriptedHook(t, `{"decision":"block","reason":"cite a source"}`)
		r := startReviewRun(t, context.Background(), closingTurnGated(lifecycleHooks(t,
			&hooks.Hook{Owner: "ops", Name: "check", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}), false, true))
		o := r.finish(t)
		if o.err == nil || o.res.StopReason != StopReasonStopBlocked || !strings.Contains(o.err.Error(), "no iteration left") {
			t.Fatalf("outcome = %+v, %v", o.res, o.err)
		}
		if h.count() != 1 {
			t.Errorf("the hook was asked %d times, want once", h.count())
		}
	})
	t.Run("hold with no verdict possible", func(t *testing.T) {
		h := newScriptedHook(t, `{"decision":"hold"}`)
		r := startReviewRun(t, context.Background(), closingTurnGated(lifecycleHooks(t,
			&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}), false, false))
		if o := r.finish(t); o.err != nil || o.res.StopReason != StopReasonRejected {
			t.Fatalf("outcome = %+v, %v", o.res, o.err)
		}
	})
	for _, tc := range []struct {
		name, kind, text, want string
	}{
		{"review approved", steer.KindApprove, "", "max_iterations"},
		{"review sent back", steer.KindReject, "redo it", StopReasonRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startReviewRun(t, context.Background(), closingTurnGated(nil, true, true))
			r.waitFor(t, providers.EventAwaitingReview)
			r.q <- verdict(tc.kind, tc.text) // stamped after the hold began, as a real verdict is
			o := r.finish(t)
			if o.err != nil || o.res.StopReason != tc.want || o.res.FinalText != "answer from both results" {
				t.Fatalf("outcome = %+v, %v; want %q on the closing answer", o.res, o.err, tc.want)
			}
		})
	}
}

// An interactive run's closing answer ends the run at its cap: it has no
// iteration left to answer an operator's next message, so it does not park
// for one.
func TestRun_ClosingTurnDoesNotParkAnInteractiveRun(t *testing.T) {
	r := startReviewRun(t, context.Background(), func(o *RunOptions) {
		closingTurnGated(nil, false, true)(o)
		o.Interactive = true
	})
	o := r.finish(t)
	if o.err != nil || o.res.StopReason != "max_iterations" || o.res.FinalText != "answer from both results" {
		t.Fatalf("outcome = %+v, %v", o.res, o.err)
	}
}
