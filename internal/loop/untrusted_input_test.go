package loop

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ctxProbeProvider records what the loop placed on the ctx of each call, and
// asks for one tool call before it ends the run.
type ctxProbeProvider struct {
	fakeProvider
	meta []providers.RunMeta
}

func (p *ctxProbeProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	m, _ := providers.RunMetaFromContext(ctx)
	p.meta = append(p.meta, m)
	return p.fakeProvider.Call(ctx, req)
}

// ctxProbeTool records the run's untrusted input as a tool sees it.
type ctxProbeTool struct{ seen [][]tools.UntrustedInput }

func (t *ctxProbeTool) Name() string                 { return "Probe" }
func (t *ctxProbeTool) Description() string          { return "" }
func (t *ctxProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *ctxProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	t.seen = append(t.seen, tools.RunUntrusted(ctx))
	return tools.Result{Text: "ok"}, nil
}

func probeRun(t *testing.T, ctx context.Context, opts RunOptions) (*ctxProbeProvider, *ctxProbeTool) {
	t.Helper()
	prov := &ctxProbeProvider{fakeProvider: fakeProvider{responses: [][]providers.Event{
		{{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Probe", Input: json.RawMessage(`{}`)}},
			{Type: providers.EventDone, StopReason: "tool_use"}},
		{{Type: providers.EventText, Text: "ok"}, {Type: providers.EventDone, StopReason: "end_turn"}},
	}}}
	probe := &ctxProbeTool{}
	opts.Provider, opts.Model = prov, "fake"
	opts.Tools, opts.Dispatcher = []tools.Tool{probe}, tools.NewDispatcher([]tools.Tool{probe})
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.meta) != 2 || len(probe.seen) != 1 {
		t.Fatalf("provider called %d times, tool %d; want 2 and 1", len(prov.meta), len(probe.seen))
	}
	return prov, probe
}

// The run's untrusted blocks reach a provider and the run's tools as data:
// the user-role blocks only, raw, in order, each under the kind it is
// rendered with, and the same on every call of the run.
func TestRun_StampsTheRunsUntrustedInputForProviderAndTools(t *testing.T) {
	prov, probe := probeRun(t, context.Background(), RunOptions{Segments: []PromptSegment{
		{Role: "system", Content: []PromptContentBlock{{Type: "untrusted-block", Text: "not the user's turn"}}},
		{Role: "user", Content: []PromptContentBlock{
			{Type: "trusted-text", Text: "Score it."},
			{Type: "untrusted-block", Kind: "web_content", Text: "a <b>page</b>"},
			{Type: "untrusted-block", Kind: "system", Text: "second"},
			{Type: "untrusted-block", Text: ""},
		}},
	}})
	wantMeta := []providers.UntrustedEntry{{Kind: "web_content", Text: "a <b>page</b>"}, {Kind: "untrusted", Text: "second"}, {Kind: "untrusted", Text: ""}}
	for i, m := range prov.meta {
		if !reflect.DeepEqual(m.Untrusted, wantMeta) {
			t.Errorf("call %d: RunMeta.Untrusted = %+v, want %+v", i, m.Untrusted, wantMeta)
		}
	}
	wantTool := []tools.UntrustedInput{{Kind: "web_content", Text: "a <b>page</b>"}, {Kind: "untrusted", Text: "second"}, {Kind: "untrusted", Text: ""}}
	if !reflect.DeepEqual(probe.seen[0], wantTool) {
		t.Errorf("tools.RunUntrusted = %+v, want %+v", probe.seen[0], wantTool)
	}
}

// Each run's value is its own. A sub-run's ctx descends from its parent's
// dispatch ctx, so a run with no untrusted input must REPLACE what is there:
// left in place, a child could forward its parent's input as its own.
func TestRun_ReplacesAnOuterRunsUntrustedInputEvenWithNone(t *testing.T) {
	outer := tools.WithRunUntrusted(context.Background(), []tools.UntrustedInput{{Kind: "untrusted", Text: "the parent's evidence"}})
	prov, probe := probeRun(t, outer, RunOptions{Segments: []PromptSegment{
		{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
	}})
	if len(probe.seen[0]) != 0 {
		t.Errorf("the run's tools see an outer run's untrusted input: %+v", probe.seen[0])
	}
	if len(prov.meta[0].Untrusted) != 0 {
		t.Errorf("the provider sees an outer run's untrusted input: %+v", prov.meta[0].Untrusted)
	}
}

// A run re-entered from the store has no user segment; its caller hands the
// value in, and that is what the run reads.
func TestRun_UsesTheUntrustedInputHandedInForAResumedRun(t *testing.T) {
	handed := []tools.UntrustedInput{{Kind: "user_input", Text: "from the stored turn"}}
	prov, probe := probeRun(t, context.Background(), RunOptions{
		Segments:      []PromptSegment{{Role: "system", Content: []PromptContentBlock{{Type: "trusted-text", Text: "sys"}}}},
		PriorMessages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "Score it.<user_input>\nfrom the stored turn\n</user_input>"}}}},
		Untrusted:     handed,
	})
	if !reflect.DeepEqual(probe.seen[0], handed) {
		t.Errorf("tools.RunUntrusted = %+v, want %+v", probe.seen[0], handed)
	}
	if want := []providers.UntrustedEntry{{Kind: "user_input", Text: "from the stored turn"}}; !reflect.DeepEqual(prov.meta[0].Untrusted, want) {
		t.Errorf("RunMeta.Untrusted = %+v, want %+v", prov.meta[0].Untrusted, want)
	}
}
