package http

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GY: a "-Context" entry in an agent's tools holds across every path that
// assembles a run's tools — a sub-agent's spawn, and a run resumed on a stored
// version (resolved from the row, as resume resolves it).

func TestPrepareSubRun_ChildWithContextRemovedDoesNotHoldContext(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Context"})
	h.srv.cfg().Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"*", "-Context"}}
	prep, err := h.srv.prepareSubRunValues(tools.WithRunID(context.Background(), "r_parent"), "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	got := tools.PolicyOf(prep.LoopCtx).Tools
	if !slices.Contains(got, "Read") {
		t.Fatalf("fixture: the child's tools %v lack Read", got)
	}
	if slices.Contains(got, "Context") {
		t.Errorf("the child's tools %v hold Context despite -Context", got)
	}
}

// register_agent: a removal of a default tool registers and resolves without
// Context; one of any other tool is refused, and a removal alone is the empty
// list it already refuses.
func TestRegisterAgent_ToolRemovalIsValidatedAndHonoured(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	reg := func(name string, list ...string) error {
		_, err := h.srv.RegisterAgent(ctx, connector.RegisterAgentRequest{Name: name, SystemPrompt: "x", Tools: list})
		return err
	}
	if err := reg("lean", "Read", "-Context"); err != nil {
		t.Fatalf("register with -Context: %v", err)
	}
	def, ok := lookup.Agent(ctx, h.st, h.srv.cfg(), "", "lean")
	if !ok {
		t.Fatal("lean not found after RegisterAgent")
	}
	if grants, _ := config.SplitTools(def.Tools); slices.Contains(grants, "Context") {
		t.Errorf("the registered agent resolves with Context: %v", def.Tools)
	}
	for _, tc := range []struct {
		list []string
		want string
	}{
		{[]string{"Read", "-Read"}, "only a default tool"},
		{[]string{"Read", "-Skill"}, "skills: [-*]"},
		{[]string{"-Context"}, "tools required"},
	} {
		if err := reg("bad", tc.list...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("register %v: err = %v, want %q", tc.list, err, tc.want)
		}
	}
}

func TestRunToolset_StoredVersionWithContextRemovedDoesNotHoldContext(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Context"})
	toolset := func(definition string) []string {
		def, ok := lookup.AgentFromDefRow(store.AgentDefRow{Name: "lean", DefID: "d1", Definition: json.RawMessage(definition)})
		if !ok {
			t.Fatalf("definition %s did not decode", definition)
		}
		got, _ := h.srv.runToolset(context.Background(), context.Background(), "", def, nil, nil, nil, func(in []tools.Tool) []tools.Tool { return in })
		return toolNames(got)
	}
	if got := toolset(`{"tools":["Read"]}`); !slices.Contains(got, "Context") {
		t.Fatalf("fixture: a stored version without the removal resolves to %v, without the Context default", got)
	}
	for _, definition := range []string{`{"tools":["Read","-Context"]}`, `{"tools":["*","-Context"]}`} {
		if got := toolset(definition); slices.Contains(got, "Context") || !slices.Contains(got, "Read") {
			t.Errorf("stored %s: run tools %v, want Read and no Context", definition, got)
		}
	}
}
