package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Strict budgets: with LOOMCYCLE_LIMITS_ENFORCE=strict a run that takes its
// (tenant, user) over a hard budget makes no further model call and starts no
// further sub-agent. Without it nothing here changes: the budget is checked
// where a run is admitted and a run in flight finishes.
//
// Every fixture reports its usage on the done event, as the real drivers do.

// spend is a done event that reports n input tokens.
func spend(stop string, n int) providers.Event {
	return providers.Event{Type: providers.EventDone, StopReason: stop, Usage: &providers.Usage{InputTokens: n}}
}

func spawnCall(id, child string) providers.Event {
	return providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
		ID: id, Name: "Agent", Input: json.RawMessage(`{"name":"` + child + `","prompt":"go"}`),
	}}
}

// unknownToolCall keeps a run going one more iteration: the dispatcher answers
// it with an error and the loop goes back to the model.
func unknownToolCall(id string) providers.Event {
	return providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
		ID: id, Name: "Nope", Input: json.RawMessage(`{}`),
	}}
}

var finalAnswer = []providers.Event{{Type: providers.EventText, Text: "done"}, spend("end_turn", 1)}

func strictLimitsConfig() *config.Config {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the parent"},
			"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are the child"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	return cfg
}

// strictLimitsServer is a server whose operator budget is a hard 100 tokens,
// with nothing spent yet.
func strictLimitsServer(t *testing.T, prov providers.Provider, strict bool) *Server {
	t.Helper()
	srv, _ := makeServer(t, prov, strictLimitsConfig())
	ctx := context.Background()
	if err := srv.store.TokenLimitPut(ctx, store.TokenLimitRow{Scope: "operator", HardLimit: i64p(100), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedLimits(ctx); err != nil {
		t.Fatal(err)
	}
	srv.SetStrictLimits(strict)
	return srv
}

func postStrictRun(t *testing.T, srv *Server, body string) (int, string) {
	t.Helper()
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const parentRunBody = `{"agent":"parent","agent_id":"a_parent","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`

func runOfAgent(t *testing.T, srv *Server, agentID string) store.Run {
	t.Helper()
	run, err := srv.store.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatalf("GetRunByAgentID(%s): %v", agentID, err)
	}
	return run
}

func assertStoppedAtBudget(t *testing.T, run store.Run) {
	t.Helper()
	if run.Status != store.RunFailed || run.StopReason != loop.StopReasonTokenLimit {
		t.Errorf("run %s (%s): status=%q stop_reason=%q, want failed / %s", run.ID, run.Agent, run.Status, run.StopReason, loop.StopReasonTokenLimit)
	}
	if !strings.Contains(run.ErrorMsg, runner.ErrTokenLimitExceeded.Error()) {
		t.Errorf("run %s error = %q, want it to name %s", run.ID, run.ErrorMsg, runner.ErrTokenLimitExceeded)
	}
}

func TestStrictLimits_ARunOverItsBudgetMakesNoFurtherModelCall(t *testing.T) {
	prov := &scriptedProvider{
		scripts:  [][]providers.Event{{unknownToolCall("tu_1"), spend("tool_use", 200)}},
		defaultS: finalAnswer,
	}
	srv := strictLimitsServer(t, prov, true)
	code, body := postStrictRun(t, srv, parentRunBody)
	if code != http.StatusOK {
		t.Fatalf("the run was under budget when it started; got %d %s", code, body)
	}
	if n := prov.calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1: the first took the caller over budget", n)
	}
	assertStoppedAtBudget(t, runOfAgent(t, srv, "a_parent"))
	if !strings.Contains(body, runner.ErrTokenLimitExceeded.Error()) {
		t.Errorf("the stream does not say why the run stopped:\n%s", body)
	}
}

func TestStrictLimits_WithoutStrictModeARunInFlightFinishes(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{spawnCall("tu_1", "child"), spend("tool_use", 200)},
			finalAnswer, // the child
		},
		defaultS: finalAnswer,
	}
	srv := strictLimitsServer(t, prov, false)
	if code, body := postStrictRun(t, srv, parentRunBody); code != http.StatusOK {
		t.Fatalf("got %d %s", code, body)
	}
	if n := prov.calls.Load(); n != 3 {
		t.Errorf("model calls = %d, want 3 (parent, child, parent)", n)
	}
	if run := runOfAgent(t, srv, "a_parent"); run.Status != store.RunCompleted {
		t.Errorf("status = %q (%s), want completed", run.Status, run.ErrorMsg)
	}
}

func TestStrictLimits_ATreeOverItsBudgetStartsNoSubAgent(t *testing.T) {
	prov := &scriptedProvider{
		scripts:  [][]providers.Event{{spawnCall("tu_1", "child"), spend("tool_use", 200)}},
		defaultS: finalAnswer,
	}
	srv := strictLimitsServer(t, prov, true)
	_, body := postStrictRun(t, srv, parentRunBody)

	// Nothing was written for the child: the refusal comes before its run row.
	children, err := srv.store.ListRunsByParentAgentID(context.Background(), "a_parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Errorf("%d child run(s) were created; the spawn must be refused before any row", len(children))
	}
	// The parent is told why, as the spawn's own result.
	refused := false
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `"type":"tool_result"`) && strings.Contains(line, runner.ErrTokenLimitExceeded.Error()) && strings.Contains(line, `"category":"business"`) {
			refused = true
		}
	}
	if !refused {
		t.Errorf("the parent's tool result does not carry the refusal:\n%s", body)
	}
	if n := prov.calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1", n)
	}
}

func TestStrictLimits_ASubAgentThatGoesOverBudgetStopsToo(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{spawnCall("tu_1", "child"), spend("tool_use", 10)},
			{unknownToolCall("tu_c1"), spend("tool_use", 200)}, // the child goes over
		},
		defaultS: finalAnswer,
	}
	srv := strictLimitsServer(t, prov, true)
	postStrictRun(t, srv, parentRunBody)

	if n := prov.calls.Load(); n != 2 {
		t.Errorf("model calls = %d, want 2 (parent, child): neither may call again", n)
	}
	children, err := srv.store.ListRunsByParentAgentID(context.Background(), "a_parent")
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, err = %v; want the one child", len(children), err)
	}
	assertStoppedAtBudget(t, children[0])
	assertStoppedAtBudget(t, runOfAgent(t, srv, "a_parent"))
}

// A continuation and a run started without HTTP build their own loop options;
// each has to carry the check.
func TestStrictLimits_AContinuationOverItsBudgetStops(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			finalAnswer,
			{unknownToolCall("tu_1"), spend("tool_use", 200)},
		},
		defaultS: finalAnswer,
	}
	srv := strictLimitsServer(t, prov, true)
	_, body := postStrictRun(t, srv, parentRunBody)
	sessionID := extractSessionID(body)
	if sessionID == "" {
		t.Fatalf("no session id in:\n%s", body)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/sessions/"+sessionID+"/messages", "application/json", strings.NewReader(
		`{"agent_id":"a_cont","segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("continuation = %d %s", resp.StatusCode, b)
	}
	if n := prov.calls.Load(); n != 2 {
		t.Errorf("model calls = %d, want 2", n)
	}
	assertStoppedAtBudget(t, runOfAgent(t, srv, "a_cont"))
}

func TestStrictLimits_ARunStartedOffHTTPOverItsBudgetStops(t *testing.T) {
	prov := &scriptedProvider{
		scripts:  [][]providers.Event{{unknownToolCall("tu_1"), spend("tool_use", 200)}},
		defaultS: finalAnswer,
	}
	srv := strictLimitsServer(t, prov, true)
	var streamed []string
	err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "parent", AgentID: "a_once", UserID: "alice",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
	}, runner.RunCallbacks{OnEvent: func(ev providers.Event) {
		if ev.Type == providers.EventError {
			streamed = append(streamed, ev.Error)
		}
	}})
	// A run that started and then failed is reported on its stream, not as
	// RunOnce's error: that is for a run that never started.
	if err != nil {
		t.Errorf("RunOnce = %v, want nil", err)
	}
	if len(streamed) != 1 || !strings.Contains(streamed[0], runner.ErrTokenLimitExceeded.Error()) {
		t.Errorf("error events = %q, want the one budget error", streamed)
	}
	if n := prov.calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1", n)
	}
	assertStoppedAtBudget(t, runOfAgent(t, srv, "a_once"))
}

// A paused run restored onto a server whose budget is spent is not handed to
// a provider: it ends at the budget like any other run in flight.
func TestStrictLimits_AResumedRunOverItsBudgetMakesNoModelCall(t *testing.T) {
	for _, strict := range []bool{true, false} {
		srv, prov, runID, envelope := restoreResumeServer(t)
		ctx := context.Background()
		seedOperatorUsage(t, srv, 1000)
		if err := srv.store.TokenLimitPut(ctx, store.TokenLimitRow{Scope: "operator", HardLimit: i64p(500), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := srv.SeedLimits(ctx); err != nil {
			t.Fatal(err)
		}
		srv.SetStrictLimits(strict)
		if _, err := srv.RestoreSnapshot(ctx, connector.RestoreSnapshotRequest{RawJSON: envelope}); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		var run store.Run
		deadline := time.Now().Add(5 * time.Second)
		for {
			var err error
			if run, err = srv.store.GetRun(ctx, runID); err != nil {
				t.Fatal(err)
			}
			if run.Status == store.RunCompleted || run.Status == store.RunFailed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("strict=%v: the restored run never ended (status=%q)", strict, run.Status)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !strict {
			if run.Status != store.RunCompleted || prov.calls.Load() != 1 {
				t.Errorf("without strict mode a resumed run finishes: status=%q calls=%d", run.Status, prov.calls.Load())
			}
			continue
		}
		assertStoppedAtBudget(t, run)
		if n := prov.calls.Load(); n != 0 {
			t.Errorf("model calls = %d, want 0", n)
		}
	}
}
