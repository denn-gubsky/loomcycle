package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GY P1 PR 9a: every agent holds Calc at run time without naming it, and
// does not with "-Calc" in its tools. Calc is added where a run's toolset is
// built (filterTools via config.EffectiveTools), never into a stored list, so
// each path that builds a toolset from a definition is checked here: a static
// agent, a runtime-created one, a registered one, a sub-agent, a pinned
// version, a resumed run and a team's own agent.

// calcToolLists are the tools lists every path is checked with, and whether a
// run of each holds Calc.
var calcToolLists = []struct {
	tools    []string
	wantCalc bool
}{
	{[]string{"Read"}, true},
	{[]string{"Read", "-Calc"}, false},
	{[]string{"*"}, true},
	{[]string{"*", "-Calc"}, false},
}

func toolsetOf(t *testing.T, h *reviewHarness, def config.AgentDef) []string {
	t.Helper()
	got, _ := h.srv.runToolset(context.Background(), context.Background(), "", def, nil, nil, nil, func(in []tools.Tool) []tools.Tool { return in })
	return toolNames(got)
}

func TestRunToolset_StaticAgentHoldsCalcUnlessRemoved(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Calc"})
	for _, tc := range calcToolLists {
		h.srv.cfg().Agents["worker"] = config.AgentDef{Model: "stub-model", SystemPrompt: "w", Tools: tc.tools}
		def, ok := lookup.Agent(context.Background(), h.st, h.srv.cfg(), "", "worker")
		if !ok || !def.Static {
			t.Fatalf("fixture: worker did not resolve as a static agent (ok=%v)", ok)
		}
		got := toolsetOf(t, h, def)
		if !slices.Contains(got, "Read") {
			t.Fatalf("tools %v: fixture: the run lacks Read: %v", tc.tools, got)
		}
		if slices.Contains(got, "Calc") != tc.wantCalc {
			t.Errorf("static agent tools %v: run holds %v; want Calc=%v", tc.tools, got, tc.wantCalc)
		}
	}
}

func TestRunToolset_RuntimeCreatedAgentHoldsCalcUnlessRemoved(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Calc"})
	for _, tc := range calcToolLists {
		body, _ := json.Marshal(map[string]any{"tools": tc.tools})
		def, ok := lookup.AgentFromDefRow(store.AgentDefRow{Name: "lean", DefID: "d1", Definition: body})
		if !ok {
			t.Fatalf("definition %s did not decode", body)
		}
		if slices.Contains(def.Tools, "Calc") {
			t.Fatalf("a resolved definition carries Calc in its tools %v; it must be added only to the toolset", def.Tools)
		}
		if got := toolsetOf(t, h, def); slices.Contains(got, "Calc") != tc.wantCalc {
			t.Errorf("stored tools %v: run holds %v; want Calc=%v", tc.tools, got, tc.wantCalc)
		}
	}
}

func TestRunToolset_RegisteredAgentHoldsCalcUnlessRemoved(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Calc"})
	ctx := context.Background()
	for i, tc := range calcToolLists[:2] { // "*" is stripped by register_agent's privileged-tool rule
		name := []string{"reg-plain", "reg-lean"}[i]
		desc, err := h.srv.RegisterAgent(ctx, connector.RegisterAgentRequest{Name: name, SystemPrompt: "x", Tools: tc.tools})
		if err != nil {
			t.Fatalf("register %v: %v", tc.tools, err)
		}
		if slices.Contains(desc.Tools, "Calc") {
			t.Errorf("register %v: the stored tools %v carry Calc", tc.tools, desc.Tools)
		}
		def, ok := lookup.Agent(ctx, h.st, h.srv.cfg(), "", name)
		if !ok {
			t.Fatalf("%s not found after RegisterAgent", name)
		}
		if got := toolsetOf(t, h, def); slices.Contains(got, "Calc") != tc.wantCalc {
			t.Errorf("registered tools %v: run holds %v; want Calc=%v", tc.tools, got, tc.wantCalc)
		}
	}
}

func TestPrepareSubRun_ChildHoldsCalcUnlessRemoved(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Calc"})
	for _, tc := range calcToolLists {
		h.srv.cfg().Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: tc.tools}
		prep, err := h.srv.prepareSubRunValues(tools.WithRunID(context.Background(), "r_parent"), "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		policy := tools.PolicyOf(prep.LoopCtx)
		prep.cleanup()
		if slices.Contains(policy.Tools, "Calc") != tc.wantCalc {
			t.Errorf("child tools %v: the sub-run holds %v; want Calc=%v", tc.tools, policy.Tools, tc.wantCalc)
		}
		// The Skill tool's subset check reads the patterns: they carry the
		// default filterTools added, so a skill needing Calc does not widen.
		if want := config.EffectiveTools(tc.tools); !slices.Equal(policy.ToolPatterns, want) {
			t.Errorf("child tools %v: tool patterns %v; want %v", tc.tools, policy.ToolPatterns, want)
		}
	}
}

func TestPinnedSubAgentDef_HoldsCalcUnlessRemoved(t *testing.T) {
	all := fakeToolsNamed("Read", "Calc", "Context")
	for _, tc := range []struct {
		base, stored []string
		wantCalc     bool
	}{
		{[]string{"Read"}, []string{"Read"}, true},
		{[]string{"Read", "-Calc"}, []string{"Read"}, false}, // the base's removal holds over the pin
		{[]string{"Read"}, []string{"Read", "-Calc"}, false},
		{[]string{"Read", "-Calc"}, []string{"*"}, false},
	} {
		stored, _ := json.Marshal(map[string]any{"tools": tc.stored})
		got := pinnedSubAgentDef(config.AgentDef{Tools: tc.base}, stored)
		if slices.Contains(got.Tools, "Calc") {
			t.Errorf("base %v, stored %v: the pinned definition's tools %v carry Calc", tc.base, tc.stored, got.Tools)
		}
		names := toolNames(filterTools(all, got.Tools, nil))
		if slices.Contains(names, "Calc") != tc.wantCalc {
			t.Errorf("base %v, stored %v: run tools %v; want Calc=%v", tc.base, tc.stored, names, tc.wantCalc)
		}
	}
}

// A caller's tools list narrows Calc away like any other tool.
func TestFilterTools_CallerListNarrowsCalcAway(t *testing.T) {
	all := fakeToolsNamed("Read", "Calc")
	if got := toolNames(filterTools(all, []string{"Read"}, []string{"Read"})); slices.Contains(got, "Calc") {
		t.Errorf("run tools %v hold Calc though the caller's list is [Read]", got)
	}
	if got := toolNames(filterTools(all, nil, nil)); !slices.Equal(got, []string{"Calc"}) {
		t.Errorf("an agent with no tools holds %v; want [Calc] (the default)", got)
	}
	if got := toolNames(filterTools(all, []string{"-Calc"}, nil)); len(got) != 0 {
		t.Errorf("an agent removing its only default holds %v; want nothing", got)
	}
}

func TestResumedRun_HoldsCalcUnlessRemoved(t *testing.T) {
	for _, tc := range calcToolLists[:2] {
		orig, resumed := startAndResumeAgentWithTools(t, tc.tools)
		if !slices.Contains(orig, "Read") {
			t.Fatalf("tools %v: fixture: the run was offered %v, without Read", tc.tools, orig)
		}
		if slices.Contains(orig, "Calc") != tc.wantCalc || slices.Contains(resumed, "Calc") != tc.wantCalc {
			t.Errorf("tools %v: offered %v then %v after the resume; want Calc=%v both times", tc.tools, orig, resumed, tc.wantCalc)
		}
	}
}

// startAndResumeAgentWithTools starts a run of an agent with the given tools,
// parks it, resumes it, and returns the tools offered on each call.
func startAndResumeAgentWithTools(t *testing.T, agentTools []string) (orig, resumed []string) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: agentTools, SystemPrompt: "you work"},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "calc_resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov},
		[]tools.Tool{namedStubTool{name: "Read"}, namedStubTool{name: "Calc"}},
		concurrency.New(4, 4, time.Second), st)
	stopDetachedOnCleanup(t, srv)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"worker","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))
	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	reqs := prov.waitForRequests(t, 2)
	return offeredToolNames(reqs[0]), offeredToolNames(reqs[1])
}

func TestTeamWalk_TeamsOwnAgentHoldsCalcUnlessRemoved(t *testing.T) {
	for _, tc := range calcToolLists[:2] {
		h := newLocalHarness(t)
		h.srv.tools = append(h.srv.tools, namedTool{"Read"}, namedTool{"Calc"})
		team := teamWith("./reviewer", map[string]string{"reviewer": "LOCAL reviewer"})
		team = strings.Replace(team, `"tools":["Agent"]`, `"tools":`+string(mustJSON(t, tc.tools)), 1)
		h.seedTeamVersion("tdf_sdlc_1", "sdlc", team, true)
		h.walk("sdlc")
		got := h.prov.offeredTo("LOCAL reviewer")
		if !slices.Contains(got, "Read") {
			t.Fatalf("tools %v: fixture: the team's agent was offered %v, without Read", tc.tools, got)
		}
		if slices.Contains(got, "Calc") != tc.wantCalc {
			t.Errorf("team-local tools %v: offered %v; want Calc=%v", tc.tools, got, tc.wantCalc)
		}
	}
}
