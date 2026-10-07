package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// errCapped stands in for the server's error for a member that stopped at its
// iteration limit.
var errCapped = errors.New(`sub-agent "capped" stopped at its iteration limit of 2 before it finished, so its last answer may be incomplete (run run_capped)`)

// cappedOrWhole answers as the server's member runner does: "capped" (or a
// Starter run handed a payload naming it) stops at its iteration limit — an
// error, Capped, its last answer kept — and every other member finishes.
func cappedOrWhole(_ context.Context, agent string, p Prompt, _ string) (SpawnResult, error) {
	if agent == "capped" || strings.Contains(p.DataSlots[StarterMessageSlot], "capped") {
		return SpawnResult{Output: "capped answer", FinalText: "capped answer", Structured: map[string]any{"x": "y"},
			RunID: "run_capped", Status: "completed", Capped: true}, errCapped
	}
	return SpawnResult{Output: "whole answer", FinalText: "whole answer", RunID: "run_" + agent, Status: "completed"}, nil
}

// envelopeRow is one results-envelope entry as a consolidator reads it.
type envelopeRow struct {
	Agent  string `json:"agent"`
	RunID  string `json:"run_id"`
	Ok     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
	Status string `json:"status"`
}

func parseEnvelope(t *testing.T, s string) []envelopeRow {
	t.Helper()
	var env struct {
		Results []envelopeRow `json:"results"`
	}
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		t.Fatalf("envelope %q: %v", s, err)
	}
	return env.Results
}

func parallelState(wait string) teamgraph.State {
	return teamgraph.State{ID: "fan", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerParallel, Agents: []string{"capped", "whole"}, Consolidator: "judge", Wait: wait,
	}}
}

// A parallel member that stopped at its iteration limit is a failed member:
// under wait:all the state fails naming the limit, and under wait:any its row
// in the envelope is not ok, with status max_iterations, the error, and its
// last answer kept for the consolidator. Its sibling's row is unchanged.
func TestParallel_ACappedMemberIsAFailedRowThatKeepsItsAnswer(t *testing.T) {
	if _, err := NewAgentRunner(cappedOrWhole).RunHandler(context.Background(), parallelState(""), &Task{Input: "go"}); err == nil ||
		!strings.Contains(err.Error(), "1 of 2 agents succeeded") || !strings.Contains(err.Error(), "stopped at its iteration limit of 2") {
		t.Fatalf("wait:all with a capped member = %v, want the state failed naming the limit", err)
	}

	var mu sync.Mutex
	envelope := ""
	// The finished member waits for the capped one, so its success cannot
	// cancel the capped member before it runs.
	cappedDone := make(chan struct{})
	spawn := func(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error) {
		switch agent {
		case "judge":
			mu.Lock()
			envelope = p.DataSlots[ThreadedOutputSlot]
			mu.Unlock()
		case "capped":
			defer close(cappedDone)
		case "whole":
			<-cappedDone
		}
		return cappedOrWhole(ctx, agent, p, defID)
	}
	if _, err := NewAgentRunner(spawn).RunHandler(context.Background(), parallelState(teamgraph.WaitAny), &Task{Input: "go"}); err != nil {
		t.Fatalf("wait:any: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	rows := parseEnvelope(t, envelope)
	if len(rows) != 2 {
		t.Fatalf("envelope = %s, want two rows", envelope)
	}
	c, w := rows[0], rows[1]
	if c.Ok || c.Status != MemberMaxIterations || c.Error != errCapped.Error() || c.Output != "capped answer" || c.RunID != "run_capped" {
		t.Errorf("capped row = %+v, want not ok, status max_iterations, the error and its answer", c)
	}
	if !w.Ok || w.Status != "" || w.Error != "" || w.Output != "whole answer" {
		t.Errorf("finished row = %+v, want an ordinary ok row", w)
	}
}

// A failed member's row is unchanged: no answer, no status, even when its
// spawner returned one with the error.
func TestParallel_AFailedMemberRowCarriesNoAnswer(t *testing.T) {
	var mu sync.Mutex
	envelope := ""
	failedDone := make(chan struct{})
	spawn := func(_ context.Context, agent string, p Prompt, _ string) (SpawnResult, error) {
		switch agent {
		case "judge":
			mu.Lock()
			envelope = p.DataSlots[ThreadedOutputSlot]
			mu.Unlock()
		case "capped":
			defer close(failedDone)
			return SpawnResult{Output: "partial", RunID: "run_failed", Status: "failed"}, errors.New("model refused")
		case "whole":
			<-failedDone
		}
		return SpawnResult{Output: "whole answer", RunID: "run_" + agent, Status: "completed"}, nil
	}
	if _, err := NewAgentRunner(spawn).RunHandler(context.Background(), parallelState(teamgraph.WaitAny), &Task{Input: "go"}); err != nil {
		t.Fatalf("wait:any: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if f := parseEnvelope(t, envelope)[0]; f.Ok || f.Output != "" || f.Status != "" || f.Error != "model refused" {
		t.Errorf("failed row = %+v, want not ok with the error only", f)
	}
}

// A Starter run that stopped at its iteration limit publishes an error sink
// message — not ok, the error naming the limit — that still carries its last
// answer and structured result; its envelope row is not ok with status
// max_iterations and the answer kept. A run that finished publishes as before.
func TestStarter_ACappedRunPublishesAnErrorWithItsAnswer(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{
		{ID: "m1", Payload: json.RawMessage(`{"who":"capped"}`)},
		{ID: "m2", Payload: json.RawMessage(`{"who":"whole"}`)},
	}}
	st := starterState()
	st.Handler.Fanout.Wait = teamgraph.WaitAny
	// The run that finishes waits for the capped one, as above.
	cappedDone := make(chan struct{})
	spawn := func(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error) {
		if strings.Contains(p.DataSlots[StarterMessageSlot], "capped") {
			defer close(cappedDone)
		} else {
			<-cappedDone
		}
		return cappedOrWhole(ctx, agent, p, defID)
	}
	out, err := starterRunner(ch, spawn).RunHandler(context.Background(), st, &Task{Input: "go"})
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	sinks := ch.sinks(t)
	if len(sinks) != 2 {
		t.Fatalf("published %d sink messages, want 2", len(sinks))
	}
	for _, m := range sinks {
		switch m.RunID {
		case "run_capped":
			if m.Status != SinkError || m.Error != errCapped.Error() || m.Output != "capped answer" || m.Structured["x"] != "y" {
				t.Errorf("capped sink message = %+v, want an error naming the limit with its answer and structured", m)
			}
		default:
			if m.Status != SinkOK || m.Error != "" || m.Output != "whole answer" {
				t.Errorf("finished sink message = %+v, want an ordinary ok message", m)
			}
		}
	}
	for _, r := range parseEnvelope(t, out.Output) {
		switch r.RunID {
		case "run_capped":
			if r.Ok || r.Status != MemberMaxIterations || r.Error != errCapped.Error() || r.Output != "capped answer" {
				t.Errorf("capped envelope row = %+v, want not ok, status max_iterations, its answer", r)
			}
		default:
			if !r.Ok || r.Status != "" || r.Output != "whole answer" {
				t.Errorf("finished envelope row = %+v, want an ordinary ok row", r)
			}
		}
	}
}
