package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run holds its definition's statement_methods only when the definition runs
// code. Load and AgentDef create/fork refuse the field on a model agent; this
// is the run-time floor for a definition that reached a run without either.
// A sub-agent holds its own definition's grant, never its parent's.
func TestAgentRunPolicy_StatementMethodsOnlyForACodeJSAgent(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	code, _ := h.srv.agentRunPolicy(ctx, config.AgentDef{Provider: "code-js", StatementMethods: []string{"rule", "extractor"}}, "linker", runPolicySource{})
	if got := strings.Join(code.StatementMethods, ","); got != "rule,extractor" {
		t.Errorf("code-js run: StatementMethods = %q, want rule,extractor", got)
	}
	model, _ := h.srv.agentRunPolicy(ctx, config.AgentDef{Provider: "anthropic", StatementMethods: []string{"rule"}}, "writer", runPolicySource{})
	if len(model.StatementMethods) != 0 {
		t.Errorf("a model-driven run holds statement methods %v", model.StatementMethods)
	}
	none, _ := h.srv.agentRunPolicy(ctx, config.AgentDef{Provider: "code-js"}, "child", runPolicySource{})
	if child := code.Inherit(tools.ChildGrant{Own: none}); len(child.StatementMethods) != 0 {
		t.Errorf("a sub-agent inherited its parent's statement methods %v", child.StatementMethods)
	}
}
