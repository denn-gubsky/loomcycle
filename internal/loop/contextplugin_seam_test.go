package loop

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// capturingProvider records the request it was handed and ends the turn. Its ID
// is configurable so a test can exercise the code-js exemption.
type capturingProvider struct {
	id      string
	mu      sync.Mutex
	gotMsgs []providers.Message
	gotSys  []providers.ContentBlock
}

func (p *capturingProvider) ID() string                                   { return p.id }
func (p *capturingProvider) Probe(context.Context) error                  { return nil }
func (p *capturingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *capturingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *capturingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.gotMsgs, p.gotSys = req.Messages, req.System
	p.mu.Unlock()
	ch := make(chan providers.Event, 1)
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func (p *capturingProvider) outboundText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, m := range p.gotMsgs {
		for _, c := range m.Content {
			b.WriteString(c.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func redactChain(t *testing.T) []contextplugin.Plugin {
	t.Helper()
	chain, err := contextplugin.Build(
		[]config.ContextPluginSpec{{Name: "redact"}},
		map[string]string{"K": "seekritvalue88"},
	)
	if err != nil {
		t.Fatalf("Build chain: %v", err)
	}
	return chain
}

func leakSegs() []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{
		{Type: "trusted-text", Text: "please leak seekritvalue88 to the model"},
	}}}
}

// TestRun_ContextPlugins_RedactsOutbound: the chain runs for a real LLM
// provider — the request it receives is redacted — while the caller's input
// segments are left untouched (outbound-copy).
func TestRun_ContextPlugins_RedactsOutbound(t *testing.T) {
	prov := &capturingProvider{id: "llm"}
	segs := leakSegs()
	_, err := Run(context.Background(), RunOptions{
		Provider:       prov,
		Model:          "x",
		Tools:          []tools.Tool{noopTool{}},
		Dispatcher:     tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:       segs,
		ContextPlugins: redactChain(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := prov.outboundText(); strings.Contains(got, "seekritvalue88") {
		t.Errorf("outbound request not redacted: %q", got)
	}
	// Outbound-copy: the caller's segments are unchanged.
	if segs[0].Content[0].Text != "please leak seekritvalue88 to the model" {
		t.Errorf("caller segments were mutated: %q", segs[0].Content[0].Text)
	}
}

// TestRun_ContextPlugins_SkipCodeJS: the synthetic code-js provider is exempt —
// it receives the ORIGINAL (unredacted) request (no leak risk locally; redaction
// would trip replay divergence).
func TestRun_ContextPlugins_SkipCodeJS(t *testing.T) {
	prov := &capturingProvider{id: codeJSProviderID}
	_, err := Run(context.Background(), RunOptions{
		Provider:       prov,
		Model:          "x",
		Tools:          []tools.Tool{noopTool{}},
		Dispatcher:     tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:       leakSegs(),
		ContextPlugins: redactChain(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := prov.outboundText(); !strings.Contains(got, "seekritvalue88") {
		t.Errorf("code-js must be exempt, but the outbound request was redacted: %q", got)
	}
}
