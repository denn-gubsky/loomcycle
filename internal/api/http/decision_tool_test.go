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
	body := fmt.Sprintf(`{"agent":%q,"user_id":"u1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`, agent)
	resp, err := http.Post(e.ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("post run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("run status = %d: %s", resp.StatusCode, raw)
	}
	sc := bufio.NewScanner(resp.Body)
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
		if ev.RunID != "" {
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

// TestDecisionTool_ARunBarredFromTheOperatorsKeyGetsNoCall — a run that may not
// spend the operator's provider key is refused before any call is made, with
// its own code, so the model can tell it from a call that failed.
func TestDecisionTool_ARunBarredFromTheOperatorsKeyGetsNoCall(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{
		"router": {Model: "stub-model", Tools: []string{"Decision"}, SystemPrompt: "route"},
	}, nil)
	tool := e.srv.tools[0]
	for _, tl := range e.srv.tools {
		if tl.Name() == "Decision" {
			tool = tl
		}
	}
	ctx := tools.WithRunID(context.Background(), "run-restricted")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1", UserID: "u1"})
	ctx = providers.WithOperatorKeyAllowed(ctx, false)
	res, err := e.srv.execBuiltin(ctx, tool, json.RawMessage(decisionCall))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.HasPrefix(res.Text, "Decision: operator_key_restricted: ") ||
		res.Error == nil || res.Error.Category != tools.CategoryPermission || res.Error.Retryable {
		t.Errorf("result = %q %+v, want a non-retryable permission refusal coded operator_key_restricted", res.Text, res.Error)
	}
	if e.endpoint.count() != 0 {
		t.Errorf("a restricted run reached the decision model %d times", e.endpoint.count())
	}
	rows, _ := e.store.TokenUsageForRun(context.Background(), "run-restricted")
	if len(rows) != 0 {
		t.Errorf("a refused call was charged: %+v", rows)
	}
	// The same call, allowed the key, answers: the refusal is the bit's doing.
	ok, _ := e.srv.execBuiltin(providers.WithOperatorKeyAllowed(ctx, true), tool, json.RawMessage(decisionCall))
	if ok.IsError || e.endpoint.count() != 1 {
		t.Errorf("an unrestricted call: %q after %d model calls, want an answer and one call", ok.Text, e.endpoint.count())
	}
}

// TestDecisionConnector_WithNoDecisionModelsSaysSo — the MCP `decision` tool
// reaches Server.Decision whatever is registered; on a server with no decision
// models the caller gets the tool's own classified refusal, not a transport
// error saying the tool does not exist.
func TestDecisionConnector_WithNoDecisionModelsSaysSo(t *testing.T) {
	e := newDecisionRunEnv(t, false, map[string]config.AgentDef{}, nil)
	res, err := e.srv.Decision(context.Background(), json.RawMessage(decisionCall))
	if err != nil {
		t.Fatalf("Decision: %v, want a tool result", err)
	}
	if !res.IsError || !strings.HasPrefix(res.Text, "Decision: decision_not_configured: ") ||
		res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryBusiness {
		t.Errorf("result = %q %+v, want a business refusal coded decision_not_configured", res.Text, res.ErrorInfo)
	}
}

// TestDecisionConnector_RunsUnderTheCallersPrincipal — a call with no run on its
// context (the MCP path) answers, and the key rule still applies to the
// principal the transport stamped: barred from the operator's key, it gets no
// call.
func TestDecisionConnector_RunsUnderTheCallersPrincipal(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{}, nil)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"substrate:tenant"}})
	res, err := e.srv.Decision(ctx, json.RawMessage(decisionCall))
	if err != nil || res.IsError || !strings.Contains(res.Text, `"model":"decide"`) || e.endpoint.count() != 1 {
		t.Fatalf("Decision = %q, %v after %d model calls; want an answer from decide", res.Text, err, e.endpoint.count())
	}
	res, err = e.srv.Decision(providers.WithOperatorKeyAllowed(ctx, false), json.RawMessage(decisionCall))
	if err != nil || !strings.HasPrefix(res.Text, "Decision: operator_key_restricted: ") || e.endpoint.count() != 1 {
		t.Errorf("restricted = %q, %v after %d model calls; want the key refusal and no second call", res.Text, err, e.endpoint.count())
	}
}
