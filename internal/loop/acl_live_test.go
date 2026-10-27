package loop

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func liveCompiled(t *testing.T, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	rules = append([]authz.Rule{{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"volume:unrelated"}}}, rules...)
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "t", ID: "u"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// A forced tool_choice naming a tool the caller's rules withdrew mid-run (or
// asking for any tool when none is left) falls back to auto: a provider
// refuses a choice its schema cannot satisfy. A choice the schema still
// satisfies is kept, and with no live view the request is untouched.
func TestLiveToolSchema_AForcedChoiceOfAWithdrawnToolFallsBackToAuto(t *testing.T) {
	start := liveCompiled(t)
	current := start
	resolve := func(context.Context, authz.Subject) (*authz.Effective, error) { return current, nil }
	ctx := tools.WithRunPolicy(authz.WithMode(context.Background(), authz.ModeCompat),
		tools.RunPolicy{Tools: []string{"Read", "Bash"}, ACL: start, LiveACL: tools.NewLiveACL(start, resolve)})
	specs := []providers.ToolSpec{{Name: "Read"}, {Name: "Bash"}}

	req := providers.Request{Tools: specs, ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "Bash"}}
	liveToolSchema(ctx, &req)
	if len(req.Tools) != 2 || req.ToolChoice.Name != "Bash" {
		t.Fatalf("unchanged set: request changed: %+v", req)
	}

	current = liveCompiled(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}})
	req = providers.Request{Tools: specs, ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "Bash"}}
	liveToolSchema(ctx, &req)
	if len(req.Tools) != 1 || req.Tools[0].Name != "Read" || req.ToolChoice != (providers.ToolChoice{}) {
		t.Errorf("withdrawn forced tool: tools %v choice %+v, want [Read] and auto", req.Tools, req.ToolChoice)
	}
	req = providers.Request{Tools: specs, ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "Read"}}
	liveToolSchema(ctx, &req)
	if req.ToolChoice.Name != "Read" {
		t.Errorf("a forced tool still offered must stay forced: %+v", req.ToolChoice)
	}

	current = liveCompiled(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:*"}})
	req = providers.Request{Tools: specs, ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceRequired}}
	liveToolSchema(ctx, &req)
	if len(req.Tools) != 0 || req.ToolChoice != (providers.ToolChoice{}) {
		t.Errorf("no tool left: tools %v choice %+v, want none and auto", req.Tools, req.ToolChoice)
	}

	req = providers.Request{Tools: specs, ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceRequired}}
	liveToolSchema(tools.WithRunPolicy(context.Background(), tools.RunPolicy{Tools: []string{"Read", "Bash"}, ACL: start}), &req)
	if len(req.Tools) != 2 || req.ToolChoice.Mode != providers.ToolChoiceRequired {
		t.Errorf("no live view: request changed: %+v", req)
	}
}
