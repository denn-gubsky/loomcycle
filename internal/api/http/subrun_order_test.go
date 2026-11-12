package http

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// prepareSubRunStepOrder is the order prepareSubRunValues' steps run in.
// Several positions are security rules, not taste (see the comment above
// prepareSubRunValues); the pairs below name them so a failure says which
// rule moved.
var prepareSubRunStepOrder = []string{
	"subrun.strip", "subrun.resolve", "subrun.budget", "subrun.admit",
	"subrun.overrides", "subrun.policy", "subrun.model", "subrun.identity",
	"subrun.rows", "subrun.run_ctx", "subrun.prompt", "subrun.hosts",
	"subrun.persist", "subrun.child_ctx", "subrun.options",
}

// WHY: the step order of prepareSubRunValues is what a spawned child inherits
// from its parent and what it may not, and nothing else holds it in place. A
// future edit that resolves the child before the operator plane is stripped,
// writes its run row before its ACL admission, or builds its dispatcher before
// its host list is narrowed compiles and passes every other test.
func TestPrepareSubRun_StepsRunInTheirSecurityOrder(t *testing.T) {
	h := newReviewHarness(t)
	var mu sync.Mutex
	var got []string
	h.srv.runPhaseHook = func(phase string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(phase, "subrun.") {
			got = append(got, phase)
		}
	}
	parent := tools.WithRunID(context.Background(), "r_parent")
	prep, err := h.srv.prepareSubRunValues(parent, "writer", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	mu.Lock()
	defer mu.Unlock()

	for _, rule := range []struct{ before, after, why string }{
		{"subrun.strip", "subrun.resolve", "nothing runs with the authority of the operator plane that started the tree"},
		{"subrun.budget", "subrun.rows", "a tree over its hard budget writes nothing for the child"},
		{"subrun.admit", "subrun.rows", "a no-trace or override refusal writes nothing for the child"},
		{"subrun.policy", "subrun.rows", "a spawn the caller's ACL refuses writes nothing for the child"},
		{"subrun.overrides", "subrun.policy", "the child's policy is decided from the definition the overrides settled"},
		{"subrun.overrides", "subrun.model", "a model the child inherits is the one it runs on"},
		{"subrun.policy", "subrun.identity", "the run record keeps the host list the child inherited"},
		{"subrun.run_ctx", "subrun.child_ctx", "the spawner's stamps are cleared before the child's ctx is built on it"},
		{"subrun.prompt", "subrun.hosts", "the ACL narrows the tool set before the host narrowing and the dispatcher"},
		{"subrun.hosts", "subrun.child_ctx", "the child's dispatcher holds the host-narrowed tools"},
		{"subrun.prompt", "subrun.child_ctx", "the child's policy is decided again with its ACL-narrowed tools"},
	} {
		b, a := slices.Index(got, rule.before), slices.Index(got, rule.after)
		if b < 0 || a < 0 || b > a {
			t.Errorf("%s must run before %s (%s); order was %s", rule.before, rule.after, rule.why, strings.Join(got, " > "))
		}
	}
	if !slices.Equal(got, prepareSubRunStepOrder) {
		t.Errorf("prepareSubRunValues step order\n got: %s\nwant: %s", strings.Join(got, " > "), strings.Join(prepareSubRunStepOrder, " > "))
	}
}
