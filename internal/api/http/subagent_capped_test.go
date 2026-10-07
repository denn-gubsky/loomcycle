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
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// cappingProvider answers two kinds of child. "you are capped" calls a tool
// on every turn it may, so it reaches its iteration limit, and answers on the
// closing turn that follows; "you are whole" answers at once. "you are the
// parent" fans out to a capped child, then answers once it has the result.
type cappingProvider struct{}

func (cappingProvider) ID() string                  { return "stub" }
func (cappingProvider) Probe(context.Context) error { return nil }
func (cappingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (cappingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (cappingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	answer := func(text string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventText, Text: text},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	var events []providers.Event
	system := ""
	if len(req.System) > 0 {
		system = req.System[0].Text
	}
	switch {
	case strings.Contains(system, "you are whole"):
		events = answer("whole answer")
	case strings.Contains(system, "you are the parent") && len(req.Messages) > 1:
		events = answer("parent done")
	case strings.Contains(system, "you are the parent"):
		events = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: json.RawMessage(
				`{"op":"parallel_spawn","spawns":[{"name":"capped","prompt":"x"}]}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	case closingTurnAsked(req):
		events = answer("capped last answer")
	default:
		events = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_busy", Name: "Agent", Input: json.RawMessage(`{"op":"bogus"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// closingTurnAsked reports whether req is a closing turn: its last message
// tells the model it has used every iteration.
func closingTurnAsked(req providers.Request) bool {
	if len(req.Messages) == 0 {
		return false
	}
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if strings.Contains(b.Text, "used every iteration") {
			return true
		}
	}
	return false
}

func cappedServer(t *testing.T) *Server {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"capped": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped", MaxIterations: 2},
		"whole":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are whole"},
	}
	srv, _ := makeServer(t, cappingProvider{}, cfg)
	return srv
}

// A child that stops at its iteration limit reaches its parent's spawn as an
// error naming the limit, with its last answer after it; its own run stays
// completed with stop reason max_iterations. A child that finishes is handed
// back as before.
func TestSubAgentCapped_SpawnGetsAnErrorWithTheChildsLastAnswer(t *testing.T) {
	srv := cappedServer(t)
	ctx, _ := lockedParentCtx(t, srv)
	agent := agentToolOf(t, srv)

	res, err := agent.Execute(ctx, json.RawMessage(`{"op":"spawn","name":"capped","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, `sub-agent "capped" stopped at its iteration limit of 2`) ||
		!strings.Contains(res.Text, "capped last answer") {
		t.Fatalf("spawn of a capped child = error:%v %q, want an error naming the limit with its last answer", res.IsError, res.Text)
	}
	runID := subAgentRunIDIn(t, res.Text)
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunCompleted || run.StopReason != "max_iterations" {
		t.Errorf("capped child's run = %s/%s, want completed/max_iterations", run.Status, run.StopReason)
	}

	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"spawn","name":"whole","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, "whole answer") || strings.Contains(res.Text, "iteration limit") {
		t.Errorf("spawn of a child that finished = error:%v %q, want its answer as the result", res.IsError, res.Text)
	}
}

// subAgentRunIDIn reads the run id from a sub-agent header in text.
func subAgentRunIDIn(t *testing.T, text string) string {
	t.Helper()
	i := strings.Index(text, "run_id=")
	if i < 0 {
		t.Fatalf("no run_id in %q", text)
	}
	id, _, _ := strings.Cut(text[i+len("run_id="):], "]")
	return id
}

// In a wait-mode parallel_spawn, a capped child's row is ok:false with status
// max_iterations, an error naming the limit and its last answer kept as
// output; its sibling that finished is an ordinary row.
func TestSubAgentCapped_ParallelSpawnRowIsNotOkAndKeepsTheAnswer(t *testing.T) {
	srv := cappedServer(t)
	ctx, _ := lockedParentCtx(t, srv)
	res, err := agentToolOf(t, srv).Execute(ctx, json.RawMessage(
		`{"op":"parallel_spawn","spawns":[{"name":"capped","prompt":"x"},{"name":"whole","prompt":"y"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil || len(env.Results) != 2 {
		t.Fatalf("envelope = %s (%v)", res.Text, err)
	}
	c, w := env.Results[0], env.Results[1]
	if c.Ok || c.Status != "max_iterations" || !strings.Contains(c.Error, "stopped at its iteration limit of 2") ||
		!strings.Contains(c.Output, "capped last answer") || c.RunID == "" {
		t.Errorf("capped row = %+v, want ok:false status max_iterations, the limit in error, its answer in output", c)
	}
	if !w.Ok || w.Status != "" || w.Error != "" || !strings.Contains(w.Output, "whole answer") {
		t.Errorf("finished row = %+v, want an ordinary ok row", w)
	}
}

// The ledger result row of a capped wait-mode parallel_spawn child records its
// status, so a parent resumed from a snapshot answers it as it was answered.
func TestResumeFanout_ACappedChildsResultRowRecordsItsStatus(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Env.ResumeFanout = true
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the parent", MaxConcurrentChildren: 4},
		"capped": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped", MaxIterations: 2},
	}
	srv, _ := makeServer(t, cappingProvider{}, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"a_fan_capped","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "parent done") {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	ctx := context.Background()
	parent, err := srv.store.GetRunByAgentID(ctx, "a_fan_capped")
	if err != nil {
		t.Fatal(err)
	}
	events, err := srv.store.GetTranscript(ctx, parent.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var rows []providers.SpawnChildEventInfo
	for _, e := range events {
		var pe providers.Event
		if e.RunID == parent.ID && e.Type == string(providers.EventSpawnChildResult) && json.Unmarshal(e.Payload, &pe) == nil && pe.SpawnChild != nil {
			rows = append(rows, *pe.SpawnChild)
		}
	}
	if len(rows) != 1 || rows[0].Ok || rows[0].Status != "max_iterations" || !strings.Contains(rows[0].Output, "capped last answer") {
		t.Errorf("result rows = %+v, want one, ok:false status max_iterations with the answer", rows)
	}
}

// A poll-mode capped child reads failed with status max_iterations, the
// limit in its error and its last answer as output; its sibling that finished
// reads completed as before.
func TestSubAgentCapped_PollRowIsFailedAndKeepsTheAnswer(t *testing.T) {
	srv := cappedServer(t)
	ctx, _ := lockedParentCtx(t, srv)
	life, end := context.WithCancel(context.Background())
	defer end()
	bg := tools.NewBackground(life)
	defer bg.Close(nil)
	ctx = tools.WithBackground(ctx, bg)
	ctx = tools.WithIterationBudget(ctx, 1, 10, false)
	agent := agentToolOf(t, srv)

	start, err := agent.Execute(ctx, json.RawMessage(
		`{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"capped","prompt":"x"},{"name":"whole","prompt":"y"}]}`))
	if err != nil || start.IsError {
		t.Fatalf("poll-mode parallel_spawn = %q (%v)", start.Text, err)
	}
	var batch struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal([]byte(start.Text), &batch); err != nil || batch.BatchID == "" {
		t.Fatalf("batch answer = %s (%v)", start.Text, err)
	}
	type row struct {
		Index  *int   `json:"index"`
		State  string `json:"state"`
		Output string `json:"output"`
		Error  string `json:"error"`
		Status string `json:"status"`
	}
	var rows []row
	for deadline := time.Now().Add(10 * time.Second); ; {
		res, err := agent.Execute(ctx, json.RawMessage(`{"op":"poll","batch_id":"`+batch.BatchID+`","wait":"all","wait_ms":5000}`))
		if err != nil || res.IsError {
			t.Fatalf("poll = %q (%v)", res.Text, err)
		}
		var p struct {
			Children []row `json:"children"`
			Pending  int   `json:"pending"`
		}
		if err := json.Unmarshal([]byte(res.Text), &p); err != nil {
			t.Fatalf("poll answer %q: %v", res.Text, err)
		}
		if p.Pending == 0 {
			rows = p.Children
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("children still pending: %s", res.Text)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	for _, r := range rows {
		switch {
		case r.Index != nil && *r.Index == 0:
			if r.State != tools.ChildFailed || r.Status != "max_iterations" || !strings.Contains(r.Error, "stopped at its iteration limit of 2") ||
				!strings.Contains(r.Output, "capped last answer") {
				t.Errorf("capped row = %+v, want failed, status max_iterations, the limit in error, its answer in output", r)
			}
		default:
			if r.State != tools.ChildCompleted || r.Status != "" || r.Error != "" || !strings.Contains(r.Output, "whole answer") {
				t.Errorf("finished row = %+v, want completed with its answer", r)
			}
		}
	}
}

// A fan-out parent resumed from a snapshot answers a capped child as the live
// call does — from the ledger row of one that ended before the snapshot, and
// from the run row of one re-dispatched across it.
func TestResumeFanout_ACappedChildIsAnsweredCapped(t *testing.T) {
	srv := cappedServer(t)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	sess, _ := srv.store.CreateSession(ctx, "", "capped", "alice")
	child, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_capped_resumed", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishRun(ctx, child.ID, store.RunCompleted, "max_iterations", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	row := func(typ providers.EventType, sc providers.SpawnChildEventInfo) store.Event {
		sc.ToolUseID = "tu_fan"
		payload, _ := json.Marshal(providers.Event{Type: typ, SpawnChild: &sc})
		return store.Event{Type: string(typ), Payload: payload}
	}
	events := []store.Event{
		row(providers.EventSpawnChildStarted, providers.SpawnChildEventInfo{Index: 0, RunID: "r_before", Agent: "capped"}),
		row(providers.EventSpawnChildResult, providers.SpawnChildEventInfo{Index: 0, RunID: "r_before", Agent: "capped",
			Error: builtin.ChildCappedMessage("capped", 2, "r_before"), Output: "answer before", Status: "max_iterations"}),
		row(providers.EventSpawnChildStarted, providers.SpawnChildEventInfo{Index: 1, RunID: child.ID, Agent: "capped"}),
	}
	msg, err := srv.reconcileFanoutParent(ctx, store.Run{ID: "r_parent"}, events,
		fanoutParkInfo{toolUseID: "tu_fan", input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"capped","prompt":"x"},{"name":"capped","prompt":"y"}]}`)},
		func(providers.Event) {})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(msg.Content[0].Text), &env); err != nil || len(env.Results) != 2 {
		t.Fatalf("envelope = %s (%v)", msg.Content[0].Text, err)
	}
	if r := env.Results[0]; r.Ok || r.Status != "max_iterations" || r.Output != "answer before" {
		t.Errorf("ledger row = %+v, want it as recorded: ok:false status max_iterations with its answer", r)
	}
	if r := env.Results[1]; r.Ok || r.Status != "max_iterations" || !strings.Contains(r.Error, "stopped at its iteration limit") ||
		!strings.Contains(r.Output, "run_id="+child.ID) {
		t.Errorf("re-dispatched row = %+v, want ok:false status max_iterations with its output", r)
	}
}

// A poll-mode child that ended at its limit while its parent was paused is
// read back from its run row as the live child is filed: failed, status
// max_iterations, its last answer kept.
func TestRestoredAgentEnding_ACappedChildIsFailedWithItsAnswer(t *testing.T) {
	srv := cappedServer(t)
	ctx := context.Background()
	sess, _ := srv.store.CreateSession(ctx, "", "capped", "alice")
	child, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_capped_restored", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishRun(ctx, child.ID, store.RunCompleted, "max_iterations",
		store.Usage{Result: json.RawMessage(`{"final_text":"restored last answer"}`)}, ""); err != nil {
		t.Fatal(err)
	}
	child, err = srv.store.GetRun(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, res := srv.restoredAgentEnding(ctx, child, tools.ChildSpec{RunID: child.ID, Agent: "capped", Index: -1})
	if state != tools.ChildFailed || res.Status != "max_iterations" || !strings.Contains(res.Error, "stopped at its iteration limit") ||
		!strings.Contains(res.Output, "restored last answer") {
		t.Errorf("ending = %s %+v, want failed, status max_iterations, its answer kept", state, res)
	}
}
