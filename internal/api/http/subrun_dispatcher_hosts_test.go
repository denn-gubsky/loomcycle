package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The child's dispatcher is what executes its tool calls, so it must hold the
// tools narrowed to the host list the child inherited, not the set before the
// narrowing: a child whose parent may reach only api.example.com is refused
// other.example.com even though the operator's list allows it.
func TestPrepareSubRun_ChildDispatcherRefusesAHostItsParentMayNotReach(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, &builtin.HTTP{HostAllowlist: []string{"api.example.com", "other.example.com"}})
	h.srv.cfg().Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"HTTP"}}
	parent := tools.UpdateRunPolicy(tools.WithRunID(context.Background(), "r_parent"), func(rp *tools.RunPolicy) {
		rp.Hosts = tools.HostPolicyValue{AllowedHosts: []string{"api.example.com"}, HasList: true}
	})
	prep, err := h.srv.prepareSubRunValues(parent, "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	if prep.Opts.Dispatcher == nil {
		t.Fatal("the child has no dispatcher")
	}
	input, _ := json.Marshal(map[string]string{"method": "GET", "url": "http://other.example.com/"})
	res := prep.Opts.Dispatcher.Execute(prep.LoopCtx, "HTTP", input)
	if !res.IsError || !strings.Contains(res.Text, "not in allowlist") {
		t.Errorf("the child's dispatcher let an HTTP call reach other.example.com, which its parent may not reach: %+v", res)
	}
}
