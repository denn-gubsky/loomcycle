package teamruntest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The decision-state tests drive a team that asks a decision model through the
// real server: POST /v1/_teamdef, the TeamDef tool, the walk, the Decision
// tool and the decision driver. Only the two models are doubles: the chat
// provider the desks run on, and the decision model's endpoint.

const ticket = "My March invoice was charged twice"

// triageOverlay is the team: an input state, a decision that routes to one of
// three desks and binds three answers, and the desks.
const triageOverlay = `{"entry":"intake","states":[` +
	`{"state":"intake","handler":{"kind":"input"}},` +
	`{"state":"triage","handler":{"kind":"decision","model":"decide",` +
	`"about":{"ticket":"{{thread.output}}","tier":"${var.tier}"},` +
	`"questions":{` +
	`"route":{"type":"choice","instructions":"Which team should handle this ticket?","criteria":{"billing":"invoices, refunds","support":"bugs, outages","sales":"upgrades"}},` +
	`"urgent":{"type":"noul","instructions":"Does this ticket need a reply within the hour?"},` +
	`"detail":{"type":"score","instructions":"How complete is the report?","criteria":["no detail","some detail","everything needed"]}},` +
	`"route":"route",` +
	`"capture":{"team":"$.answers.route.choice","urgent":"$.answers.urgent.noul","detail":"$.answers.detail.score"}}},` +
	`{"state":"billing-desk","handler":{"kind":"agent","agent":"desk","input_template":"team=${var.team} urgent=${var.urgent} detail=${var.detail}: {{thread.output}}"}},` +
	`{"state":"support-desk","handler":{"kind":"agent","agent":"desk"}},` +
	`{"state":"sales-desk","handler":{"kind":"agent","agent":"desk"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[` +
	`{"from":"intake","to":"triage","on":"success"},` +
	`{"from":"triage","to":"billing-desk","on":"conditional:billing"},` +
	`{"from":"triage","to":"support-desk","on":"conditional:support"},` +
	`{"from":"triage","to":"sales-desk","on":"conditional:sales"},` +
	`{"from":"billing-desk","to":"done","on":"success"},` +
	`{"from":"support-desk","to":"done","on":"success"},` +
	`{"from":"sales-desk","to":"done","on":"success"}],` +
	`"vars":{"tier":"gold"}}`

const triageAnswers = `{"route":{"type":"choice","choice":"billing","probabilities":{"billing":0.91,"support":0.06,"sales":0.03},"confidence":0.85},` +
	`"urgent":{"type":"noul","noul":0.4},` +
	`"detail":{"type":"score","score":1.5}}`

// deskProvider is the chat model the desks run on. It records what each was
// asked.
type deskProvider struct {
	mu      sync.Mutex
	prompts []string
}

func (p *deskProvider) ID() string                  { return "stub" }
func (p *deskProvider) Probe(context.Context) error { return nil }
func (p *deskProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *deskProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *deskProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	var asked []string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			asked = append(asked, b.Text)
		}
	}
	p.mu.Lock()
	p.prompts = append(p.prompts, strings.Join(asked, "\n"))
	p.mu.Unlock()
	events := says("handled")
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *deskProvider) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

// decisionDouble stands in for the decision model's endpoint and records each
// request body.
type decisionDouble struct {
	mu     sync.Mutex
	bodies []string
	status int
	url    string
}

func newDecisionDouble(t *testing.T) *decisionDouble {
	t.Helper()
	d := &decisionDouble{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.bodies = append(d.bodies, string(raw))
		status := d.status
		d.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"overloaded"}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"nimble","answers":`+triageAnswers+`,"usage":{"input_tokens":1116,"output_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)
	d.url = srv.URL
	return d
}

func (d *decisionDouble) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.bodies...)
}

type decisionEnv struct {
	t     *testing.T
	st    store.Store
	ts    *httptest.Server
	desks *deskProvider
	model *decisionDouble
}

// newDecisionEnv builds an open server whose TeamDef tool is wired as main
// wires it. configured=false is a deployment with no decision models.
func newDecisionEnv(t *testing.T, configured bool) *decisionEnv {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"desk": {Model: "stub-model", SystemPrompt: "you staff a desk"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 2000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "decision-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	model := newDecisionDouble(t)
	desks := &deskProvider{}
	team := &builtin.TeamDef{Store: st}
	all := []tools.Tool{team}
	var svc *decision.Service
	if configured {
		drv, err := decision.New("ollama", decision.Options{ProviderID: "ollama", BaseURL: model.url})
		if err != nil {
			t.Fatal(err)
		}
		svc, err = decision.NewService("decide", []decision.ModelSpec{{Name: "decide", Provider: "ollama", Model: "nimble", Driver: drv}})
		if err != nil {
			t.Fatal(err)
		}
		team.Decision = &builtin.Decision{Service: svc}
		all = append(all, team.Decision)
	}
	srv := lchttp.New(cfg, oneProvider{desks}, all, concurrency.New(4, 4, 2*time.Second), st)
	srv.SetTeamDefTool(team)
	if svc != nil {
		svc.SetOnUsage(srv.RecordRunSideCallUsage) // as main wires it
	}
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &decisionEnv{t: t, st: st, ts: ts, desks: desks, model: model}
}

// teamdef posts one TeamDef call and returns the HTTP status and the decoded
// answer.
func (e *decisionEnv) teamdef(input string) (int, map[string]any, string) {
	e.t.Helper()
	resp, err := http.Post(e.ts.URL+"/v1/_teamdef", "application/json", strings.NewReader(input))
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func (e *decisionEnv) create(overlay string) (int, string) {
	status, _, raw := e.teamdef(`{"op":"create","name":"triage","promote":true,"overlay":` + overlay + `}`)
	return status, raw
}

// TestDecisionState_ATeamRoutesOnADecisionAndBindsItsAnswers is the request's
// acceptance walk: the ticket goes to the billing desk on the model's choice,
// no agent run is made for the decision, the three answers are variables the
// desk's prompt reads, and the desk is handed the ticket, not the answer.
func TestDecisionState_ATeamRoutesOnADecisionAndBindsItsAnswers(t *testing.T) {
	e := newDecisionEnv(t, true)
	if status, raw := e.create(triageOverlay); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, raw)
	}
	_, verify, raw := e.teamdef(`{"op":"verify","name":"triage"}`)
	if verify["runnable"] != true || verify["issues"] != nil {
		t.Fatalf("verify = %s, want runnable with no issues", raw)
	}

	status, out, raw := e.teamdef(`{"op":"run","name":"triage","input":` + jsonString(ticket) + `}`)
	if status != http.StatusOK || out["status"] != "completed" || out["final_state"] != "done" {
		t.Fatalf("run = %d: %s", status, raw)
	}
	steps, _ := out["steps"].([]any)
	if len(steps) != 3 {
		t.Fatalf("the walk took %d steps, want intake, triage, billing-desk: %s", len(steps), raw)
	}
	triage, _ := steps[1].(map[string]any)
	if triage["state"] != "triage" || triage["edge"] != "conditional:billing" || triage["next"] != "billing-desk" || triage["output"] != ticket {
		t.Errorf("the triage step = %v, want the billing edge and the ticket as its output", triage)
	}
	answer, _ := triage["answer"].(map[string]any)
	answers, _ := answer["answers"].(map[string]any)
	route, _ := answers["route"].(map[string]any)
	if answer["model"] != "decide" || route["choice"] != "billing" {
		t.Errorf("the triage step's answer = %v, want the model's answer as an object", triage["answer"])
	}
	if intake, _ := steps[0].(map[string]any); intake["answer"] != nil {
		t.Errorf("a step that is not a decision carries an answer: %v", intake)
	}

	// One call to the decision model, about the ticket and the variable.
	asked := e.model.asked()
	if len(asked) != 1 || !strings.Contains(asked[0], ticket) || !strings.Contains(asked[0], "gold") {
		t.Fatalf("the decision model was asked %d times: %v", len(asked), asked)
	}
	// One agent run in the whole walk, the desk's: its prompt has the bound
	// answers and the ticket, and nothing of the answer JSON.
	prompts := e.desks.asked()
	if len(prompts) != 1 {
		t.Fatalf("the chat model was called %d times, want once, for the desk: %v", len(prompts), prompts)
	}
	if want := "team=billing urgent=0.4 detail=1.5: " + ticket; !strings.Contains(prompts[0], want) || strings.Contains(prompts[0], "probabilities") {
		t.Errorf("the desk was asked %q, want it to contain %q and none of the answer", prompts[0], want)
	}

	// The call is charged to the walk's own run.
	runID, _ := out["run_id"].(string)
	if runID == "" {
		t.Fatalf("the walk reported no run: %s", raw)
	}
	rows, err := e.st.TokenUsageForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Provider != "ollama" || rows[0].InputTokens != 1116 || rows[0].OutputTokens != 4 {
		t.Errorf("the walk's run was charged %+v, want the one decision call", rows)
	}
}

// A routed answer with no edge is refused when the team is saved, naming the
// option; a question with one option is refused in the Decision tool's words.
func TestDecisionState_ATeamThatCannotRouteIsRefusedAtCreate(t *testing.T) {
	e := newDecisionEnv(t, true)
	noSales := strings.Replace(triageOverlay, `{"from":"triage","to":"sales-desk","on":"conditional:sales"},`, "", 1)
	noSales = strings.Replace(noSales, `{"state":"sales-desk","handler":{"kind":"agent","agent":"desk"}},`, "", 1)
	noSales = strings.Replace(noSales, `{"from":"sales-desk","to":"done","on":"success"}`, "", 1)
	noSales = strings.Replace(noSales, `"on":"success"},],`, `"on":"success"}],`, 1)
	status, raw := e.create(noSales)
	if status == http.StatusOK || !strings.Contains(raw, `answer \"sales\" has no transition`) {
		t.Errorf("create without the sales edge = %d: %s", status, raw)
	}
	_, verify, raw := e.teamdef(`{"op":"verify","name":"triage","overlay":` + noSales + `}`)
	issues, _ := verify["issues"].([]any)
	first, _ := issues[0].(map[string]any)
	if verify["valid"] != false || len(issues) != 1 || first["path"] != "states[1].handler.route" || first["severity"] != "refused" {
		t.Errorf("verify of the draft = %s, want one refusal at states[1].handler.route", raw)
	}

	oneOption := strings.Replace(triageOverlay, `{"billing":"invoices, refunds","support":"bugs, outages","sales":"upgrades"}`, `{"billing":null}`, 1)
	_, verify, raw = e.teamdef(`{"op":"verify","name":"triage","overlay":` + oneOption + `}`)
	var hit bool
	for _, is := range verify["issues"].([]any) {
		m := is.(map[string]any)
		if m["path"] == "states[1].handler.questions.route" && strings.Contains(m["detail"].(string), "bad_options") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("verify of a one-option choice = %s, want bad_options at states[1].handler.questions.route", raw)
	}
}

// What the deployment lacks does not refuse a save: the team is stored, and
// verify says it cannot run here, with the place.
func TestDecisionState_WhatTheDeploymentLacksIsUnrunnableNotRefused(t *testing.T) {
	kindAt := func(verify map[string]any) map[string]string {
		out := map[string]string{}
		issues, _ := verify["issues"].([]any)
		for _, is := range issues {
			m := is.(map[string]any)
			out[m["kind"].(string)] = m["severity"].(string) + " " + m["path"].(string)
		}
		return out
	}
	t.Run("no decision models", func(t *testing.T) {
		e := newDecisionEnv(t, false)
		if status, raw := e.create(triageOverlay); status != http.StatusOK {
			t.Fatalf("create = %d: %s", status, raw)
		}
		_, verify, raw := e.teamdef(`{"op":"verify","name":"triage"}`)
		if got := kindAt(verify); verify["runnable"] != false || got["decision_unconfigured"] != "unrunnable states[1].handler" {
			t.Errorf("verify = %s, want decision_unconfigured, unrunnable", raw)
		}
		status, _, raw := e.teamdef(`{"op":"run","name":"triage","input":"x"}`)
		if status == http.StatusOK || !strings.Contains(raw, `state \"triage\"`) || !strings.Contains(raw, "decision_not_configured") {
			t.Errorf("run = %d: %s, want the walk failed at triage with decision_not_configured", status, raw)
		}
	})
	t.Run("a model the operator does not list", func(t *testing.T) {
		e := newDecisionEnv(t, true)
		other := strings.Replace(triageOverlay, `"model":"decide"`, `"model":"elsewhere"`, 1)
		if status, raw := e.create(other); status != http.StatusOK {
			t.Fatalf("create = %d: %s", status, raw)
		}
		_, verify, raw := e.teamdef(`{"op":"verify","name":"triage"}`)
		if got := kindAt(verify); verify["runnable"] != false || got["decision_model_unknown"] != "unrunnable states[1].handler.model" {
			t.Errorf("verify = %s, want decision_model_unknown at the model", raw)
		}
		status, _, raw := e.teamdef(`{"op":"run","name":"triage","input":"x"}`)
		if status == http.StatusOK || !strings.Contains(raw, "model_not_allowed") || len(e.model.asked()) != 0 {
			t.Errorf("run = %d: %s after %d model calls, want model_not_allowed and none", status, raw, len(e.model.asked()))
		}
	})
	t.Run("more options than the model takes", func(t *testing.T) {
		e := newDecisionEnv(t, true)
		var opts []string
		for c := 'a'; c <= 'z'; c++ {
			opts = append(opts, `"`+string(c)+`":null`)
		}
		opts = append(opts, `"billing":null`, `"support":null`, `"sales":null`)
		many := strings.Replace(triageOverlay, `{"billing":"invoices, refunds","support":"bugs, outages","sales":"upgrades"}`, "{"+strings.Join(opts, ",")+"}", 1)
		many = strings.Replace(many, `{"from":"triage","to":"sales-desk","on":"conditional:sales"},`,
			`{"from":"triage","to":"sales-desk","on":"conditional:sales"},{"from":"triage","to":"done","on":"success"},`, 1)
		if status, raw := e.create(many); status != http.StatusOK {
			t.Fatalf("create = %d: %s", status, raw)
		}
		_, verify, raw := e.teamdef(`{"op":"verify","name":"triage"}`)
		if got := kindAt(verify); verify["runnable"] != false || got["decision_limits"] != "unrunnable states[1].handler.questions.route" {
			t.Errorf("verify = %s, want decision_limits at the question", raw)
		}
	})
}

// A failed call fails the walk at the state with the model's code, and the
// operator's endpoint is not in what the caller is told.
func TestDecisionState_AFailedCallFailsTheWalkWithItsCode(t *testing.T) {
	e := newDecisionEnv(t, true)
	if status, raw := e.create(triageOverlay); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, raw)
	}
	e.model.mu.Lock()
	e.model.status = http.StatusInternalServerError
	e.model.mu.Unlock()
	status, _, raw := e.teamdef(`{"op":"run","name":"triage","input":"x"}`)
	if status == http.StatusOK || !strings.Contains(raw, `state \"triage\"`) || !strings.Contains(raw, "decision: call_failed") {
		t.Errorf("run = %d: %s, want the walk failed at triage with call_failed", status, raw)
	}
	if strings.Contains(raw, strings.TrimPrefix(e.model.url, "http://")) {
		t.Errorf("the failure names the operator's endpoint: %s", raw)
	}
	if n := len(e.desks.asked()); n != 0 {
		t.Errorf("a desk ran %d times after the decision failed", n)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
