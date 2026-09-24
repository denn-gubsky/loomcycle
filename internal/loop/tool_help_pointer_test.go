package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

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

	provider := &fakeProvider{responses: [][]providers.Event{{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}}
	// A run with an agent name but no user id: Path's user scope cannot resolve,
	// so the description must say so rather than offer it.
	ctx := tools.WithAgentName(context.Background(), "helper")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1"})
	if _, err := Run(ctx, RunOptions{
		Provider:   provider,
		Model:      "fake-model",
		Tools:      order,
		Dispatcher: tools.NewDispatcher(order),
		Segments: []PromptSegment{
			{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
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
		`call Context with {"op":"help","topic":"Path"}; for one operation: {"op":"help","topic":"Path/resolve"}.`,
		`Scopes this run may use: agent, tenant (not user).`,
		`{"op":"help","topic":"scopes"}`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("Path description sent to the model lacks %q:\n%s", want, desc)
		}
	}
}
