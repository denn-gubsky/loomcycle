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

	// kb/classify: a one-word judgement with nothing to act with, its prompt the
	// owner's wording verbatim.
	cl, ok := cfg.Agents["kb/classify"]
	if !ok {
		t.Fatalf("kb/classify not registered (agents: %v)", agentNames(cfg))
	}
	if len(cl.Tools) != 0 || !containsString(cl.Skills, "-*") || !cl.DisableContext {
		t.Errorf("kb/classify must have no tools at all (tools=%v skills=%v disable_context=%v)", cl.Tools, cl.Skills, cl.DisableContext)
	}
	if len(cl.MemoryScopes) > 0 || len(cl.SqlScopes) > 0 {
		t.Errorf("kb/classify declares scopes (memory=%v sql=%v) — it reads only the question", cl.MemoryScopes, cl.SqlScopes)
	}
	if cl.Provider != "" || cl.Model != "" || cl.Tier != "low" {
		t.Errorf("kb/classify provider=%q model=%q tier=%q, want the operator's low tier and no pin", cl.Provider, cl.Model, cl.Tier)
	}
	if cl.MaxIterations != 1 || cl.MaxTokens != 1024 {
		t.Errorf("kb/classify max_iterations=%d max_tokens=%d, want 1 and 1024", cl.MaxIterations, cl.MaxTokens)
	}
	if cl.Sampling == nil || cl.Sampling.Temperature == nil || *cl.Sampling.Temperature != 0 {
		t.Errorf("kb/classify sampling = %+v, want temperature 0 set", cl.Sampling)
	}
	const classifyPrompt = "You classify one question that a user asks a knowledge base of documents. You do not answer it.\n\n" +
		"Is the question asking for an overview: a list of everything a knowledge base holds on a subject, spread over several documents?\n\n" +
		"If you are unsure, answer YES.\n\n" +
		"Reply with exactly one word: YES or NO.\n"
	if cl.SystemPrompt != classifyPrompt {
		t.Errorf("kb/classify prompt = %q, want %q", cl.SystemPrompt, classifyPrompt)
	}

	// Model-visible text must not cite design documents.
	for _, name := range []string{"kb/ask", "kb/classify", "kb/search", "kb/walker"} {
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

// askFake stands in for the Decision tool and the agents kb/ask spawns.
type askFake struct {
	mu       sync.Mutex
	score    float64
	decErr   string            // a Decision failure text; "" answers with score
	answers  map[string]string // final_text per agent name
	spawns   []string          // "name\nprompt" per spawn, in order
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

// spawned lists the agent names spawned, in order.
func (f *askFake) spawned() []string {
	var out []string
	for _, s := range f.spawns {
		out = append(out, strings.SplitN(s, "\n", 2)[0])
	}
	return out
}

func runAsk(t *testing.T, f *askFake, question string, metadata map[string]any) string {
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
		Metadata:   metadata,
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
	askQuestion     = "What do we hold on speech and voice?"
	askWalkerAnswer = "Five documents cover it: [A](loom:doc/0123456789abcdef0123456789abcdef?scope=tenant).\nMOVES: search, down\nMISSING: none"
	askHitDoc       = "loom:doc/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	askHitChunk     = "loom:chunk/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func askSearchAnswer(cov string) string {
	return "It is set in [Guide](" + askHitDoc + "?scope=tenant), see [step](" + askHitChunk + "?scope=tenant).\nCOVERAGE: " + cov
}

// askAnswers is every agent kb/ask may spawn, with kb/classify answering classify.
func askAnswers(classify, coverage string) map[string]string {
	return map[string]string{"kb/classify": classify, "kb/search": askSearchAnswer(coverage), "kb/walker": askWalkerAnswer}
}

func wantSpawns(t *testing.T, f *askFake, want ...string) {
	t.Helper()
	if got := strings.Join(f.spawned(), ","); got != strings.Join(want, ",") {
		t.Fatalf("spawned %q, want %q", f.spawned(), want)
	}
}

func wantRouteLine(t *testing.T, out, want string) {
	t.Helper()
	if got := lastLine(out); got != want {
		t.Errorf("route line = %q, want %q", got, want)
	}
}

func TestKbAsk_HighDecisionScoreRoutesToWalkerWithoutAskingTheAgent(t *testing.T) {
	f := &askFake{score: 0.62, answers: askAnswers("NO", "complete")}
	out := runAsk(t, f, askQuestion, nil)
	wantSpawns(t, f, "kb/walker")
	if !strings.HasPrefix(f.spawns[0], "kb/walker\n"+askQuestion) {
		t.Errorf("the walker got %q, want the question", f.spawns[0])
	}
	if !strings.HasPrefix(out, askWalkerAnswer) {
		t.Errorf("the walker's answer must come back unchanged; got %q", out)
	}
	wantRouteLine(t, out, "ROUTE: walker | SCORE: 0.62 | CLASSIFIER: decision | RUNS: r1:completed")
}

func TestKbAsk_LowDecisionScoreRoutesToSearchWithoutAskingTheAgent(t *testing.T) {
	f := &askFake{score: 0.07, answers: askAnswers("YES", "complete")}
	out := runAsk(t, f, "What is the default echo setting?", nil)
	wantSpawns(t, f, "kb/search")
	wantRouteLine(t, out, "ROUTE: search | SCORE: 0.07 | CLASSIFIER: decision | RUNS: r1:completed")
}

// A partial answer to a question the decision model was sure is not an overview
// is not walked.
func TestKbAsk_PartialCoverageAtOrBelowLowScoreStaysWithSearch(t *testing.T) {
	f := &askFake{score: 0.1, answers: askAnswers("YES", "partial")}
	out := runAsk(t, f, "Find the release guide.", nil)
	wantSpawns(t, f, "kb/search")
	wantRouteLine(t, out, "ROUTE: search | SCORE: 0.1 | CLASSIFIER: decision | RUNS: r1:completed")
}

func TestKbAsk_MiddleBandScoreAsksTheClassifyAgent(t *testing.T) {
	for _, tc := range []struct {
		reply, route string
		spawns       []string
	}{
		{"YES", "walker", []string{"kb/classify", "kb/walker"}},
		{"no.", "search", []string{"kb/classify", "kb/search"}},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			f := &askFake{score: 0.3, answers: askAnswers(tc.reply, "complete")}
			out := runAsk(t, f, askQuestion, nil)
			wantSpawns(t, f, tc.spawns...)
			if f.spawns[0] != "kb/classify\n"+askQuestion {
				t.Errorf("kb/classify got %q, want the question alone", f.spawns[0])
			}
			wantRouteLine(t, out, "ROUTE: "+tc.route+" | SCORE: 0.3 | CLASSIFIER: agent | RUNS: r1:completed, r2:completed")
		})
	}
}

// A slow or missing decision model hands the question to kb/classify rather
// than making it a walk or a flat search by default.
func TestKbAsk_DecisionErrorAsksTheClassifyAgentAndReportsTheCode(t *testing.T) {
	for _, tc := range []struct{ text, code string }{
		{"Decision: timeout: the decision model did not answer in time", "timeout"},
		{"Decision: decision_not_configured: this deployment declares no decision models", "decision_not_configured"},
		{"Decision: call_failed: the call to the decision model failed", "call_failed"},
		{"Decision: model_not_found: gone", "model_not_found"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := &askFake{decErr: tc.text, answers: askAnswers("NO", "complete")}
			out := runAsk(t, f, askQuestion, nil)
			if f.decCalls != 1 {
				t.Errorf("decision calls = %d, want 1", f.decCalls)
			}
			wantSpawns(t, f, "kb/classify", "kb/search")
			wantRouteLine(t, out, "ROUTE: search | SCORE: none ("+tc.code+") | CLASSIFIER: agent | RUNS: r1:completed, r2:completed")
		})
	}
}

// A wrong walk costs less than a lost overview: an answer with no YES or NO in
// it counts as an overview.
func TestKbAsk_UnreadableClassifyAnswerRoutesToWalker(t *testing.T) {
	f := &askFake{score: 0.3, answers: askAnswers("It depends on the corpus.", "complete")}
	out := runAsk(t, f, askQuestion, nil)
	wantSpawns(t, f, "kb/classify", "kb/walker")
	wantRouteLine(t, out, "ROUTE: walker | SCORE: 0.3 | CLASSIFIER: agent-unreadable | RUNS: r1:completed, r2:completed")
}

// A classify run that never started is unreadable too.
func TestKbAsk_FailedClassifySpawnRoutesToWalker(t *testing.T) {
	f := &askFake{score: 0.3, answers: map[string]string{"kb/search": askSearchAnswer("complete"), "kb/walker": askWalkerAnswer}}
	out := runAsk(t, f, askQuestion, nil)
	wantSpawns(t, f, "kb/classify", "kb/walker")
	wantRouteLine(t, out, "ROUTE: walker | SCORE: 0.3 | CLASSIFIER: agent-unreadable | RUNS: -:not_started, r2:completed")
}

// The measurement's switch: run metadata classifier=agent asks kb/classify only.
func TestKbAsk_AgentClassifierMetadataSkipsTheDecisionCall(t *testing.T) {
	f := &askFake{score: 0.9, answers: askAnswers("NO", "complete")}
	out := runAsk(t, f, askQuestion, map[string]any{"classifier": "agent"})
	if f.decCalls != 0 {
		t.Errorf("decision calls = %d, want 0 with classifier=agent", f.decCalls)
	}
	wantSpawns(t, f, "kb/classify", "kb/search")
	wantRouteLine(t, out, "ROUTE: search | SCORE: none | CLASSIFIER: agent | RUNS: r1:completed, r2:completed")
}

// An agent NO is not a sure NO: a partial flat answer still goes to the walker,
// with the ids the search found.
func TestKbAsk_PartialCoverageAfterAgentNoFallsBackToWalkerWithHitIDs(t *testing.T) {
	f := &askFake{score: 0.2, answers: askAnswers("NO", "partial")}
	out := runAsk(t, f, askQuestion, nil)
	wantSpawns(t, f, "kb/classify", "kb/search", "kb/walker")
	walk := f.spawns[2]
	if !strings.HasPrefix(walk, "kb/walker\n"+askQuestion) {
		t.Errorf("the walker's prompt must start with the question:\n%s", walk)
	}
	for _, id := range []string{askHitDoc, askHitChunk} {
		if !strings.Contains(walk, id) {
			t.Errorf("the walker's prompt is missing the hit %s:\n%s", id, walk)
		}
	}
	if !strings.HasPrefix(out, askWalkerAnswer) {
		t.Errorf("the walker's answer must replace the partial one; got %q", out)
	}
	wantRouteLine(t, out, "ROUTE: search -> walker (partial coverage) | SCORE: 0.2 | CLASSIFIER: agent | RUNS: r1:completed, r2:completed, r3:completed")
}
