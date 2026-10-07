package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A resident child whose turn ended because it stopped at its iteration
// limit hands that turn back to its parent as an error naming the limit, with
// the turn's answer after the message — on open and on send alike. Its run
// stays completed/max_iterations. A turn that ends as usual is unchanged.

// laterCappedProvider answers a resident child's first instruction, then
// calls a tool on every turn it may once told to "keep going", so that send
// reaches its iteration limit and answers on the closing turn.
type laterCappedProvider struct{ cappingProvider }

func (p laterCappedProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if len(req.System) == 0 || !strings.Contains(req.System[0].Text, "you are capped later") || closingTurnAsked(req) {
		return p.cappingProvider.Call(ctx, req)
	}
	keepGoing := false
	for _, m := range req.Messages {
		for _, b := range m.Content {
			keepGoing = keepGoing || strings.Contains(b.Text, "keep going")
		}
	}
	text := "first answer"
	events := []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	if keepGoing {
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

func cappedResidentServer(t *testing.T) *Server {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"capped":       {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped", MaxIterations: 2},
		"capped-later": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped later", MaxIterations: 3},
		"whole":        {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are whole"},
	}
	srv, _ := makeServer(t, laterCappedProvider{}, cfg)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	return srv
}

// residentEnvelope is what open/send return for a turn that ended as usual.
type residentEnvelope struct {
	ChildRunID string `json:"child_run_id"`
	State      string `json:"state"`
	Output     string `json:"output"`
}

func TestResidentCapped_OpenGetsAnErrorWithTheTurnsAnswer(t *testing.T) {
	srv := cappedResidentServer(t)
	ctx, _ := lockedParentCtx(t, srv)
	agent := agentToolOf(t, srv)

	res, err := agent.Execute(ctx, json.RawMessage(`{"op":"open","name":"capped","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, `sub-agent "capped" stopped at its iteration limit of 2`) ||
		!strings.Contains(res.Text, "Its last answer:\n\ncapped last answer") {
		t.Fatalf("open of a child capped on its first turn = error:%v %q, want an error naming the limit with its answer", res.IsError, res.Text)
	}
	runID := runIDAfter(t, res.Text, "(run ")
	if run, err := srv.store.GetRun(context.Background(), runID); err != nil || run.Status != store.RunCompleted || run.StopReason != "max_iterations" {
		t.Errorf("capped resident's run = %+v (%v), want completed/max_iterations", run, err)
	}

	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"open","name":"whole","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env residentEnvelope
	if res.IsError || json.Unmarshal([]byte(res.Text), &env) != nil || env.State != "awaiting_input" || env.Output != "whole answer" {
		t.Errorf("open of a child that finished its turn = error:%v %q, want it parked with its answer", res.IsError, res.Text)
	}
}

func TestResidentCapped_SendGetsAnErrorWithTheTurnsAnswer(t *testing.T) {
	srv := cappedResidentServer(t)
	ctx, _ := lockedParentCtx(t, srv)
	agent := agentToolOf(t, srv)

	res, err := agent.Execute(ctx, json.RawMessage(`{"op":"open","name":"capped-later","prompt":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env residentEnvelope
	if res.IsError || json.Unmarshal([]byte(res.Text), &env) != nil || env.State != "awaiting_input" || env.Output != "first answer" {
		t.Fatalf("open = error:%v %q, want it parked with its first answer", res.IsError, res.Text)
	}

	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"send","child_run_id":"`+env.ChildRunID+`","prompt":"keep going"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, `sub-agent "capped-later" stopped at its iteration limit of 3`) ||
		!strings.Contains(res.Text, "Its last answer:\n\ncapped last answer") || !strings.Contains(res.Text, env.ChildRunID) {
		t.Fatalf("send whose turn hit the limit = error:%v %q, want an error naming the limit with the turn's answer", res.IsError, res.Text)
	}
	if run, err := srv.store.GetRun(context.Background(), env.ChildRunID); err != nil || run.Status != store.RunCompleted || run.StopReason != "max_iterations" {
		t.Errorf("capped resident's run = %+v (%v), want completed/max_iterations", run, err)
	}
}

// A send whose turn ends as usual is unchanged.
func TestResidentCapped_ASendThatFinishesIsUnchanged(t *testing.T) {
	srv := cappedResidentServer(t)
	ctx, _ := lockedParentCtx(t, srv)
	agent := agentToolOf(t, srv)

	res, err := agent.Execute(ctx, json.RawMessage(`{"op":"open","name":"whole","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env residentEnvelope
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatalf("open = %q: %v", res.Text, err)
	}
	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"send","child_run_id":"`+env.ChildRunID+`","prompt":"again"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || json.Unmarshal([]byte(res.Text), &env) != nil || env.State != "awaiting_input" || env.Output != "whole answer" {
		t.Errorf("send of a turn that finished = error:%v %q, want it parked with its answer", res.IsError, res.Text)
	}
	_, _ = agent.Execute(ctx, json.RawMessage(`{"op":"close","child_run_id":"`+env.ChildRunID+`"}`))
}

// runIDAfter reads the run id that follows marker in text.
func runIDAfter(t *testing.T, text, marker string) string {
	t.Helper()
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no %q in %q", marker, text)
	}
	id, _, _ := strings.Cut(text[i+len(marker):], ")")
	return id
}

// A resident child with no max_iterations of its own runs unbounded (the loop
// lifts the default for an interactive run), so its message names no limit;
// one with a limit names it, and a one-shot run with none names the default.
func TestIterationLimitOf_AnInteractiveRunWithNoLimitHasNone(t *testing.T) {
	q := make(chan steer.Message)
	for _, tc := range []struct {
		name string
		opts loop.RunOptions
		want int
	}{
		{"interactive, no limit", loop.RunOptions{Interactive: true, SteerQueue: q}, 0},
		{"interactive, a limit", loop.RunOptions{Interactive: true, SteerQueue: q, MaxIterations: 5}, 5},
		{"one-shot, no limit", loop.RunOptions{}, loop.DefaultMaxIterations},
	} {
		if got := iterationLimitOf(tc.opts); got != tc.want {
			t.Errorf("%s: limit = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A subagent_stop refusal names what the parent can still do with the child:
// one that is still open may be sent to again or closed; one whose run has
// ended — here at its iteration limit, refused at its open and again when a
// later poll reads its ending — can only be replaced.
func TestResidentHandBack_AHookRefusalSaysWhetherTheChildIsStillOpen(t *testing.T) {
	srv := cappedResidentServer(t)
	ctx := tools.WithAgentName(residentParentCtx("parent-agent", ""), "lead")
	deny := newRecordingHook(t, `{"decision":"deny","reason":"not good enough"}`)
	register(t, srv, &hooks.Hook{Owner: "ops", Name: "check", Phase: hooks.PhaseSubagentStop, Agents: []string{"lead"}, CallbackURL: deny.srv.URL})

	runID, _, _, err := srv.openResidentChild(ctx, "whole", "x", "", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "was refused: not good enough") ||
		!strings.Contains(err.Error(), "is still open: send again or close it") {
		t.Errorf("refused turn of an open child: %v, want it still open", err)
	}
	_ = srv.closeResidentChild(ctx, runID)

	runID, _, _, err = srv.openResidentChild(ctx, "capped", "x", "", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "was refused: not good enough") {
		t.Fatalf("refused last turn of a capped child: %v, want the refusal", err)
	}
	for _, wrong := range []string{"is still open", "send again"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("refused last turn of a capped child: %v, says %q", err, wrong)
		}
	}
	if !strings.Contains(err.Error(), "has ended") {
		t.Errorf("refused last turn of a capped child: %v, want it to say the child has ended", err)
	}
	waitResidentGone(t, srv, runID)
	if _, _, err := srv.pollResidentChild(ctx, runID, 0); err == nil || !strings.Contains(err.Error(), "was refused") ||
		strings.Contains(err.Error(), "is still open") || !strings.Contains(err.Error(), "has ended") {
		t.Errorf("poll of the ended child under the refusal: %v, want it refused and ended", err)
	}
}
