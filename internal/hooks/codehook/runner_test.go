package codehook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// fakeInterruption records every call and answers from a script.
type fakeInterruption struct {
	mu       sync.Mutex
	inputs   []string
	policies []tools.InterruptionPolicyValue
	answer   func(n int, input string) tools.Result
	delay    time.Duration
}

func (f *fakeInterruption) Name() string                 { return "Interruption" }
func (f *fakeInterruption) Description() string          { return "" }
func (f *fakeInterruption) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *fakeInterruption) Execute(ctx context.Context, in json.RawMessage) (tools.Result, error) {
	f.mu.Lock()
	n := len(f.inputs)
	f.inputs = append(f.inputs, string(in))
	f.policies = append(f.policies, tools.InterruptionPolicy(ctx))
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return tools.Result{IsError: true, Text: "interruption cancelled"}, nil
		}
	}
	return f.answer(n, string(in)), nil
}

func answered(a string) tools.Result {
	b, _ := json.Marshal(map[string]string{"interrupt_id": "int_1", "answer": a})
	return tools.Result{Text: string(b)}
}

func codeHook(code string) *hooks.Hook {
	return &hooks.Hook{ID: "hook_1", Owner: "ops", Name: "gate", Phase: hooks.PhasePre, Code: code, Timeout: time.Second}
}

func preCall(tool, input string) hooks.PreHookCall {
	return hooks.PreHookCall{Phase: hooks.PhasePre, Agent: "a", RunContext: hooks.RunContext{RunID: "r1"},
		ToolCall: hooks.ToolCall{ID: "t1", Name: tool, Input: json.RawMessage(input)}}
}

// A body decides from the event it is given: the tool, its input, the event
// name.
func TestRunner_TheBodyDecidesFromTheEvent(t *testing.T) {
	r := New(nil)
	h := codeHook(`function hook(ev) {
		if (ev.event !== "pre_tool_use") return {decision: "deny", reason: "wrong event " + ev.event};
		if (ev.tool_call.input.path === "/etc/passwd") return {decision: "deny", reason: "not " + ev.tool_call.name};
		return {updated_input: {path: "/safe" + ev.tool_call.input.path}};
	}`)
	got, err := r.Run(context.Background(), h, "pre_tool_use", preCall("Read", `{"path":"/etc/passwd"}`))
	if err != nil || got.Decision != "deny" || got.Reason != "not Read" {
		t.Fatalf("deny: got %+v, %v", got, err)
	}
	got, err = r.Run(context.Background(), h, "pre_tool_use", preCall("Read", `{"path":"/tmp/x"}`))
	if err != nil || string(got.UpdatedInput) != `{"path":"/safe/tmp/x"}` {
		t.Fatalf("rewrite: got %+v (%s), %v", got, got.UpdatedInput, err)
	}
}

// A body that asks holds the call until the answer arrives, then decides on
// it. The ask runs under the hook's own grant — the agent's policy on ctx is
// not what lets it ask.
func TestRunner_AnAskHoldsTheCallAndTheAnswerDecides(t *testing.T) {
	f := &fakeInterruption{answer: func(int, string) tools.Result { return answered("deny") }}
	r := New(f)
	h := codeHook(`function hook(ev) {
		var a = Interruption.ask({question: "Allow " + ev.tool_call.name + "?", options: ["allow", "deny"]});
		return a === "allow" ? {decision: "allow"} : {decision: "deny", reason: "operator said " + a};
	}`)
	ctx := tools.WithInterruptionPolicy(context.Background(), tools.InterruptionPolicyValue{Enabled: false, MaxPending: 3})
	got, err := r.Run(ctx, h, "pre_tool_use", preCall("HTTP", `{}`))
	if err != nil || got.Decision != "deny" || got.Reason != "operator said deny" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(f.inputs) != 1 || !strings.Contains(f.inputs[0], `"op":"ask"`) || !strings.Contains(f.inputs[0], `"question":"Allow HTTP?"`) {
		t.Fatalf("asks = %v", f.inputs)
	}
	p := f.policies[0]
	if !p.Enabled || len(p.Kinds) != 1 || p.Kinds[0] != "question" || p.MaxPending != 3 {
		t.Errorf("the ask ran under %+v, want the hook's own grant keeping the run's pending cap", p)
	}
}

// Each answer is asked for once: a later run of the body replays the answers
// it already has instead of asking again.
func TestRunner_EarlierAnswersAreReplayedNotAskedAgain(t *testing.T) {
	f := &fakeInterruption{answer: func(n int, _ string) tools.Result { return answered([]string{"yes", "no"}[n]) }}
	r := New(f)
	h := codeHook(`function hook(ev) {
		var a = Interruption.ask({question: "first"});
		var b = Interruption.ask({question: "second"});
		return {decision: "deny", reason: a + "/" + b};
	}`)
	got, err := r.Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err != nil || got.Reason != "yes/no" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(f.inputs) != 2 {
		t.Errorf("asked %d times, want 2: %v", len(f.inputs), f.inputs)
	}
}

// The time a person takes to answer is not the hook's timeout: that bounds
// each run of the code.
func TestRunner_TheWaitForAnAnswerIsNotCountedAgainstTheTimeout(t *testing.T) {
	f := &fakeInterruption{delay: 150 * time.Millisecond, answer: func(int, string) tools.Result { return answered("ok") }}
	r := New(f)
	h := codeHook(`function hook(ev) { return {decision: "deny", reason: Interruption.ask({question: "q"})}; }`)
	h.Timeout = 20 * time.Millisecond
	got, err := r.Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err != nil || got.Reason != "ok" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The timeout does stop code that runs too long.
func TestRunner_TheTimeoutStopsALoopingBody(t *testing.T) {
	h := codeHook(`function hook(ev) { while (true) {} }`)
	h.Timeout = 20 * time.Millisecond
	_, err := New(nil).Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// A declined question reads as null; one that timed out throws, so a body
// that does not catch it fails and the fail mode decides.
func TestRunner_ADeclinedAskIsNullAndATimedOutAskThrows(t *testing.T) {
	declined := &fakeInterruption{answer: func(int, string) tools.Result {
		return tools.Result{Text: `{"declined":true,"interrupt_id":"int_1"}`}
	}}
	h := codeHook(`function hook(ev) { var a = Interruption.ask({question: "q"}); return {decision: a === null ? "allow" : "deny"}; }`)
	got, err := New(declined).Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err != nil || got.Decision != "allow" {
		t.Fatalf("declined: got %+v, %v", got, err)
	}
	timedOut := &fakeInterruption{answer: func(int, string) tools.Result {
		return tools.Result{IsError: true, Text: "interruption timed_out (id=int_1)"}
	}}
	_, err = New(timedOut).Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err == nil || !strings.Contains(err.Error(), "timed_out") {
		t.Fatalf("timed out: err = %v", err)
	}
}

// Interruption is the only tool a body has; reaching for another one fails
// at the call and says why.
func TestRunner_ACallToAnyOtherToolFails(t *testing.T) {
	h := codeHook(`function hook(ev) { Bash({command: "id"}); return {}; }`)
	_, err := New(nil).Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err == nil || !strings.Contains(err.Error(), "only tool is Interruption") {
		t.Fatalf("err = %v", err)
	}
}

// No state crosses from one call to the next: every call starts from a fresh
// runtime.
func TestRunner_NoStateCarriesFromOneCallToTheNext(t *testing.T) {
	h := codeHook(`var n = 0; function hook(ev) { n++; return {decision: "deny", reason: "call " + n}; }`)
	r := New(nil)
	for i := 0; i < 2; i++ {
		got, err := r.Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
		if err != nil || got.Reason != "call 1" {
			t.Fatalf("call %d: got %+v, %v", i+1, got, err)
		}
	}
}

// A misspelt field is refused rather than silently ignored.
func TestRunner_AMisspeltDecisionFieldIsRefused(t *testing.T) {
	h := codeHook(`function hook(ev) { return {decison: "deny"}; }`)
	_, err := New(nil).Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err == nil || !strings.Contains(err.Error(), "decison") {
		t.Fatalf("err = %v", err)
	}
}

// Cancelling the run ends the wait for an answer.
func TestRunner_ACancelledRunEndsTheWait(t *testing.T) {
	f := &fakeInterruption{delay: time.Hour, answer: func(int, string) tools.Result { return answered("x") }}
	h := codeHook(`function hook(ev) { Interruption.ask({question: "q"}); return {}; }`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { _, err := New(f).Run(ctx, h, "pre_tool_use", preCall("Read", `{}`)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled run returned a decision")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait outlived the run")
	}
}

// A body that asks without end is stopped.
func TestRunner_ABodyThatAsksWithoutEndIsStopped(t *testing.T) {
	f := &fakeInterruption{answer: func(int, string) tools.Result { return answered("again") }}
	h := codeHook(`function hook(ev) { for (var i = 0; ; i++) { Interruption.ask({question: "q" + i}); } }`)
	_, err := New(f).Run(context.Background(), h, "pre_tool_use", preCall("Read", `{}`))
	if err == nil || !strings.Contains(err.Error(), "Interruption calls") {
		t.Fatalf("err = %v", err)
	}
	if len(f.inputs) != maxAsks {
		t.Errorf("asked %d times, want the cap %d", len(f.inputs), maxAsks)
	}
}

// Registration refuses a body that would fail on its first call.
func TestRunner_CompileRefusesABrokenBody(t *testing.T) {
	r := New(nil)
	for name, src := range map[string]string{
		"syntax":         `function hook(ev) { return {`,
		"no hook":        `function run(input) { return {}; }`,
		"top-level ask":  `Interruption.ask({question: "q"}); function hook(ev) {}`,
		"top-level loop": `while (true) {}`,
	} {
		if err := r.Compile(src); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if err := r.Compile(`function hook(ev) { return {decision: "allow"}; }`); err != nil {
		t.Errorf("a good body was refused: %v", err)
	}
}

// An observe-only hook reports on something that already happened: it may
// notify, but an ask is refused rather than left waiting on a finished run.
func TestRunner_AnObserveHookMayNotifyButNotAsk(t *testing.T) {
	f := &fakeInterruption{answer: func(int, string) tools.Result { return tools.Result{Text: `{}`} }}
	ctx := hooks.WithObserve(context.Background())
	notify := &hooks.Hook{ID: "h", Owner: "ops", Name: "n", Phase: hooks.PhaseRunEnd, Timeout: time.Second,
		Code: `function hook(ev) { Interruption.notify({message: "run " + ev.run_id + " " + ev.status}); }`}
	if _, err := New(f).Run(ctx, notify, "run_end", hooks.LifecycleHookCall{RunContext: hooks.RunContext{RunID: "r1"}, Status: "failed"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if len(f.inputs) != 1 || !strings.Contains(f.inputs[0], `"message":"run r1 failed"`) {
		t.Errorf("notify calls = %v", f.inputs)
	}
	ask := &hooks.Hook{ID: "h", Owner: "ops", Name: "a", Phase: hooks.PhaseRunEnd, Timeout: time.Second,
		Code: `function hook(ev) { Interruption.ask({question: "?"}); }`}
	if _, err := New(f).Run(ctx, ask, "run_end", hooks.LifecycleHookCall{}); err == nil || !strings.Contains(err.Error(), "not available to a run_end hook") {
		t.Fatalf("ask: err = %v", err)
	}
	if len(f.inputs) != 1 {
		t.Errorf("the ask reached Interruption: %v", f.inputs)
	}
}

// A body registration refuses is never cached, nor is any body only compiled;
// the run-time cache is bounded; and an oversized body is refused before it is
// parsed. The cache used to keep every body it ever saw, registration's
// included, forever, and a 10 MB body was parsed before the size check.
func TestRunner_TheProgramCacheHoldsOnlyRunBodiesAndIsBounded(t *testing.T) {
	r := New(nil)
	for i := 0; i < 10; i++ {
		_ = r.Compile(fmt.Sprintf("function hook(ev) { return %d; }", i))
		_ = r.Compile(fmt.Sprintf("function run() { return %d; }", i)) // refused: no hook(ev)
	}
	if n := r.lru.Len(); n != 0 {
		t.Errorf("after compiling only, %d programs are cached", n)
	}
	for i := 0; i < maxCachedPrograms+20; i++ {
		if _, err := r.Run(context.Background(), codeHook(fmt.Sprintf("function hook(ev) { var n = %d; }", i)), "pre_tool_use", preCall("Read", `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if n, m := r.lru.Len(), len(r.cache); n != maxCachedPrograms || m != maxCachedPrograms {
		t.Errorf("cache holds %d (index %d) programs, want the %d most recently run", n, m, maxCachedPrograms)
	}

	big := "function hook(ev) {" + strings.Repeat(" ", hooks.MaxCodeBytes) + "(" // a syntax error, too
	if err := r.Compile(big); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Errorf("an oversized body: err = %v, want the size refusal before any parse", err)
	}
}
