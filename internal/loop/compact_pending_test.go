package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// bigResultTool returns one large result.
type bigResultTool struct{ text string }

func (bigResultTool) Name() string                 { return "FakeRead" }
func (bigResultTool) Description() string          { return "" }
func (bigResultTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b bigResultTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: b.text}, nil
}

// A tool result that pushes the NEXT request over the auto-compaction
// threshold is compacted before that request goes out — the last call's
// measured input was under the threshold, so a gate that read only it let the
// oversized request through first. The new tool result is kept verbatim.
func TestRun_AutoCompactCountsTheToolResultsAboutToBeSent(t *testing.T) {
	big := "RESULT " + strings.Repeat("x", 4400) // ~1100 tokens
	prov := &fakeProvider{responses: [][]providers.Event{
		// Iteration 0: measured input 1250 of a 4000 window — under 50%.
		{toolCall("t1"), {Type: providers.EventDone, StopReason: "tool_use", Usage: usage(1250, 5)}},
		// The compaction's summary call.
		{{Type: providers.EventText, Text: "earlier: an old exchange"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: usage(10, 5)}},
		// Iteration 1.
		{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: usage(10, 1)}},
	}}
	tool := bigResultTool{text: big}
	var events []providers.Event
	old := strings.Repeat("an old exchange worth summarising. ", 70) // ~600 tokens each
	res, err := Run(context.Background(), RunOptions{
		Provider:         prov,
		Model:            "fake-model",
		Tools:            []tools.Tool{tool},
		Dispatcher:       tools.NewDispatcher([]tools.Tool{tool}),
		MaxContextTokens: 4000,
		Compaction:       &config.Compaction{Enabled: cptr(true), AutoCompactAtPct: cptr(50), KeepLastN: cptr(2)},
		PriorMessages:    []providers.Message{userMsg("old task " + old), asstMsg("old answer " + old)},
		Segments:         []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "new question"}}}},
		OnEvent:          func(ev providers.Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	compacted := false
	for _, ev := range events {
		if ev.Type == providers.EventContextCompaction {
			compacted = true
		}
	}
	if !compacted || res.FinalText != "done" || len(prov.calls) != 3 {
		t.Fatalf("compacted=%v final=%q calls=%d; want a compaction before iteration 1, then its answer", compacted, res.FinalText, len(prov.calls))
	}
	var sawResult, sawOld bool
	for _, m := range prov.calls[2].Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.Text == big {
				sawResult = true
			}
			if strings.Contains(c.Text, "old answer") {
				sawOld = true
			}
		}
	}
	if !sawResult || sawOld {
		t.Errorf("after compaction: tool result kept verbatim = %v, old answer still sent = %v; want true, false", sawResult, sawOld)
	}
}
