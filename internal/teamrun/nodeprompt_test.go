package teamrun

import (
	"context"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// capturingSpawn records the full Prompt each agent was handed, so these tests
// assert what actually reached the agent rather than only what came back.
func capturingSpawn(mu *sync.Mutex, got map[string]Prompt) SpawnFunc {
	return func(_ context.Context, agent string, p Prompt, _ string) (string, error) {
		mu.Lock()
		got[agent] = p
		mu.Unlock()
		return agent + ":ok", nil
	}
}

// The point of per-node prompts: ONE AgentDef, two states, different roles, no
// clone. This is the test that would fail if a node's SystemPrompt were dropped
// on the way to the spawn — which is exactly what the old bare-string SpawnFunc
// made unavoidable.
func TestRunHandler_OneAgentTwoStatesGetTheirOwnSystemPrompts(t *testing.T) {
	var mu sync.Mutex
	got := map[string]Prompt{}
	r := NewAgentRunner(func(_ context.Context, agent string, p Prompt, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		got[p.System] = p // key by role: same agent, two roles
		return "ok", nil
	})

	sec := teamgraph.State{ID: "sec", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerAgent, Agent: "reviewer",
		SystemPrompt: "You are a security reviewer.",
	}}
	style := teamgraph.State{ID: "style", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerAgent, Agent: "reviewer",
		SystemPrompt: "You are a style reviewer.",
	}}

	if _, err := r.RunHandler(context.Background(), sec, &Task{Input: "the diff"}); err != nil {
		t.Fatalf("sec: %v", err)
	}
	if _, err := r.RunHandler(context.Background(), style, &Task{Input: "the diff"}); err != nil {
		t.Fatalf("style: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("one AgentDef ran two roles but %d distinct system prompts reached it: %v", len(got), got)
	}
	for role, p := range got {
		if p.Input != "the diff" {
			t.Errorf("role %q got input %q, want the threaded input", role, p.Input)
		}
	}
}

func TestNodePrompt_InputTemplateReplacesTheThreadedInput(t *testing.T) {
	h := teamgraph.Handler{Kind: teamgraph.HandlerAgent, InputTemplate: "Summarise the release notes."}
	if got := mustPrompt(t, h, "previous state output"); got.Input != "Summarise the release notes." {
		t.Errorf("input = %q, want the template", got.Input)
	}
}

func TestNodePrompt_EmptyTemplateFallsBackToTheThreadedInput(t *testing.T) {
	h := teamgraph.Handler{Kind: teamgraph.HandlerAgent}
	if got := mustPrompt(t, h, "previous state output"); got.Input != "previous state output" {
		t.Errorf("input = %q, want the threaded input", got.Input)
	}
}

// A fan-out is one node with one role, so every member is handed the same
// prompt. States whose members need different roles are separate `agent` states
// — the shape per-node prompts exist to make cheap.
func TestRunHandler_ParallelMembersShareTheNodesPrompt(t *testing.T) {
	var mu sync.Mutex
	got := map[string]Prompt{}
	r := NewAgentRunner(func(_ context.Context, agent string, p Prompt, _ string) (string, error) {
		mu.Lock()
		got[agent] = p
		mu.Unlock()
		if agent == "judge" {
			return "signal: success", nil
		}
		return agent + ":ok", nil
	})

	st := teamgraph.State{ID: "fan", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerParallel, Agents: []string{"a", "b"},
		Consolidator:  "judge",
		SystemPrompt:  "Review only what you are qualified to review.",
		InputTemplate: "Look at PR 42.",
	}}
	if _, err := r.RunHandler(context.Background(), st, &Task{Input: "threaded"}); err != nil {
		t.Fatalf("RunHandler: %v", err)
	}

	for _, name := range []string{"a", "b"} {
		if got[name].System != st.Handler.SystemPrompt {
			t.Errorf("%s system = %q, want the node's", name, got[name].System)
		}
		if got[name].Input != "Look at PR 42." {
			t.Errorf("%s input = %q, want the node's template", name, got[name].Input)
		}
	}
}

// A consolidator that FOLLOWS a handler is a different agent doing a different
// job, so the node's system prompt (which describes the state's own agent) must
// not leak onto it. It receives the results envelope and nothing else.
func TestRunHandler_ConsolidatorDoesNotInheritTheNodesSystemPrompt(t *testing.T) {
	var mu sync.Mutex
	got := map[string]Prompt{}
	r := NewAgentRunner(func(_ context.Context, agent string, p Prompt, _ string) (string, error) {
		mu.Lock()
		got[agent] = p
		mu.Unlock()
		if agent == "judge" {
			return "signal: success", nil
		}
		return "work product", nil
	})

	st := teamgraph.State{ID: "review", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerAgent, Agent: "reviewer", Consolidator: "judge",
		SystemPrompt: "You are a security reviewer.",
	}}
	if _, err := r.RunHandler(context.Background(), st, &Task{Input: "the diff"}); err != nil {
		t.Fatalf("RunHandler: %v", err)
	}

	if got["reviewer"].System != st.Handler.SystemPrompt {
		t.Errorf("the state's own agent lost its role: %q", got["reviewer"].System)
	}
	if got["judge"].System != "" {
		t.Errorf("consolidator inherited the reviewer's role %q — it is a different agent doing a different job", got["judge"].System)
	}
}

// A STANDALONE consolidator state IS the consolidator, so the node's prompt does
// apply to it. The mirror of the test above, and the reason the two cases are
// wired differently.
func TestRunHandler_StandaloneConsolidatorStateUsesItsOwnSystemPrompt(t *testing.T) {
	var mu sync.Mutex
	got := map[string]Prompt{}
	r := NewAgentRunner(capturingSpawn(&mu, got))

	st := teamgraph.State{ID: "judge", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerConsolidator, Agent: "judge",
		SystemPrompt: "Weigh the reviews and decide.",
	}}
	if _, err := r.RunHandler(context.Background(), st, &Task{Input: "the reviews"}); err != nil {
		t.Fatalf("RunHandler: %v", err)
	}
	if got["judge"].System != st.Handler.SystemPrompt {
		t.Errorf("standalone consolidator system = %q, want the node's", got["judge"].System)
	}
}

// mustPrompt composes a node's prompt with an empty environment and fails on any
// refused variable — these cases carry none, so a refusal would be a surprise
// worth surfacing rather than silently dropping.
func mustPrompt(t *testing.T, h teamgraph.Handler, threaded string) Prompt {
	t.Helper()
	p, refused := nodePrompt(h, threaded, Env{})
	if len(refused) != 0 {
		t.Fatalf("unexpected refused variables: %v", refused)
	}
	return p
}
