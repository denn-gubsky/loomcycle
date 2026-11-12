package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// kb/ask, kb/search and kb/walker: answering one question from the tenant
// knowledge base. kb/ask is a code agent that routes; the other two are the
// agents it routes to.

// TestKnowledgeBaseBundle_AnsweringAgentsHoldOnlyTheirTools pins the grants. kb/ask
// reads no document itself, so it holds the classifier and the spawn and nothing
// else; the two answering agents read the tenant knowledge base and hold no tool
// that writes anywhere else. (A tenant Document grant can still write documents
// until the read-only tool mode exists; their prompts forbid it.)
func TestKnowledgeBaseBundle_AnsweringAgentsHoldOnlyTheirTools(t *testing.T) {
	cfg := knowledgeBaseConfig(t)
	sorted := func(in []string) string {
		got := append([]string(nil), in...)
		sort.Strings(got)
		return strings.Join(got, ",")
	}

	ask, ok := cfg.Agents["kb/ask"]
	if !ok {
		t.Fatalf("kb/ask not registered (agents: %v)", agentNames(cfg))
	}
	if ask.Provider != "code-js" || ask.Model != "" || ask.Tier != "" {
		t.Errorf("kb/ask provider=%q model=%q tier=%q, want a code-js agent resolving no model", ask.Provider, ask.Model, ask.Tier)
	}
	if got := sorted(ask.Tools); got != "Agent,Decision" {
		t.Errorf("kb/ask tools = %v, want exactly [Decision Agent]", ask.Tools)
	}
	if len(ask.MemoryScopes) > 0 || len(ask.SqlScopes) > 0 {
		t.Errorf("kb/ask declares scopes (memory=%v sql=%v) — it reads no document itself", ask.MemoryScopes, ask.SqlScopes)
	}
	if _, err := codejs.Validate(ask.Code); err != nil {
		t.Fatalf("the shipped kb/ask body does not parse: %v", err)
	}

	writers := []string{"Write", "Edit", "NotebookEdit", "Bash", "Bashbox", "Memory", "Channel", "AgentDef", "SkillDef",
		"File", "Git", "HTTP", "WebFetch", "WebSearch", "Agent", "Decision"}
	for name, want := range map[string]string{"kb/search": "Document", "kb/walker": "Document,Path"} {
		a, ok := cfg.Agents[name]
		if !ok {
			t.Fatalf("%s not registered (agents: %v)", name, agentNames(cfg))
		}
		if got := sorted(a.Tools); got != want {
			t.Errorf("%s tools = %v, want exactly [%s]", name, a.Tools, want)
		}
		for _, w := range writers {
			if hasToolPreset(a.Tools, w) {
				t.Errorf("%s grants %q — it only reads the knowledge base", name, w)
			}
		}
		if strings.Join(a.MemoryScopes, ",") != "tenant" || strings.Join(a.SqlScopes, ",") != "tenant" {
			t.Errorf("%s scopes memory=%v sql=%v, want tenant for both (the tenant knowledge base)", name, a.MemoryScopes, a.SqlScopes)
		}
		if !containsString(a.Skills, "-*") || !a.DisableContext {
			t.Errorf("%s must deny skills and Context, or the runtime auto-adds those tools", name)
		}
		if a.Provider != "" || a.Model != "" || a.Tier != "middle" {
			t.Errorf("%s provider=%q model=%q tier=%q, want the operator's middle tier and no pin", name, a.Provider, a.Model, a.Tier)
		}
		if _, ok := cfg.Tiers[a.Tier]; !ok {
			t.Errorf("the base preset should supply the %q tier %s declares", a.Tier, name)
		}
	}
	if !strings.Contains(cfg.Agents["kb/search"].SystemPrompt, "COVERAGE: partial") {
		t.Error("kb/search must end its answer with the COVERAGE line kb/ask reads")
	}

	// Model-visible text must not cite design documents.
	for _, name := range []string{"kb/ask", "kb/search", "kb/walker"} {
		a := cfg.Agents[name]
		for where, text := range map[string]string{"system_prompt": a.SystemPrompt, "code": a.Code} {
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "RFC") || strings.Contains(line, "§") {
					t.Errorf("%s %s cites a design document (%q)", name, where, strings.TrimSpace(line))
				}
			}
		}
	}
}

// askFake stands in for the Decision tool and the two answering agents.
type askFake struct {
	mu       sync.Mutex
	score    float64
	decErr   string // a Decision failure text; "" answers with score
	answers  map[string]string
	spawns   []string // "name\nprompt" per spawn, in order
	decCalls int
}

type askFakeDecision struct{ f *askFake }

func (d *askFakeDecision) Name() string                 { return "Decision" }
func (d *askFakeDecision) Description() string          { return "decision (test double)" }
func (d *askFakeDecision) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (d *askFakeDecision) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	d.f.mu.Lock()
	d.f.decCalls++
	d.f.mu.Unlock()
	var in struct {
		State     map[string]any            `json:"state"`
		Questions map[string]map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.State["question"] == nil || in.Questions["overview"]["type"] != "noul" {
		return tools.Result{IsError: true, Text: "Decision: invalid_input: unexpected call " + string(raw)}, nil
	}
	if d.f.decErr != "" {
		return tools.Result{IsError: true, Text: d.f.decErr}, nil
	}
	return okResult(map[string]any{"model": "m", "answers": map[string]any{"overview": map[string]any{"type": "noul", "noul": d.f.score}}})
}

type askFakeAgent struct{ f *askFake }

func (a *askFakeAgent) Name() string                 { return "Agent" }
func (a *askFakeAgent) Description() string          { return "agent (test double)" }
func (a *askFakeAgent) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (a *askFakeAgent) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	name, _ := in["name"].(string)
	prompt, _ := in["prompt"].(string)
	if in["result"] != "object" {
		return tools.Result{IsError: true, Text: "kb/ask must ask for result object"}, nil
	}
	a.f.mu.Lock()
	a.f.spawns = append(a.f.spawns, name+"\n"+prompt)
	n := len(a.f.spawns)
	a.f.mu.Unlock()
	answer, ok := a.f.answers[name]
	if !ok {
		return tools.Result{IsError: true, Text: fmt.Sprintf("unexpected agent %q", name)}, nil
	}
	return okResult(map[string]any{"agent_id": "x", "run_id": fmt.Sprintf("r%d", n), "status": "completed", "final_text": answer})
}

func runAsk(t *testing.T, f *askFake, question string) string {
	t.Helper()
	agent := knowledgeBaseConfig(t).Agents["kb/ask"]
	set := []tools.Tool{&askFakeDecision{f: f}, &askFakeAgent{f: f}}
	ctx := tools.WithRunID(context.Background(), "run-ask")
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/ask",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: question}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	t.Logf("answer:\n%s", res.FinalText)
	return res.FinalText
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

const (
	askWalkerAnswer = "Five documents cover it: [A](loom:doc/0123456789abcdef0123456789abcdef?scope=tenant).\nMOVES: search, down\nMISSING: none"
	askHitDoc       = "loom:doc/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	askHitChunk     = "loom:chunk/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func askSearchAnswer(cov string) string {
	return "It is set in [Guide](" + askHitDoc + "?scope=tenant), see [step](" + askHitChunk + "?scope=tenant).\nCOVERAGE: " + cov
}

func TestKbAsk_OverviewScoreRoutesToWalker(t *testing.T) {
	f := &askFake{score: 0.62, answers: map[string]string{"kb/walker": askWalkerAnswer, "kb/search": askSearchAnswer("complete")}}
	out := runAsk(t, f, "What do we hold on speech and voice?")
	if len(f.spawns) != 1 || !strings.HasPrefix(f.spawns[0], "kb/walker\nWhat do we hold on speech and voice?") {
		t.Fatalf("spawns = %q, want one kb/walker run with the question", f.spawns)
	}
	if !strings.HasPrefix(out, askWalkerAnswer) {
		t.Errorf("the walker's answer must come back unchanged; got %q", out)
	}
	if got, want := lastLine(out), "ROUTE: walker | SCORE: 0.62 | RUNS: r1:completed"; got != want {
		t.Errorf("route line = %q, want %q", got, want)
	}
}

func TestKbAsk_LowScoreRoutesToSearch(t *testing.T) {
	f := &askFake{score: 0.07, answers: map[string]string{"kb/search": askSearchAnswer("complete")}}
	out := runAsk(t, f, "What is the default echo setting?")
	if len(f.spawns) != 1 || !strings.HasPrefix(f.spawns[0], "kb/search\n") {
		t.Fatalf("spawns = %q, want one kb/search run", f.spawns)
	}
	if got, want := lastLine(out), "ROUTE: search | SCORE: 0.07 | RUNS: r1:completed"; got != want {
		t.Errorf("route line = %q, want %q", got, want)
	}
}

// A partial answer to a clearly low-scored question is not walked: the walk is
// the expensive path, kept for questions the classifier did not rule out.
func TestKbAsk_PartialCoverageBelowLowBoundStaysWithSearch(t *testing.T) {
	f := &askFake{score: 0.05, answers: map[string]string{"kb/search": askSearchAnswer("partial"), "kb/walker": askWalkerAnswer}}
	out := runAsk(t, f, "Find the release guide.")
	if len(f.spawns) != 1 {
		t.Fatalf("spawns = %q, want kb/search only", f.spawns)
	}
	if !strings.HasPrefix(lastLine(out), "ROUTE: search | SCORE: 0.05 |") {
		t.Errorf("route line = %q", lastLine(out))
	}
}

// A slow or missing decision model must not make every question a walk.
func TestKbAsk_DecisionErrorRoutesToSearchAndReportsTheCode(t *testing.T) {
	for _, tc := range []struct{ text, code string }{
		{"Decision: timeout: the decision model did not answer in time", "timeout"},
		{"Decision: decision_not_configured: this deployment declares no decision models", "decision_not_configured"},
		{"Decision: call_failed: the call to the decision model failed", "call_failed"},
		{"Decision: model_not_found: gone", "model_not_found"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := &askFake{decErr: tc.text, answers: map[string]string{"kb/search": askSearchAnswer("complete"), "kb/walker": askWalkerAnswer}}
			out := runAsk(t, f, "What do we hold on speech and voice?")
			if f.decCalls != 1 || len(f.spawns) != 1 || !strings.HasPrefix(f.spawns[0], "kb/search\n") {
				t.Fatalf("decision calls %d, spawns %q, want one Decision call and one kb/search run", f.decCalls, f.spawns)
			}
			if got, want := lastLine(out), "ROUTE: search | SCORE: none ("+tc.code+") | RUNS: r1:completed"; got != want {
				t.Errorf("route line = %q, want %q", got, want)
			}
		})
	}
}

// With no score the question is not clearly low, so a partial flat answer still
// goes on to the walker.
func TestKbAsk_DecisionErrorWithPartialCoverageFallsBackToWalker(t *testing.T) {
	f := &askFake{decErr: "Decision: timeout: slow", answers: map[string]string{"kb/search": askSearchAnswer("partial"), "kb/walker": askWalkerAnswer}}
	out := runAsk(t, f, "What do we hold on speech and voice?")
	if len(f.spawns) != 2 || !strings.HasPrefix(f.spawns[1], "kb/walker\n") {
		t.Fatalf("spawns = %q, want kb/search then kb/walker", f.spawns)
	}
	if got, want := lastLine(out), "ROUTE: search -> walker (partial coverage) | SCORE: none (timeout) | RUNS: r1:completed, r2:completed"; got != want {
		t.Errorf("route line = %q, want %q", got, want)
	}
}

func TestKbAsk_PartialCoverageAtMiddleScoreFallsBackToWalkerWithHitIDs(t *testing.T) {
	f := &askFake{score: 0.2, answers: map[string]string{"kb/search": askSearchAnswer("partial"), "kb/walker": askWalkerAnswer}}
	q := "Which tools can a code agent call?"
	out := runAsk(t, f, q)
	if len(f.spawns) != 2 || !strings.HasPrefix(f.spawns[0], "kb/search\n") || !strings.HasPrefix(f.spawns[1], "kb/walker\n"+q) {
		t.Fatalf("spawns = %q, want kb/search then kb/walker with the question", f.spawns)
	}
	walk := f.spawns[1]
	for _, id := range []string{askHitDoc, askHitChunk} {
		if !strings.Contains(walk, id) {
			t.Errorf("the walker's prompt is missing the hit %s:\n%s", id, walk)
		}
	}
	if !strings.HasPrefix(out, askWalkerAnswer) {
		t.Errorf("the walker's answer must replace the partial one; got %q", out)
	}
	if got, want := lastLine(out), "ROUTE: search -> walker (partial coverage) | SCORE: 0.2 | RUNS: r1:completed, r2:completed"; got != want {
		t.Errorf("route line = %q, want %q", got, want)
	}
}
