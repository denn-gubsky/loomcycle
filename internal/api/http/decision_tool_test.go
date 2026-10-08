package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// decisionEndpoint is a /v1/systemone double: it records the model of each call
// and answers every question as a noul, reporting 1116 input and 4 output
// tokens.
type decisionEndpoint struct {
	mu     sync.Mutex
	models []string
	url    string
}

func newDecisionEndpoint(t *testing.T) *decisionEndpoint {
	t.Helper()
	e := &decisionEndpoint{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.models = append(e.models, body.Model)
		e.mu.Unlock()
		answers := map[string]json.RawMessage{}
		for q := range body.Questions {
			answers[q] = json.RawMessage(`{"type":"noul","noul":0.938}`)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": body.Model, "answers": answers,
			"usage": map[string]int{"input_tokens": 1116, "output_tokens": 4},
		})
	}))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

func (e *decisionEndpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.models)
}

func (e *decisionEndpoint) asked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.models...)
}

// toolListProvider replays scripted turns and records the tool names each call
// was offered.
type toolListProvider struct {
	scriptedProvider
	mu      sync.Mutex
	offered [][]string
}

func (p *toolListProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	p.mu.Lock()
	p.offered = append(p.offered, names)
	p.mu.Unlock()
	return p.scriptedProvider.Call(ctx, req)
}

func (p *toolListProvider) firstOffer() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.offered) == 0 {
		return nil
	}
	return p.offered[0]
}

type decisionRunEnv struct {
	t        *testing.T
	srv      *Server
	ts       *httptest.Server
	store    store.Store
	prov     *toolListProvider
	endpoint *decisionEndpoint
}

const decisionCall = `{"state":{"ticket":"charged twice"},"questions":{"urgent":{"type":"noul","instructions":"Does it need a reply within the hour?"}}}`

// newDecisionRunEnv builds a server whose agents are `agents`, with the
// Decision tool registered over a keyed provider when configured is true and
// absent otherwise (as main registers it only for a declared decision: block).
func newDecisionRunEnv(t *testing.T, configured bool, agents map[string]config.AgentDef, scripts [][]providers.Event) *decisionRunEnv {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      agents,
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 2000},
	}
	cfg.Env.AuthToken = ""
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "decision.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := newDecisionEndpoint(t)
	var svc *decision.Service
	var all []tools.Tool
	if configured {
		drv, err := decision.New("ollama", decision.Options{
			ProviderID: "ollama", BaseURL: e.url, APIKey: "test-operator-key", KeyEnvName: "OLLAMA_API_KEY",
		})
		if err != nil {
			t.Fatal(err)
		}
		svc, err = decision.NewService("decide", []decision.ModelSpec{
			{Name: "decide", Provider: "ollama", Model: "nimble", Driver: drv},
			{Name: "deep", Provider: "ollama", Model: "clef", Driver: drv},
		})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, &builtin.Decision{Service: svc})
	}
	prov := &toolListProvider{scriptedProvider: scriptedProvider{
		scripts:  scripts,
		defaultS: []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn"}},
	}}
	srv := New(cfg, &stubResolver{p: prov}, all, concurrency.New(4, 4, 2*time.Second), st)
	if svc != nil {
		svc.SetOnUsage(srv.RecordRunSideCallUsage) // as main wires it
	}
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &decisionRunEnv{t: t, srv: srv, ts: ts, store: st, prov: prov, endpoint: e}
}

// run starts a run of agent as user u1 and returns its id and each tool
// result's text, in order.
func (e *decisionRunEnv) run(agent string) (runID string, results []string) {
	e.t.Helper()
	return e.runAs(nil, agent)
}

// runAs is run with the request made by principal p (nil = open mode), through
// the same handler POST /v1/runs reaches, so the run's context is stamped as a
// real run's is.
func (e *decisionRunEnv) runAs(p *auth.Principal, agent string) (runID string, results []string) {
	e.t.Helper()
	body := fmt.Sprintf(`{"agent":%q,"user_id":"u1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`, agent)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), *p))
	}
	rr := httptest.NewRecorder()
	e.srv.handleRuns(rr, req)
	if rr.Code != 200 {
		e.t.Fatalf("run status = %d: %s", rr.Code, rr.Body.String())
	}
	sc := bufio.NewScanner(rr.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			RunID string `json:"run_id"`
			Text  string `json:"text"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.RunID != "" && runID == "" {
			runID = ev.RunID
		}
		if ev.Type == "tool_result" {
			results = append(results, ev.Text)
		}
	}
	if runID == "" {
		e.t.Fatal("the run's stream carried no run_id")
	}
	return runID, results
}

// TestDecisionTool_ACallInsideARunIsChargedToTheRun — a granted agent's call
// answers, and its tokens land on the calling run: one token_usage row carrying
// the decision model's provider and model with BOTH input and output tokens,
// and the same tokens on the user's month-to-date budget counter.
func TestDecisionTool_ACallInsideARunIsChargedToTheRun(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{
		"router": {Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "route"},
	}, [][]providers.Event{toolCall("tu_1", "Decision", decisionCall)})

	runID, results := e.run("router")
	if len(results) != 1 || !strings.Contains(results[0], `"urgent":{"type":"noul","noul":0.938}`) {
		t.Fatalf("tool results = %q, want the model's answer", results)
	}
	if !contains(e.prov.firstOffer(), "Decision") {
		t.Errorf("the model was offered %v, want Decision among them", e.prov.firstOffer())
	}
	rows, err := e.store.TokenUsageForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var side []store.TokenUsageRow
	for _, r := range rows {
		if r.Provider == "ollama" {
			side = append(side, r)
		}
	}
	if len(side) != 1 {
		t.Fatalf("the run has %d usage rows for the decision provider, want 1 (all rows: %+v)", len(side), rows)
	}
	if r := side[0]; r.Model != "nimble" || r.InputTokens != 1116 || r.OutputTokens != 4 || r.UserID != "u1" || r.CredentialSource != "operator" {
		t.Errorf("row = %+v, want nimble with 1116 input and 4 output tokens for u1 on the operator's key", r)
	}
	// The loop's own two calls are 7 tokens each turn at most; the decision's
	// 1120 can only be on the counter if the side call was counted.
	if used := e.srv.limits.UsedFor("user", "", "u1"); used < 1120 {
		t.Errorf("the user's budget counter is %d, want the decision's 1120 tokens counted", used)
	}
}

// TestDecisionTool_AnAgentsNarrowingIsItsOwn — a run applies its agent's
// `decision` block: a call naming no model is answered by the agent's default.
// A sub-agent applies ITS definition's block, not its parent's: the child of a
// narrowed parent, declaring nothing, gets the operator's default.
func TestDecisionTool_AnAgentsNarrowingIsItsOwn(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Decision", "Agent"}, SystemPrompt: "route",
			Decision: &config.AgentDecision{Default: "deep", Models: []string{"deep"}}},
		"child": {Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "judge"},
	}, [][]providers.Event{
		toolCall("tu_p1", "Decision", decisionCall),                          // parent: narrowed
		toolCall("tu_p2", "Agent", `{"name":"child","prompt":"judge this"}`), // parent spawns the child
		toolCall("tu_c1", "Decision", decisionCall),                          // child: its own definition
	})
	_, results := e.run("parent")
	if len(results) != 2 || !strings.Contains(results[0], `"model":"deep"`) {
		t.Fatalf("the parent's results = %q, want its Decision answered by deep", results)
	}
	if got := e.endpoint.asked(); len(got) != 2 || got[0] != "clef" || got[1] != "nimble" {
		t.Errorf("the provider was asked for %v, want clef (the parent's default) then nimble (the operator's default, for the child)", got)
	}
}

// TestDecisionTool_OfferedOnlyWhenGrantedAndConfigured — an agent that does not
// list the tool is not offered it and cannot call it; an agent that lists it on
// a server with no decision models is not offered it either.
func TestDecisionTool_OfferedOnlyWhenGrantedAndConfigured(t *testing.T) {
	agents := map[string]config.AgentDef{
		"router":  {Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "route"},
		"ungated": {Model: "stub-model", Tools: []string{"Read"}, SystemPrompt: "read"},
	}
	t.Run("ungranted", func(t *testing.T) {
		e := newDecisionRunEnv(t, true, agents, [][]providers.Event{toolCall("tu_1", "Decision", decisionCall)})
		_, results := e.run("ungated")
		if contains(e.prov.firstOffer(), "Decision") {
			t.Errorf("an agent that does not list Decision was offered it: %v", e.prov.firstOffer())
		}
		if len(results) != 1 || strings.Contains(results[0], `"answers"`) || e.endpoint.count() != 0 {
			t.Errorf("an ungranted call: results %q after %d model calls, want a refusal and no call", results, e.endpoint.count())
		}
	})
	t.Run("no decision models", func(t *testing.T) {
		e := newDecisionRunEnv(t, false, agents, [][]providers.Event{toolCall("tu_1", "Decision", decisionCall)})
		_, results := e.run("router")
		if contains(e.prov.firstOffer(), "Decision") {
			t.Errorf("a server with no decision models offered Decision: %v", e.prov.firstOffer())
		}
		if len(results) != 1 || strings.Contains(results[0], `"answers"`) {
			t.Errorf("results = %q, want a refusal", results)
		}
	})
}

// keyRefused reports whether a tool result is the Decision tool's operator-key
// refusal: its own code, not a failed call.
func keyRefused(result string) bool {
	// A classified failure reaches the model as an envelope around the text.
	var env struct {
		IsError  bool   `json:"isError"`
		Error    string `json:"error"`
		Category string `json:"errorCategory"`
		Retry    bool   `json:"isRetryable"`
	}
	if json.Unmarshal([]byte(result), &env) != nil {
		return false
	}
	return env.IsError && env.Category == "permission" && !env.Retry &&
		strings.HasPrefix(env.Error, "Decision: operator_key_restricted: ")
}

// decisionProviderRows are a run's usage rows for the decision provider.
func (e *decisionRunEnv) decisionProviderRows(runID string) []store.TokenUsageRow {
	e.t.Helper()
	rows, err := e.store.TokenUsageForRun(context.Background(), runID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.TokenUsageRow
	for _, r := range rows {
		if r.Provider == "ollama" {
			out = append(out, r)
		}
	}
	return out
}

// TestDecisionTool_ARestrictedRunGetsNoCall — a run started by a principal that
// may not spend the operator's provider key is refused by the Decision tool
// before any call is made, with a code of its own, and is charged nothing. The
// restriction is the one a real run start stamps from the principal and the
// deployment's gate, not a context built by the test: the tool has no key rule
// of its own, so this is the crossing from run start to the decision driver.
// The same run by a principal holding the scope, and by the restricted one with
// the gate off, is answered.
func TestDecisionTool_ARestrictedRunGetsNoCall(t *testing.T) {
	agents := map[string]config.AgentDef{
		"router": {Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "route"},
	}
	script := [][]providers.Event{toolCall("tu_1", "Decision", decisionCall)}
	restricted := restrictedPrincipal()
	scoped := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeProvidersOperatorKey}}

	t.Run("restricted", func(t *testing.T) {
		e := newDecisionRunEnv(t, true, agents, script)
		e.srv.cfg().Env.OperatorKeyRestriction = true
		runID, results := e.runAs(&restricted, "router")
		if len(results) != 1 || !keyRefused(results[0]) {
			t.Errorf("tool results = %q, want the operator-key refusal", results)
		}
		if e.endpoint.count() != 0 {
			t.Errorf("a restricted run reached the decision model %d times", e.endpoint.count())
		}
		if rows := e.decisionProviderRows(runID); len(rows) != 0 {
			t.Errorf("a refused call was charged: %+v", rows)
		}
	})
	t.Run("holding the scope", func(t *testing.T) {
		e := newDecisionRunEnv(t, true, agents, script)
		e.srv.cfg().Env.OperatorKeyRestriction = true
		_, results := e.runAs(&scoped, "router")
		if len(results) != 1 || !strings.Contains(results[0], `"answers"`) || e.endpoint.count() != 1 {
			t.Errorf("tool results = %q after %d model calls, want an answer and one call", results, e.endpoint.count())
		}
	})
	t.Run("gate off", func(t *testing.T) {
		e := newDecisionRunEnv(t, true, agents, script)
		_, results := e.runAs(&restricted, "router")
		if len(results) != 1 || !strings.Contains(results[0], `"answers"`) || e.endpoint.count() != 1 {
			t.Errorf("tool results = %q after %d model calls, want an answer and one call", results, e.endpoint.count())
		}
	})
}

// TestDecisionTool_ASubAgentOfARestrictedRunGetsNoCall — the restriction
// follows the run tree: a child spawned by a restricted run is refused too.
func TestDecisionTool_ASubAgentOfARestrictedRunGetsNoCall(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "delegate"},
		"child":  {Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "judge"},
	}, [][]providers.Event{
		toolCall("tu_p1", "Agent", `{"name":"child","prompt":"judge this"}`),
		toolCall("tu_c1", "Decision", decisionCall),  // the child's call
		finalText("the decision tool said: refused"), // the child's answer
	})
	e.srv.cfg().Env.OperatorKeyRestriction = true
	restricted := restrictedPrincipal()
	runID, _ := e.runAs(&restricted, "parent")

	children, err := e.store.ListRunsByParentRunID(context.Background(), runID)
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %+v, %v; want the one sub-agent run", children, err)
	}
	if got := e.toolResults(children[0].ID); len(got) != 1 || !keyRefused(got[0]) {
		t.Errorf("the child's tool results = %q, want the operator-key refusal", got)
	}
	if e.endpoint.count() != 0 {
		t.Errorf("a restricted run's child reached the decision model %d times", e.endpoint.count())
	}
}

// TestDecisionTool_AResumedRestrictedRunGetsNoCall — a paused run resumed with
// no principal anywhere (the boot sweep) is held to the restriction recorded on
// its row. The gate is OFF, so the row is the only source: a resume that
// dropped it would answer.
func TestDecisionTool_AResumedRestrictedRunGetsNoCall(t *testing.T) {
	for _, c := range []struct {
		name       string
		restricted bool
	}{{"restricted row", true}, {"unrestricted row", false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newDecisionRunEnv(t, true, map[string]config.AgentDef{
				"router": {Provider: "scripted", Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "route"},
			}, [][]providers.Event{toolCall("tu_1", "Decision", decisionCall)})
			ctx := context.Background()
			sess, err := e.store.CreateSession(ctx, "", "router", "alice")
			if err != nil {
				t.Fatal(err)
			}
			run, err := e.store.CreateRun(ctx, sess.ID, store.RunIdentity{
				AgentID: "a_paused", UserID: "alice", Model: "stub-model", OperatorKeyRestricted: c.restricted})
			if err != nil {
				t.Fatal(err)
			}
			appendResumeEvent(t, e.srv, run.ID, "user_input", []loop.PromptSegment{
				{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "route it"}}},
			})
			if err := e.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
				t.Fatal(err)
			}
			if n, warnings := e.srv.ResumePausedRuns(ctx); n != 1 {
				t.Fatalf("re-dispatched %d, want 1 (warnings: %v)", n, warnings)
			}
			waitRunEndedWithin(t, e.store, run.ID, 5*time.Second)

			results := e.toolResults(run.ID)
			if len(results) != 1 {
				t.Fatalf("tool results = %q, want the Decision call's", results)
			}
			if c.restricted && (!keyRefused(results[0]) || e.endpoint.count() != 0) {
				t.Errorf("result %q after %d model calls, want the operator-key refusal and no call", results[0], e.endpoint.count())
			}
			if !c.restricted && (!strings.Contains(results[0], `"answers"`) || e.endpoint.count() != 1) {
				t.Errorf("result %q after %d model calls, want an answer and one call", results[0], e.endpoint.count())
			}
		})
	}
}

// toolResults reads a run's tool results from its stored transcript, in order.
func (e *decisionRunEnv) toolResults(runID string) []string {
	e.t.Helper()
	events, err := e.store.GetRunEventsSince(context.Background(), runID, 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		if ev.Type != "tool_result" {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		out = append(out, p.Text)
	}
	return out
}
