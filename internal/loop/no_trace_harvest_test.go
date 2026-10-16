package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// notRetainedTool answers with a no-trace run's content, as the Agent tool
// does with a no-trace child's answer: it marks its call.
type notRetainedTool struct{ reply string }

func (n *notRetainedTool) Name() string                 { return "Echo" }
func (n *notRetainedTool) Description() string          { return "hands back a no-trace answer" }
func (n *notRetainedTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n *notRetainedTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	tools.MarkContentNotRetained(ctx)
	return tools.Result{Text: n.reply}, nil
}

// A stateful run banks each step's observation for the consolidator. An
// observation that is a no-trace run's content is stored nowhere, so it is
// not banked either; the run's own reasoning still is.
func TestRun_Stateful_DoesNotBankANoTraceObservation(t *testing.T) {
	const secret = "NO-TRACE-OBSERVATION-31f9"
	prov := &statefulScriptProvider{scripts: []string{
		`{"reasoning":"begin","patch":{"count":0},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":1},"done":true,"final":"done"}`,
	}}
	tool := &notRetainedTool{reply: secret}
	sc := statefulCtx(nil)
	sc.HarvestToMemory = cptr(true)
	var banked []providers.Message
	opts := RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{tool},
		Dispatcher: tools.NewDispatcher([]tools.Tool{tool}),
		Segments:   statefulTaskSegs(),
		Context:    sc,
		OnEvent:    func(providers.Event) {},
		BankCompactedSpan: func(_ context.Context, span []providers.Message) (string, error) {
			banked = append(banked, span...)
			return "mp", nil
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("stateful run: %v", err)
	}
	if len(banked) == 0 {
		t.Fatal("nothing was banked: this test cannot detect a leak")
	}
	b, _ := json.Marshal(banked)
	if strings.Contains(string(b), secret) {
		t.Errorf("a no-trace observation was banked: %s", b)
	}
}
