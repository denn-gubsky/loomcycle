package loop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// probeProviderTool captures the resolved provider/model that the loop
// stamps onto the per-iteration ctx, proving the values reach a dispatched
// tool (the path the Context tool's op=self reads).
type probeProviderTool struct {
	gotProvider string
	gotModel    string
}

func (t *probeProviderTool) Name() string        { return "Probe" }
func (t *probeProviderTool) Description() string { return "" }
func (t *probeProviderTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *probeProviderTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	t.gotProvider = tools.ResolvedProvider(ctx)
	t.gotModel = tools.ResolvedModel(ctx)
	return tools.Result{Text: "ok"}, nil
}

// TestRun_StampsResolvedProviderModelOnToolCtx asserts loop.Run stamps the
// run's provider id + model name onto the ctx a dispatched tool receives —
// the seam Context op=self surfaces to the agent.
func TestRun_StampsResolvedProviderModelOnToolCtx(t *testing.T) {
	probe := &probeProviderTool{}
	prov := &scriptedProvider{toolCalls: []providers.ToolUse{
		{ID: "call_0", Name: "Probe", Input: json.RawMessage(`{}`)},
	}}
	disp := tools.NewDispatcher([]tools.Tool{probe})

	_, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "scripted-model",
		Tools:           []tools.Tool{probe},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 8,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.gotProvider != "scripted" {
		t.Errorf("ResolvedProvider on tool ctx = %q, want %q", probe.gotProvider, "scripted")
	}
	if probe.gotModel != "scripted-model" {
		t.Errorf("ResolvedModel on tool ctx = %q, want %q", probe.gotModel, "scripted-model")
	}
}
