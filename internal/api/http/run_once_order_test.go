package http

import (
	"context"
	"go/ast"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// runOncePhaseOrder is the order RunOnce's phases run in. Several positions
// are security rules, not taste (see the comment above RunOnce); the pairs
// below name them so a failure says which rule moved.
var runOncePhaseOrder = []string{
	"rate", "validate", "identity", "session", "owner", "conditions", "agent",
	"replay", "provider", "pause", "budget", "agent_id", "run_context", "slot",
	"budget_at_start", "reresolve", "tools", "tools.filter", "tools.recall", "tools.acl", "tools.hosts",
	"prompt", "run_config", "rows",
	"wiring", "emit", "loop_ctx", "options", "launch",
}

// WHY: the step order of RunOnce is the run admission rule for every
// transport, and nothing else holds it in place. A future edit that moves the
// rate check behind validation, the budget behind the queue, or the ACL's tool
// removal behind the system prompt compiles and passes every other test.
func TestRunOnce_PhasesRunInTheirSecurityOrder(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:worker."},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	srv, _, _ := spawnCeilingServer(t, cfg, prov, nil)

	var mu sync.Mutex
	var got []string
	srv.runPhaseHook = func(phase string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, phase)
	}
	if err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "worker", AgentID: "a_order",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()

	for _, rule := range []struct{ before, after, why string }{
		{"rate", "validate", "the caller's rate is counted before anything else"},
		{"conditions", "agent", "the condition facts are stamped before the agent admission reads them"},
		{"budget", "slot", "a run over its budget is refused, not queued"},
		{"slot", "budget_at_start", "a queued run is asked its budget again when it starts"},
		{"tools", "prompt", "the ACL removes tools before the system prompt is built from them"},
		{"tools", "run_config", "the caller's narrowed host list is what the run record keeps"},
	} {
		b, a := slices.Index(got, rule.before), slices.Index(got, rule.after)
		if b < 0 || a < 0 || b > a {
			t.Errorf("%s must run before %s (%s); order was %s", rule.before, rule.after, rule.why, strings.Join(got, " > "))
		}
	}
	if !slices.Equal(got, runOncePhaseOrder) {
		t.Errorf("RunOnce phase order\n got: %s\nwant: %s", strings.Join(got, " > "), strings.Join(runOncePhaseOrder, " > "))
	}
}

// censusFuncName is the name a source census files a function under. RunOnce's
// phases (methods of runAdmission) are filed as RunOnce: they are one run
// start split for reading, and a census rule about what "RunOnce" does — counts
// the rate once, admits the principal before it creates the row, records the
// run's configuration with the options it runs on — holds across all of them.
// Their order is TestRunOnce_PhasesRunInTheirSecurityOrder's to pin.
func censusFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "runAdmission" {
				return "RunOnce"
			}
		}
	}
	return fn.Name.Name
}
