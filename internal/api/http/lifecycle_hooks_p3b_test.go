package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/hooks/codehook"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// recordingHook answers with resp and keeps each payload.
type recordingHook struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newRecordingHook(t *testing.T, resp string) *recordingHook {
	t.Helper()
	h := &recordingHook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(b))
		h.mu.Unlock()
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// waitBody returns the first payload containing want, failing after 3s.
func (h *recordingHook) waitBody(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, b := range h.bodies {
			if strings.Contains(b, want) {
				h.mu.Unlock()
				return b
			}
		}
		h.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no hook payload containing %s", want)
	return ""
}

func register(t *testing.T, s *Server, h *hooks.Hook) {
	t.Helper()
	if _, err := s.testHooks().Register(h); err != nil {
		t.Fatal(err)
	}
}

// parentCtx is the context of a parent run's Agent tool call.
func parentCtx(t *testing.T, s *Server) (context.Context, *[]providers.Event) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.store.CreateSession(ctx, "", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_lead", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []providers.Event
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_lead"})
	ctx = tools.WithRunID(ctx, run.ID)
	ctx = tools.WithAgentName(ctx, "lead")
	ctx = tools.WithEventEmitter(ctx, func(ev providers.Event) { mu.Lock(); events = append(events, ev); mu.Unlock() })
	return ctx, &events
}

// subagent_start may refuse a child before it is created: the parent's call
// gets the reason and the child never reaches a model.
func TestSubagentHooks_AStartDenyRefusesTheChild(t *testing.T) {
	h := newReviewHarness(t)
	deny := newRecordingHook(t, `{"decision":"deny","reason":"lead may not spawn writers today"}`)
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "gate", Phase: hooks.PhaseSubagentStart, Agents: []string{"lead"}, CallbackURL: deny.srv.URL})
	ctx, _ := parentCtx(t, h.srv)
	_, _, runID, err := h.srv.runAgentToolChild(ctx, "writer", "go", "")
	if err == nil || !strings.Contains(err.Error(), "was not started: lead may not spawn writers today") || runID != "" {
		t.Fatalf("err = %v, run %q", err, runID)
	}
	h.prov.mu.Lock()
	calls := len(h.prov.lastUsers)
	h.prov.mu.Unlock()
	if calls != 0 {
		t.Errorf("the child reached the model %d times", calls)
	}
	body := deny.waitBody(t, `"phase":"subagent_start"`)
	for _, want := range []string{`"agent":"lead"`, `"subagent":"writer"`} {
		if !strings.Contains(body, want) {
			t.Errorf("payload lacks %s: %s", want, body)
		}
	}
}

// Context from subagent_start reaches the child's prompt; context from
// subagent_stop reaches the parent with the child's result; each decision is
// recorded on the parent's stream.
func TestSubagentHooks_ContextReachesTheChildAndTheParent(t *testing.T) {
	h := newReviewHarness(t)
	start := newRecordingHook(t, `{"additional_context":"Answer in one line."}`)
	stop := newRecordingHook(t, `{"additional_context":"(checked by ops)"}`)
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "brief", Phase: hooks.PhaseSubagentStart, CallbackURL: start.srv.URL})
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "check", Phase: hooks.PhaseSubagentStop, CallbackURL: stop.srv.URL})
	ctx, events := parentCtx(t, h.srv)
	out, _, runID, err := h.srv.runAgentToolChild(ctx, "writer", "write the plan", "")
	if err != nil {
		t.Fatal(err)
	}
	h.prov.mu.Lock()
	prompt := h.prov.lastUsers[0]
	h.prov.mu.Unlock()
	if !strings.Contains(prompt, "write the plan") || !strings.Contains(prompt, "Answer in one line.") {
		t.Errorf("the child's prompt = %q", prompt)
	}
	if !strings.HasSuffix(out, "(checked by ops)") {
		t.Errorf("the parent sees %q", out)
	}
	body := stop.waitBody(t, `"phase":"subagent_stop"`)
	for _, want := range []string{`"subagent_run_id":"` + runID + `"`, `"status":"completed"`} {
		if !strings.Contains(body, want) {
			t.Errorf("payload lacks %s: %s", want, body)
		}
	}
	var kinds []string
	for _, ev := range *events {
		if ev.HookDecision != nil {
			kinds = append(kinds, ev.HookDecision.Phase+":"+ev.HookDecision.Decision)
		}
	}
	if strings.Join(kinds, ",") != "subagent_start:context,subagent_stop:context" {
		t.Errorf("recorded decisions = %v", kinds)
	}
}

// subagent_stop may refuse a child's result: the parent gets the reason as an
// error, with the child's run id so it can be found.
func TestSubagentHooks_AStopDenyRefusesTheResult(t *testing.T) {
	h := newReviewHarness(t)
	deny := newRecordingHook(t, `{"decision":"deny","reason":"the plan cites no source"}`)
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "check", Phase: hooks.PhaseSubagentStop, CallbackURL: deny.srv.URL})
	ctx, _ := parentCtx(t, h.srv)
	out, _, runID, err := h.srv.runAgentToolChild(ctx, "writer", "write the plan", "")
	if err == nil || !strings.Contains(err.Error(), "was refused: the plan cites no source") || out != "" || runID == "" {
		t.Fatalf("out %q run %q err %v", out, runID, err)
	}
}

// A pre_compact hook refuses a manual compaction before any summary is made:
// the caller gets a 409 naming the reason. post_compact reports one that went
// through.
func TestCompactRun_HooksGateAndReportAManualCompaction(t *testing.T) {
	srv, prov := compactFixture(t)
	_, _, runID := seedContinuationSession(t, srv, 8, 1)
	deny := newRecordingHook(t, `{"decision":"deny","reason":"keep the whole history for the audit"}`)
	if _, err := srv.testHooks().Register(&hooks.Hook{Owner: "ops", Name: "keep", Phase: hooks.PhasePreCompact, CallbackURL: deny.srv.URL}); err != nil {
		t.Fatal(err)
	}
	_, err := srv.CompactRun(context.Background(), runID)
	var ce *compactErr
	if !errors.As(err, &ce) || ce.status != http.StatusConflict || ce.code != "denied_by_hook" || !strings.Contains(ce.msg, "keep the whole history") {
		t.Fatalf("err = %#v", err)
	}
	if n := prov.calls.Load(); n != 0 {
		t.Errorf("the summary was made (%d calls) for a refused compaction", n)
	}
	if !strings.Contains(deny.waitBody(t, `"phase":"pre_compact"`), `"trigger":"manual"`) {
		t.Error("the payload does not name the trigger")
	}

	srv.resetTestHooks()
	report := newRecordingHook(t, `{}`)
	register(t, srv, &hooks.Hook{Owner: "ops", Name: "log", Phase: hooks.PhasePostCompact, CallbackURL: report.srv.URL})
	res, err := srv.CompactRun(context.Background(), runID)
	if err != nil || !res.Compacted {
		t.Fatalf("res = %+v, %v", res, err)
	}
	body := report.waitBody(t, `"phase":"post_compact"`)
	if !strings.Contains(body, `"trigger":"manual"`) || !strings.Contains(body, `"after_tokens":`) {
		t.Errorf("payload = %s", body)
	}
}

// run_end reports a finished run, after its row is final, through the real
// server.
func TestRunEnd_ReportsAFinishedRun(t *testing.T) {
	h := newReviewHarness(t)
	end := newRecordingHook(t, `{"decision":"deny"}`)
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "log", Phase: hooks.PhaseRunEnd, CallbackURL: end.srv.URL})
	runID, _, frames, stop := h.start(`{"agent":"writer","segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
	defer stop()
	h.waitFrame(frames, "done")
	body := end.waitBody(t, `"run_id":"`+runID+`"`)
	for _, want := range []string{`"phase":"run_end"`, `"status":"completed"`, `"agent":"writer"`} {
		if !strings.Contains(body, want) {
			t.Errorf("payload lacks %s: %s", want, body)
		}
	}
	if run, _ := h.st.GetRun(t.Context(), runID); run.Status != store.RunCompleted {
		t.Errorf("a run_end deny changed the run: %q", run.Status)
	}
}

// The crossing: a real parent run's Agent tool call goes through the
// subagent hooks. A subagent_start deny reaches the parent's model as its tool
// result, and the child never makes a model call.
func TestSubagentHooks_TheAgentToolGoesThroughThem(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the parent"},
			"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are the child"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{scripts: [][]providers.Event{
		{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"say hello"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
		},
		{
			{Type: providers.EventText, Text: "parent done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent-hooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	deny := newRecordingHook(t, `{"decision":"deny","reason":"no children today"}`)
	register(t, srv, &hooks.Hook{Owner: "ops", Name: "gate", Phase: hooks.PhaseSubagentStart, Agents: []string{"parent"}, CallbackURL: deny.srv.URL})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "was not started: no children today") {
		t.Fatalf("the parent's stream has no denied tool result: %s", body)
	}
	if n := prov.calls.Load(); n != 2 {
		t.Errorf("model calls = %d, want 2 (the parent's two turns; the child made none)", n)
	}
	if !strings.Contains(deny.waitBody(t, `"subagent":"child"`), `"agent":"parent"`) {
		t.Error("the hook did not see the parent as the agent")
	}
}

// runRecordingTool stands in for the Interruption tool and records the run
// each call was made on.
type runRecordingTool struct {
	mu   sync.Mutex
	runs []string
}

func (r *runRecordingTool) Name() string                 { return "Interruption" }
func (r *runRecordingTool) Description() string          { return "" }
func (r *runRecordingTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (r *runRecordingTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	r.mu.Lock()
	r.runs = append(r.runs, tools.RunID(ctx))
	r.mu.Unlock()
	return tools.Result{Text: `{}`}, nil
}

// A hook fired outside the run's loop still knows its run: a run_end code
// hook's notify is made on the run that ended. It used to be made on no run,
// and the Interruption tool refused it.
func TestRunEnd_ACodeHooksNotifyIsMadeOnTheRun(t *testing.T) {
	h := newReviewHarness(t)
	rec := &runRecordingTool{}
	h.srv.SetCodeHookRunner(codehook.New(rec))
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "tell", Phase: hooks.PhaseRunEnd,
		Code: `function hook(ev) { Interruption.notify({message: "ended " + ev.status}); }`})
	runID, _, frames, stop := h.start(`{"agent":"writer","segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
	defer stop()
	h.waitFrame(frames, "done")
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.mu.Lock()
		runs := append([]string(nil), rec.runs...)
		rec.mu.Unlock()
		if len(runs) > 0 {
			if runs[0] != runID {
				t.Fatalf("notify made on run %q, want %q", runs[0], runID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run_end hook never notified")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
