package codejs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// writeAgent writes agent_code/<name>/index.js under a temp root and returns
// the root. The provider resolves agents relative to this root.
func writeAgent(t *testing.T, name, js string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// dispatchFn stands in for the loop's tool dispatch: given a tool name +
// input, it returns the result text and whether it is an error.
type dispatchFn func(name string, input json.RawMessage) (text string, isError bool)

// driveResult is the outcome of driving a code-agent run to completion.
type driveResult struct {
	events    []providers.Event // every event across all Call turns, in order
	toolCalls []providers.ToolUse
	finalText string
	errText   string // set if the run ended in EventError
}

// drive mimics internal/loop: it calls Provider.Call, ranges the events,
// and on a tool_use turn dispatches the tool (via fn) and re-invokes Call
// with the tool_result appended — exactly as the real loop does. This proves
// the replay handshake against the actual provider contract without pulling
// in the whole loop package.
func drive(t *testing.T, ctx context.Context, p *Provider, agentName, prompt string, tools []providers.ToolSpec, fn dispatchFn) driveResult {
	t.Helper()
	ctx = providers.WithRunMeta(ctx, providers.RunMeta{AgentName: agentName, UserID: "u-test"})

	msgs := []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: prompt}}}}
	var res driveResult
	const maxTurns = 50
	for turn := 0; turn < maxTurns; turn++ {
		ch, err := p.Call(ctx, providers.Request{Model: "code-js", Messages: msgs, Tools: tools})
		if err != nil {
			t.Fatalf("Call returned a Go error (should surface as EventError): %v", err)
		}
		var stop string
		var lastTool *providers.ToolUse
		for ev := range ch {
			res.events = append(res.events, ev)
			switch ev.Type {
			case providers.EventToolCall:
				tu := *ev.ToolUse
				lastTool = &tu
				res.toolCalls = append(res.toolCalls, tu)
			case providers.EventText:
				res.finalText += ev.Text
			case providers.EventDone:
				stop = ev.StopReason
			case providers.EventError:
				res.errText = ev.Error
			}
		}
		if stop != "tool_use" || lastTool == nil {
			return res
		}
		text, isErr := fn(lastTool.Name, lastTool.Input)
		msgs = append(msgs,
			providers.Message{Role: "assistant", Content: []providers.ContentBlock{{
				Type: "tool_use", ToolUseID: lastTool.ID, ToolName: lastTool.Name, ToolInput: lastTool.Input,
			}}},
			providers.Message{Role: "user", Content: []providers.ContentBlock{{
				Type: "tool_result", ToolUseID: lastTool.ID, Text: text, IsError: isErr,
			}}},
		)
	}
	t.Fatalf("drive exceeded %d turns without completion", maxTurns)
	return res
}

func newTestProvider(root string) *Provider {
	return New(Config{CodeRoot: root, RunTimeout: 5 * time.Second})
}

// The multi-turn replay handshake: a code-agent that calls memory.get then
// memory.set then returns must produce, across the loop's turns,
// EventToolCall(Memory,get) → [dispatch] → EventToolCall(Memory,set) →
// [dispatch] → final text. On each resume the run() re-executes from the top,
// fast-forwarding the recorded results so the get value (41) flows into the
// set value (42) — proving replay reaches the same state.
func TestCodeJS_MultiTurnReplay_TwoToolCalls(t *testing.T) {
	js := `
function run(input) {
  var got = memory.get({ scope: "user", key: "counter" });
  var n = got && got.value ? got.value : 0;
  memory.set({ scope: "user", key: "counter", value: n + 1 });
  return { final_text: "counter was " + n + " for " + input.metadata.user_id };
}`
	root := writeAgent(t, "counter", js)
	p := newTestProvider(root)

	var setInput json.RawMessage
	res := drive(t, context.Background(), p, "counter", "go", []providers.ToolSpec{{Name: "Memory"}},
		func(name string, input json.RawMessage) (string, bool) {
			switch {
			case strings.Contains(string(input), `"op":"get"`):
				return `{"value": 41, "expires_at": null}`, false
			case strings.Contains(string(input), `"op":"set"`):
				setInput = input
				return `{"ok": true}`, false
			}
			t.Fatalf("unexpected tool input: %s", input)
			return "", false
		})

	if res.errText != "" {
		t.Fatalf("run errored: %s", res.errText)
	}
	if len(res.toolCalls) != 2 {
		t.Fatalf("want 2 tool calls (get, set), got %d: %+v", len(res.toolCalls), res.toolCalls)
	}
	if res.toolCalls[0].Name != "Memory" || !strings.Contains(string(res.toolCalls[0].Input), `"op":"get"`) {
		t.Errorf("first call should be Memory get; got %s %s", res.toolCalls[0].Name, res.toolCalls[0].Input)
	}
	if !strings.Contains(string(res.toolCalls[1].Input), `"op":"set"`) {
		t.Errorf("second call should be Memory set; got %s", res.toolCalls[1].Input)
	}
	// The loop-dispatched get result (41) must have flowed into the JS and
	// driven the set value (42) — proving the result returned synchronously.
	if !strings.Contains(string(setInput), `"value":42`) {
		t.Errorf("set value should derive from the get result (42); got %s", setInput)
	}
	if !strings.Contains(res.finalText, "counter was 41") || !strings.Contains(res.finalText, "u-test") {
		t.Errorf("final text wrong: %q", res.finalText)
	}
}

// A run that makes no tool calls returns immediately with end_turn.
func TestCodeJS_NoToolCalls(t *testing.T) {
	root := writeAgent(t, "hello", `function run(input){ return { final_text: "hi " + input.prompt }; }`)
	p := newTestProvider(root)
	res := drive(t, context.Background(), p, "hello", "there", nil, func(string, json.RawMessage) (string, bool) {
		t.Fatal("no tool call expected")
		return "", false
	})
	if res.errText != "" || res.finalText != "hi there" {
		t.Fatalf("want 'hi there', got final=%q err=%q", res.finalText, res.errText)
	}
	if len(res.toolCalls) != 0 {
		t.Errorf("expected zero tool calls, got %d", len(res.toolCalls))
	}
}

// A tool the loop returns as IsError surfaces as a catchable JS throw; caught,
// the run continues and completes normally.
func TestCodeJS_IsError_IsCatchableThrow(t *testing.T) {
	js := `
function run(input) {
  try {
    memory.get({ scope: "user", key: "x" });
    return { final_text: "no throw" };
  } catch (e) {
    return { final_text: "caught: " + e.message };
  }
}`
	root := writeAgent(t, "catcher", js)
	p := newTestProvider(root)
	res := drive(t, context.Background(), p, "catcher", "go", []providers.ToolSpec{{Name: "Memory"}},
		func(string, json.RawMessage) (string, bool) {
			return "quota exceeded", true // IsError
		})
	if res.errText != "" {
		t.Fatalf("run should not error (the throw is caught): %s", res.errText)
	}
	if !strings.Contains(res.finalText, "caught:") || !strings.Contains(res.finalText, "quota exceeded") {
		t.Errorf("expected caught error text, got %q", res.finalText)
	}
}

// An uncaught throw (including a tool IsError that propagates) ends the run as
// EventError with the code_agent_threw prefix.
func TestCodeJS_UncaughtThrow_BecomesEventError(t *testing.T) {
	root := writeAgent(t, "thrower", `function run(){ throw new Error("boom"); }`)
	p := newTestProvider(root)
	res := drive(t, context.Background(), p, "thrower", "go", nil, nil)
	if res.errText == "" {
		t.Fatal("expected EventError for an uncaught throw")
	}
	if !strings.HasPrefix(res.errText, "code_agent_threw:") || !strings.Contains(res.errText, "boom") {
		t.Errorf("want code_agent_threw + boom, got %q", res.errText)
	}
}

// Any allowed tool is callable, not just the memory/channel/agent meta-tools:
// a built-in like WebFetch binds as a flat callable by its canonical name and
// dispatches through the loop like any other tool. (Regression for the bug
// where only memory/channel/agent + mcp__* were bound, so the ATS example's
// WebFetch was unreachable and a fictional mcp__http_fetch__get was invented.)
func TestCodeJS_BuiltinTool_FlatCallable(t *testing.T) {
	// The JS JSON.parse()s the WebFetch result. This only works if a flat tool
	// returns its RAW STRING — auto-parsing a JSON-looking body (the old bug)
	// would hand the JS an object and the JSON.parse would throw.
	root := writeAgent(t, "fetcher",
		`function run(){ var d = JSON.parse(WebFetch({ url: "https://x.example/api" })); return { final_text: "n=" + d.jobs.length }; }`)
	p := newTestProvider(root)
	var sawURL bool
	res := drive(t, context.Background(), p, "fetcher", "go", []providers.ToolSpec{{Name: "WebFetch"}},
		func(name string, input json.RawMessage) (string, bool) {
			if name != "WebFetch" {
				t.Errorf("dispatched tool name = %q, want WebFetch", name)
			}
			if strings.Contains(string(input), "x.example") {
				sawURL = true
			}
			return `{"jobs":[{"id":"1"},{"id":"2"}]}`, false // a JSON body, as a string
		})
	if res.errText != "" {
		t.Fatalf("run errored (flat tool result not a raw string?): %s", res.errText)
	}
	if !sawURL {
		t.Error("WebFetch input (url) did not reach the dispatcher")
	}
	if !strings.Contains(res.finalText, "n=2") {
		t.Errorf("WebFetch raw-string body did not reach JS for JSON.parse: %q", res.finalText)
	}
}

// Default-deny: a tool absent from allowed_tools (req.Tools) gets NO binding,
// so referencing it is a ReferenceError — not a permission error.
func TestCodeJS_AllowedTools_DisallowedIsReferenceError(t *testing.T) {
	root := writeAgent(t, "sneaky", `function run(){ channel.publish({name:"x"}); return {final_text:"ok"}; }`)
	p := newTestProvider(root)
	// Only Memory is allowed; channel must not exist.
	res := drive(t, context.Background(), p, "sneaky", "go", []providers.ToolSpec{{Name: "Memory"}}, nil)
	if res.errText == "" || !strings.Contains(res.errText, "channel is not defined") {
		t.Errorf("want ReferenceError 'channel is not defined', got %q", res.errText)
	}
}

// eval and the Function constructor are removed from the sandbox at boot.
func TestCodeJS_SandboxBlocksEvalAndFunction(t *testing.T) {
	for _, tc := range []struct{ name, js, want string }{
		{"eval", `function run(){ return {final_text: eval("1+1")+""}; }`, "eval is not defined"},
		{"Function", `function run(){ var f = Function("return 1"); return {final_text: f()+""}; }`, "Function is not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeAgent(t, "esc", tc.js)
			p := newTestProvider(root)
			res := drive(t, context.Background(), p, "esc", "go", nil, nil)
			if res.errText == "" || !strings.Contains(res.errText, tc.want) {
				t.Errorf("want %q, got %q", tc.want, res.errText)
			}
		})
	}
}

// A missing agent file fails loud as an EventError naming the path.
func TestCodeJS_MissingAgentFile(t *testing.T) {
	p := newTestProvider(t.TempDir())
	res := drive(t, context.Background(), p, "ghost", "go", nil, nil)
	if res.errText == "" || !strings.Contains(res.errText, "no index.js") {
		t.Errorf("want missing-file error, got %q", res.errText)
	}
}

// A broken JS file fails at Compile (load time), naming the path.
func TestCodeJS_Compile_ParseError(t *testing.T) {
	root := writeAgent(t, "broken", `function run( { syntax error`)
	p := newTestProvider(root)
	if _, err := p.Compile("broken"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("want parse error from Compile, got %v", err)
	}
}

// A CPU-bound JS loop executes bytecode and so is interruptible — the
// per-turn timeout's Interrupt kills it. (Replay never parks in a tool call,
// so the goja-#97 parked-host-func case the Mechanism-1 model worried about
// does not arise: tool calls return immediately — fast-forward or frontier.)
func TestCodeJS_CPUBoundLoop_KilledByInterrupt(t *testing.T) {
	root := writeAgent(t, "spin", `function run(){ while(true){} }`)
	p := New(Config{CodeRoot: root, RunTimeout: 100 * time.Millisecond})
	res := drive(t, context.Background(), p, "spin", "go", nil, nil)
	if res.errText == "" {
		t.Fatal("expected EventError from an interrupted CPU-bound loop")
	}
}

// Cross-run reproducibility (LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1) pins
// Date.now() to the fixed epoch.
func TestCodeJS_DeterministicMode(t *testing.T) {
	root := writeAgent(t, "det", `function run(){ return {final_text: Date.now() + "|" + (typeof Math.random())}; }`)
	p := New(Config{CodeRoot: root, RunTimeout: time.Second, Deterministic: true})
	res := drive(t, context.Background(), p, "det", "go", nil, nil)
	if !strings.HasPrefix(res.finalText, "1700000000000|") {
		t.Errorf("deterministic Date.now() not applied: %q", res.finalText)
	}
}

// Ambient determinism is ALWAYS on (Appendix B): Math.random()/Date.now() are
// seeded/anchored per run, so re-executions reproduce them — the property that
// makes replay divergence-free. Two runs of the same agent with the same
// RunMeta produce byte-identical tool inputs, and Date.now() returns the
// anchor (here the fixed-epoch fallback, since the test sets no StartedAt),
// not a real wall-clock value.
func TestCodeJS_AmbientDeterminism_StableAcrossReplays(t *testing.T) {
	js := `function run(){ mcp__probe__emit({ r: Math.random(), t: Date.now() }); return {final_text:"ok"}; }`
	root := writeAgent(t, "seeded", js)
	p := newTestProvider(root)
	ff := func(string, json.RawMessage) (string, bool) { return `{}`, false }

	r1 := drive(t, context.Background(), p, "seeded", "go", []providers.ToolSpec{{Name: "mcp__probe__emit"}}, ff)
	r2 := drive(t, context.Background(), p, "seeded", "go", []providers.ToolSpec{{Name: "mcp__probe__emit"}}, ff)
	if len(r1.toolCalls) == 0 || len(r2.toolCalls) == 0 {
		t.Fatal("expected a tool call")
	}
	if string(r1.toolCalls[0].Input) != string(r2.toolCalls[0].Input) {
		t.Errorf("seeded ambient not stable across runs:\n r1=%s\n r2=%s", r1.toolCalls[0].Input, r2.toolCalls[0].Input)
	}
	if !strings.Contains(string(r1.toolCalls[0].Input), `"t":1700000000000`) {
		t.Errorf("Date.now() should return the anchor, got %s", r1.toolCalls[0].Input)
	}
}

// Divergence guard: if a replay's tool-call sequence no longer matches the
// recorded transcript (e.g. the agent's control flow changed, or allowed_tools
// changed mid-run), the run fails loud rather than feeding a mismatched result
// into the JS. Forced here with a transcript whose first recorded call is
// "Channel" while the JS calls "Memory" first.
func TestCodeJS_ReplayDivergence_FailsLoud(t *testing.T) {
	root := writeAgent(t, "div", `function run(){ memory.get({key:"a", scope:"user"}); return {final_text:"x"}; }`)
	p := newTestProvider(root)
	ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{AgentName: "div"})
	req := providers.Request{
		Tools: []providers.ToolSpec{{Name: "Memory"}, {Name: "Channel"}},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "go"}}},
			{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: "cj-1-0", ToolName: "Channel", ToolInput: json.RawMessage(`{"op":"publish"}`)}}},
			{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: "cj-1-0", Text: "{}"}}},
		},
	}
	ch, err := p.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var gotErr string
	for ev := range ch {
		if ev.Type == providers.EventError {
			gotErr = ev.Error
		}
	}
	if !strings.Contains(gotErr, "code_agent_replay_divergence") {
		t.Errorf("want code_agent_replay_divergence, got %q", gotErr)
	}
}

// Concurrent runs each build their own goja Runtime per Call — no shared
// Runtime/Value. The race detector guards the no-shared-state invariant.
func TestCodeJS_ConcurrentRuns_Isolated(t *testing.T) {
	js := `
function run(input) {
  var n = memory.get({ scope: "user", key: "k" });
  memory.set({ scope: "user", key: "k", value: (n || 0) + 1 });
  return { final_text: "p=" + input.prompt };
}`
	root := writeAgent(t, "conc", js)
	p := newTestProvider(root)

	const N = 24
	var wg sync.WaitGroup
	errs := make(chan string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := drive(t, context.Background(), p, "conc", fmt.Sprintf("run-%d", i), []providers.ToolSpec{{Name: "Memory"}},
				func(name string, input json.RawMessage) (string, bool) {
					if strings.Contains(string(input), `"op":"get"`) {
						return `7`, false
					}
					return `{"ok":true}`, false
				})
			if res.errText != "" {
				errs <- res.errText
				return
			}
			if !strings.Contains(res.finalText, fmt.Sprintf("p=run-%d", i)) {
				errs <- fmt.Sprintf("run %d got wrong final text %q (state bled across runtimes?)", i, res.finalText)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Compile returns a stable content hash (the provider.code_hash lineage field).
func TestCodeJS_Compile_Hash(t *testing.T) {
	root := writeAgent(t, "h", `function run(){ return {final_text:"x"}; }`)
	p := newTestProvider(root)
	h1, err := p.Compile("h")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := p.Compile("h")
	if h1 == "" || h1 != h2 || len(h1) != 64 {
		t.Errorf("hash should be a stable 64-hex sha256; got %q / %q", h1, h2)
	}
}
