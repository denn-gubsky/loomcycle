package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// RFC DI: a member run is addressable. Every sink message names the run that
// produced it — including a member that FAILED, the one most worth opening.
func TestStarter_SinkMessagesNameTheMemberRun(t *testing.T) {
	ch := threeMessages()
	r := starterRunner(ch, func(_ context.Context, _ string, p Prompt, _ string) (SpawnResult, error) {
		msg := p.DataSlots[StarterMessageSlot]
		switch {
		case strings.Contains(msg, `"pr":2`):
			return SpawnResult{RunID: "r_pr2"}, errors.New("reviewer crashed")
		case strings.Contains(msg, `"pr":1`):
			return SpawnResult{Output: "ok 1", RunID: "r_pr1"}, nil
		default:
			return SpawnResult{Output: "ok 3", RunID: "r_pr3"}, nil
		}
	})
	// wait=all fails the state when a member fails — and still publishes every
	// member's message, which is the guarantee this test reads through.
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk"}); err == nil {
		t.Fatal("a failed member under wait=all should fail the state")
	}
	byRun := map[string]SinkMessage{}
	for _, m := range ch.sinks(t) {
		byRun[m.RunID] = m
	}
	if len(byRun) != 3 {
		t.Fatalf("sink messages by run id = %v, want one per member run", byRun)
	}
	if m := byRun["r_pr2"]; m.Status != SinkError {
		t.Errorf("the failed member's message = %+v, want status error AND its run id", m)
	}
	if m := byRun["r_pr1"]; m.Status != SinkOK || m.Output != "ok 1" {
		t.Errorf("member 1's message = %+v", m)
	}
}

// A parallel state's results envelope — what its consolidator reads — names
// each member's run, so the judgement can be traced back to the run it judged.
func TestParallel_ResultsEnvelopeNamesEachMemberRun(t *testing.T) {
	d := mustParse(t, parallelJSON)
	var mu sync.Mutex
	var envelope string
	spawn := func(_ context.Context, agent string, p Prompt, _ string) (SpawnResult, error) {
		switch agent {
		case "a":
			return SpawnResult{Output: "A-out", RunID: "r_a"}, nil
		case "b":
			return SpawnResult{Output: "B-out", RunID: "r_b"}, nil
		}
		mu.Lock()
		envelope = p.DataSlots[ThreadedOutputSlot]
		mu.Unlock()
		return SpawnResult{Output: "merged\nsignal: success", RunID: "r_c"}, nil
	}
	if _, err := Walk(context.Background(), d, &Task{Input: "task"}, NewAgentRunner(spawn)); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var env struct {
		Results []agentResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(envelope), &env); err != nil {
		t.Fatalf("envelope %q: %v", envelope, err)
	}
	runs := map[string]string{}
	for _, r := range env.Results {
		runs[r.Agent] = r.RunID
	}
	if runs["a"] != "r_a" || runs["b"] != "r_b" {
		t.Errorf("envelope run ids = %v, want a→r_a, b→r_b", runs)
	}
}
