package loop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// windowedProvider reports a fixed context window from Capabilities over an
// otherwise-scripted provider, so a test can give a run a model window
// without configuring max_context_tokens. A non-zero reported is stamped on
// every call's usage, the way Ollama reports the context it actually loaded.
type windowedProvider struct {
	providers.Provider
	window   int
	reported int
}

func (p windowedProvider) Capabilities() providers.Capabilities {
	c := p.Provider.Capabilities()
	c.MaxContextTokens = p.window
	return c
}

func (p windowedProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	in, err := p.Provider.Call(ctx, req)
	if err != nil || p.reported == 0 {
		return in, err
	}
	out := make(chan providers.Event, cap(in))
	go func() {
		defer close(out)
		for ev := range in {
			if ev.Type == providers.EventDone && ev.Usage != nil {
				u := *ev.Usage
				u.MaxContextTokens = p.reported
				ev.Usage = &u
			}
			out <- ev
		}
	}()
	return out, nil
}

// windowProbeTool records the two window values a dispatched tool sees.
type windowProbeTool struct {
	effective  int
	configured int
}

func (t *windowProbeTool) Name() string                 { return "Echo" }
func (t *windowProbeTool) Description() string          { return "" }
func (t *windowProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *windowProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	t.effective = tools.EffectiveContextWindow(ctx)
	t.configured = tools.MaxContextTokens(ctx)
	return tools.Result{Text: "ok"}, nil
}

var windowCases = []struct {
	name                      string
	capability, configured    int
	wantEffective, wantConfig int
}{
	// The case the stamp exists for: a small local model with no
	// max_context_tokens still has a window, and its tools must see it.
	{"capability only", 32768, 0, 32768, 0},
	{"configured below capability", 128000, 16384, 16384, 16384},
	{"configured above capability", 32768, 65536, 32768, 65536},
}

func TestRun_ToolSeesEffectiveContextWindowOnFirstCall(t *testing.T) {
	for _, tc := range windowCases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &windowProbeTool{}
			prov := windowedProvider{
				Provider: &scriptedProvider{toolCalls: []providers.ToolUse{
					{ID: "call_0", Name: "Echo", Input: json.RawMessage(`{}`)},
				}},
				window: tc.capability,
			}
			if _, err := Run(context.Background(), RunOptions{
				Provider:         prov,
				Model:            "scripted-model",
				Tools:            []tools.Tool{probe},
				Dispatcher:       tools.NewDispatcher([]tools.Tool{probe}),
				Segments:         []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
				MaxContextTokens: tc.configured,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if probe.effective != tc.wantEffective {
				t.Errorf("EffectiveContextWindow on tool ctx = %d, want %d", probe.effective, tc.wantEffective)
			}
			// The configured cap keeps its meaning: op=self reports it as set.
			if probe.configured != tc.wantConfig {
				t.Errorf("MaxContextTokens on tool ctx = %d, want the configured %d", probe.configured, tc.wantConfig)
			}
		})
	}
}

func TestRun_Stateful_ActionSeesEffectiveContextWindow(t *testing.T) {
	for _, tc := range windowCases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &windowProbeTool{}
			prov := windowedProvider{
				Provider: &actionScriptProvider{scripts: []string{
					`{"patch":{},"action":{"tool":"Echo","input":{}}}`,
					`{"patch":{},"done":true,"final":"ok"}`,
				}},
				window: tc.capability,
			}
			if _, err := Run(context.Background(), RunOptions{
				Provider: prov, Model: "x",
				Tools:            []tools.Tool{probe},
				Dispatcher:       tools.NewDispatcher([]tools.Tool{probe}),
				Segments:         statefulTaskSegs(),
				Context:          statefulCtx(nil),
				MaxContextTokens: tc.configured,
				OnEvent:          func(providers.Event) {},
			}); err != nil {
				t.Fatalf("run: %v", err)
			}
			if probe.effective != tc.wantEffective {
				t.Errorf("EffectiveContextWindow on action ctx = %d, want %d", probe.effective, tc.wantEffective)
			}
			if probe.configured != tc.wantConfig {
				t.Errorf("MaxContextTokens on action ctx = %d, want the configured %d", probe.configured, tc.wantConfig)
			}
		})
	}
}

// The window a driver reports on the call that asked for the tool beats the
// static capability already on that first call — the stamp must sit after the
// call, not at the top of the iteration where only the seed is known.
func TestRun_ToolSeesDriverReportedWindowOnFirstCall(t *testing.T) {
	probe := &windowProbeTool{}
	prov := windowedProvider{
		Provider: &scriptedProvider{toolCalls: []providers.ToolUse{
			{ID: "call_0", Name: "Echo", Input: json.RawMessage(`{}`)},
		}},
		window:   4096,
		reported: 40960,
	}
	if _, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "scripted-model",
		Tools:      []tools.Tool{probe},
		Dispatcher: tools.NewDispatcher([]tools.Tool{probe}),
		Segments:   []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.effective != 40960 {
		t.Errorf("EffectiveContextWindow on tool ctx = %d, want the driver-reported 40960 (capability says 4096)", probe.effective)
	}
}
