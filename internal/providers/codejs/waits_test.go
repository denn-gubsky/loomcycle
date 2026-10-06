package codejs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// These drive a code-js orchestrator through the REAL loop and the REAL Agent
// tool, with sub-agents that take longer than the orchestrator's whole budget.
// The budget is active time: an orchestrator blocked on its children is
// parked, so it completes with their results. Before the run clock, the time
// blocked counted, the next replay turn started over budget, and the run
// failed code_agent_timeout with the children's results discarded.

const (
	orchestratorBudget = time.Second
	childTakes         = 2 * time.Second // longer than the whole budget
)

// aggregate stands in for the work a real orchestrator does with its
// children's results once the wait returns. It matters to the fail-before: a
// replay turn that does no work at all can finish inside the 1 ms an
// over-budget turn used to be given, and pass without the run clock.
const aggregate = `
function aggregate(xs) {
  var s = 0;
  for (var i = 0; i < 300000; i++) { s += i % 7; }
  return xs.join(",");
}`

// slowChildren is a sub-agent runner whose every child takes childTakes.
func slowChildren(ctx context.Context, _ string, prompt string, _ string) (string, error) {
	select {
	case <-time.After(childTakes):
		return "child:" + prompt, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// slowTool is a tool that takes a while but is NOT a wait: its time counts.
type slowTool struct{ takes time.Duration }

func (s slowTool) Name() string                 { return "mcp__test__slow" }
func (s slowTool) Description() string          { return "takes a while" }
func (s slowTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s slowTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	time.Sleep(s.takes)
	return tools.Result{Text: `{"ok":true}`}, nil
}

// runOrchestrator runs js as a code agent with the given budget and tools.
func runOrchestrator(t *testing.T, js string, budget time.Duration, ts ...tools.Tool) (loop.RunResult, error) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "orch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := codejs.New(codejs.Config{CodeRoot: root, RunTimeout: budget})
	return loop.Run(context.Background(), loop.RunOptions{
		Provider:   prov,
		Model:      "code-js",
		AgentName:  "orch",
		Tools:      ts,
		Dispatcher: tools.NewDispatcher(ts),
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	})
}

func TestCodeJSOrchestrator_ParallelSpawnWaitDoesNotSpendBudget(t *testing.T) {
	js := aggregate + `
function run() {
  var r = Agent.spawn({op: "parallel_spawn", spawns: [{name: "kid", prompt: "a"}, {name: "kid", prompt: "b"}]});
  return { final_text: aggregate(r.results.map(function (x) { return x.output; })) };
}`
	res, err := runOrchestrator(t, js, orchestratorBudget, &builtin.AgentTool{Run: slowChildren})
	if err != nil {
		t.Fatalf("an orchestrator waiting %s on its children failed under a %s budget: %v", childTakes, orchestratorBudget, err)
	}
	if res.FinalText != "child:a,child:b" {
		t.Fatalf("final text = %q, want the children's results", res.FinalText)
	}
}

func TestCodeJSOrchestrator_SpawnWaitDoesNotSpendBudget(t *testing.T) {
	js := aggregate + `
function run() {
  var a = Agent.spawn({name: "kid", prompt: "a"});
  return { final_text: aggregate([String(a)]) };
}`
	res, err := runOrchestrator(t, js, orchestratorBudget, &builtin.AgentTool{Run: slowChildren})
	if err != nil {
		t.Fatalf("an orchestrator waiting %s on a spawn failed under a %s budget: %v", childTakes, orchestratorBudget, err)
	}
	if res.FinalText != "child:a" {
		t.Fatalf("final text = %q, want the child's result", res.FinalText)
	}
}

// A wait pauses the budget; it does not replace it. After waiting longer than
// its whole budget, a busy loop still times out, and the message says the
// wait did not count.
func TestCodeJSOrchestrator_BusyLoopAfterAWaitStillTimesOut(t *testing.T) {
	js := `
function run() {
  Agent.spawn({name: "kid", prompt: "a"});
  while (true) {}
}`
	start := time.Now()
	_, err := runOrchestrator(t, js, orchestratorBudget, &builtin.AgentTool{Run: slowChildren})
	if err == nil || !strings.Contains(err.Error(), "code_agent_timeout") {
		t.Fatalf("a busy loop must still time out; got %v", err)
	}
	if !strings.Contains(err.Error(), "budget of active time") || !strings.Contains(err.Error(), "which did not count") {
		t.Errorf("the timeout must say only active time counted and how long the run waited; got %q", err)
	}
	if took := time.Since(start); took < childTakes+orchestratorBudget/2 {
		t.Errorf("the run ended after %s — it timed out before spending its active budget after the wait", took)
	}
}

// A slow tool call that is not a wait counts like any other active time.
func TestCodeJSOrchestrator_SlowNonWaitToolStillCounts(t *testing.T) {
	js := `
function run() {
  mcp__test__slow({});
  return { final_text: "done" };
}`
	_, err := runOrchestrator(t, js, orchestratorBudget, slowTool{takes: orchestratorBudget + 500*time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "code_agent_timeout") {
		t.Fatalf("a slow non-waiting tool must spend the budget and time the run out; got %v", err)
	}
}

// Context op=self tells an orchestrator its budget, what it has used, and what
// it has waited.
func TestCodeJSOrchestrator_ContextSelfReportsTheRunBudget(t *testing.T) {
	js := `
function run() {
  Agent.spawn({name: "kid", prompt: "a"});
  return { final_text: Context({op: "self"}) };
}`
	res, err := runOrchestrator(t, js, 3*time.Second, &builtin.AgentTool{Run: slowChildren}, &builtin.Context{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var self struct {
		RunBudget map[string]int64 `json:"run_budget"`
	}
	if err := json.Unmarshal([]byte(res.FinalText), &self); err != nil {
		t.Fatalf("op=self output %q: %v", res.FinalText, err)
	}
	b := self.RunBudget
	if b == nil {
		t.Fatalf("op=self reported no run_budget: %s", res.FinalText)
	}
	if b["budget_ms"] != 3000 {
		t.Errorf("budget_ms = %d, want 3000", b["budget_ms"])
	}
	if b["waited_ms"] < childTakes.Milliseconds()-100 {
		t.Errorf("waited_ms = %d, want about %d (the spawn)", b["waited_ms"], childTakes.Milliseconds())
	}
	// Each figure is truncated to whole milliseconds on its own, so the
	// remainder may be one short of budget minus used.
	if d := b["budget_ms"] - b["used_ms"] - b["remaining_ms"]; b["used_ms"] >= 1000 || d < 0 || d > 1 {
		t.Errorf("used_ms = %d remaining_ms = %d: the wait was spent, or the remainder does not add up", b["used_ms"], b["remaining_ms"])
	}
}
