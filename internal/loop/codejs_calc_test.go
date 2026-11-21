package loop_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// countingCalc is the real Calc tool, counting how often it is executed.
type countingCalc struct {
	builtin.Calc
	n atomic.Int32
}

func (c *countingCalc) Execute(ctx context.Context, in json.RawMessage) (tools.Result, error) {
	c.n.Add(1)
	return c.Calc.Execute(ctx, in)
}

func runCalcAgent(t *testing.T, js string, h *hooks.Dispatcher) (loop.RunResult, *countingCalc) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "calculator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "calculator", "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	calc := &countingCalc{}
	res, err := loop.Run(context.Background(), loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 5 * time.Second}),
		Model:      "code-js",
		AgentName:  "calculator",
		Tools:      []tools.Tool{calc},
		Dispatcher: tools.NewDispatcher([]tools.Tool{calc}),
		Hooks:      h,
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	return res, calc
}

// A code agent calls Calc through the loop, and the replay that re-runs its
// script on each turn feeds back the recorded results: three calls evaluate
// three times, not once per call per turn (1+2+3).
func TestCodeJS_CalcCallsAreEvaluatedOnceEachAcrossReplay(t *testing.T) {
	js := `
function run(input) {
  var a = JSON.parse(Calc({ expr: "1249.99 * 3" })).text;
  var b = JSON.parse(Calc({ expr: "round(x * 0.2, 2)", vars: { x: a } })).text;
  var c = JSON.parse(Calc({ expr: "x + y", vars: { x: a, y: b } })).text;
  return { final_text: "total=" + c };
}`
	res, calc := runCalcAgent(t, js, nil)
	if res.FinalText != "total=4499.96" {
		t.Errorf("final text = %q, want total=4499.96", res.FinalText)
	}
	if n := calc.n.Load(); n != 3 {
		t.Errorf("Calc executed %d times, want 3: replay must not re-evaluate a recorded call", n)
	}
}

// A pre-tool-use hook can refuse a Calc call like any other: the tool does not
// run and the code agent sees the hook's refusal as a thrown error.
func TestCodeJS_PreHookRefusesACalcCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call hooks.PreHookCall
		_ = json.NewDecoder(r.Body).Decode(&call)
		out := hooks.PreHookResult{}
		if call.ToolCall.Name == "Calc" {
			out.Deny = &hooks.ToolResult{Text: "Calc is closed for maintenance", IsError: true}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	reg := hooks.NewSet()
	if _, err := reg.Register(&hooks.Hook{OperatorConfig: true, Owner: "test", Name: "no-calc",
		Phase: hooks.PhasePre, CallbackURL: srv.URL, Tools: []string{"Calc"}}); err != nil {
		t.Fatal(err)
	}
	js := `
function run(input) {
  try { Calc({ expr: "2 + 2" }); return { final_text: "ran" }; }
  catch (e) { return { final_text: "refused: " + e }; }
}`
	res, calc := runCalcAgent(t, js, hooks.NewDispatcher(reg, nil))
	if !strings.Contains(res.FinalText, "Calc is closed for maintenance") {
		t.Errorf("final text = %q, want the hook's refusal", res.FinalText)
	}
	if n := calc.n.Load(); n != 0 {
		t.Errorf("Calc executed %d times behind a denying hook", n)
	}
}
