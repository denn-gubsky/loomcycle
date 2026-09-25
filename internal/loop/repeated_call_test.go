package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// refusingTool always fails, as a scope refusal does.
type refusingTool struct{ calls int }

func (r *refusingTool) Name() string                 { return "Memory" }
func (r *refusingTool) Description() string          { return "stores things" }
func (r *refusingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *refusingTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	r.calls++
	return tools.Result{Text: `scope "tenant" not granted`, IsError: true}, nil
}

// A stateful run that keeps sending the same refused call ends with
// repeated_failed_call instead of spinning to its deadline — measured live, a
// refused tenant write re-sent until a 10-minute timeout.
func TestRun_Stateful_ARunRepeatingAFailedCallIsStopped(t *testing.T) {
	step := `{"patch":{},"action":{"tool":"Memory","input":{"op":"set","scope":"tenant","key":"k"}}}`
	scripts := make([]string, 20)
	for i := range scripts {
		scripts[i] = step
	}
	prov := &statefulScriptProvider{scripts: scripts}
	mem := &refusingTool{}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{mem},
		Dispatcher: tools.NewDispatcher([]tools.Tool{mem}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	})
	if err == nil || res.StopReason != StopReasonRepeatedFailedCall || !strings.Contains(err.Error(), "same failed Memory call") {
		t.Fatalf("stop=%q err=%v; want repeated_failed_call", res.StopReason, err)
	}
	if mem.calls != 2 {
		t.Errorf("the tool ran %d times; want 2 (the repeats after that are refused, not run)", mem.calls)
	}
	if prov.calls() > 6 {
		t.Errorf("%d model calls; the run should stop soon after the refusals, not at its cap", prov.calls())
	}
}
