package loop

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/hooks/codehook"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// relayInterruption stands in for the Interruption tool with an mcp_server
// backend: it delivers each question through a consumer's tool with
// tools.ExecuteHooked, as the real one does. maxDepth caps the calls so a
// re-entry is counted instead of overflowing the stack.
type relayInterruption struct {
	disp     *tools.Dispatcher
	calls    atomic.Int32
	maxDepth int32
}

func (*relayInterruption) Name() string                 { return "Interruption" }
func (*relayInterruption) Description() string          { return "" }
func (*relayInterruption) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *relayInterruption) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	if r.calls.Add(1) > r.maxDepth {
		return tools.Result{Text: "depth cap reached", IsError: true}, nil
	}
	res := tools.ExecuteHooked(ctx, r.disp, "mcp__consumer__ask", input)
	out, _ := json.Marshal(map[string]string{"answer": res.Text})
	return tools.Result{Text: string(out)}, nil
}

// consumerAsk is the consumer's ask tool: it answers every question "allow".
type consumerAsk struct{ calls atomic.Int32 }

func (*consumerAsk) Name() string                 { return "mcp__consumer__ask" }
func (*consumerAsk) Description() string          { return "" }
func (*consumerAsk) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *consumerAsk) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	c.calls.Add(1)
	return tools.Result{Text: "allow"}, nil
}

// A code hook's question, delivered through a consumer's tool, does not go back
// through the hooks. It used to: the post hook ran on the tool call's ctx, which
// carries the loop's hooked executor, so the delivery call reached the same
// hook, which asked again — a pending question per level, then a stack
// overflow.
func TestLoop_ACodeHooksAskDeliveredThroughAToolDoesNotReenterTheHooks(t *testing.T) {
	ask := &consumerAsk{}
	fetch := &fakeWebFetch{result: tools.Result{Text: "page"}}
	disp := tools.NewDispatcher([]tools.Tool{fetch, ask})
	intr := &relayInterruption{disp: disp, maxDepth: 26}

	reg := hooks.NewSet()
	if _, err := reg.Register(&hooks.Hook{Owner: "ops", Name: "review", Phase: hooks.PhasePost, Code: `
		function hook(ev) {
			var a = Interruption.ask({question: "Show the model the result of " + ev.tool_call.name + "?"});
			return a === "allow" ? {} : {updated_output: {text: "withheld"}};
		}`}); err != nil {
		t.Fatal(err)
	}
	hd := hooks.NewDispatcher(reg, nil)
	hd.SetCodeRunner(codehook.New(intr))

	prov := &scriptedProvider{toolCalls: []providers.ToolUse{{ID: "c1", Name: "WebFetch", Input: json.RawMessage(`{}`)}}}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Tools: []tools.Tool{fetch, ask}, Dispatcher: disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 1, AgentName: "any", Hooks: hd,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := intr.calls.Load(); n != 1 {
		t.Errorf("the hook asked %d times, want once: its question's delivery went back through the hooks", n)
	}
	if n := ask.calls.Load(); n != 1 {
		t.Errorf("the consumer's ask tool ran %d times, want once", n)
	}
}
