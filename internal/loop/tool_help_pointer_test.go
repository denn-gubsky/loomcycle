// An EXTERNAL test package: it runs the loop with the real builtin tools, and
// builtin reaches the loop package through the error classifier (builtin →
// errclassify → runner → loop). An in-package test importing builtin would be
// an import cycle.
package loop_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// recordingProvider answers every call with one end_turn reply and records the
// requests it was sent.
type recordingProvider struct {
	mu    sync.Mutex
	calls []providers.Request
}

func (p *recordingProvider) ID() string                  { return "fake" }
func (p *recordingProvider) Probe(context.Context) error { return nil }
func (p *recordingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"fake-model"}, nil
}
func (p *recordingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *recordingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

// The crossing: what the MODEL receives. The pointer and the scope grants are
// built from three things that each have their own tests — the help corpus, the
// Context tool, and each tool's scope check — and none of those tests shows the
// loop actually sending the result. This one runs a real loop with the real
// Context tool and bundled help, and reads the tool array off the request.
func TestRun_ToolDescriptionsCarryHelpPointerAndThisRunsScopes(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	pathTool := &builtin.Path{}
	ctxTool := &builtin.Context{Help: set}
	order := []tools.Tool{pathTool, ctxTool}

	provider := &recordingProvider{}
	// A run with an agent name but no user id: Path's user scope cannot resolve,
	// so the description must say so rather than offer it.
	ctx := tools.WithAgentName(context.Background(), "helper")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1"})
	if _, err := loop.Run(ctx, loop.RunOptions{
		Provider:   provider,
		Model:      "fake-model",
		Tools:      order,
		Dispatcher: tools.NewDispatcher(order),
		Segments: []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.calls) == 0 {
		t.Fatal("provider never called")
	}
	var desc string
	for _, s := range provider.calls[0].Tools {
		if s.Name == "Path" {
			desc = s.Description
		}
	}
	for _, want := range []string{
		pathTool.Description(),
		`Before your first call to Path, read the call format for the operation you need: call Context with {"op":"help","topic":"Path/resolve"}`,
		`Scopes this run may use: agent, tenant (not user).`,
		`{"op":"help","topic":"scopes"}`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("Path description sent to the model lacks %q:\n%s", want, desc)
		}
	}
}
