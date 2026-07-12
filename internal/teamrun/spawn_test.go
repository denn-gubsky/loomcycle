package teamrun

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// recordingSpawn returns a SpawnFunc that records every (agent, input) call
// under mu, then delegates to fn. calls/inputs are safe to read after Walk
// returns (Walk joins all goroutines before returning).
func recordingSpawn(mu *sync.Mutex, calls *[]string, inputs map[string]string, fn func(ctx context.Context, agent, input string) (string, error)) SpawnFunc {
	return func(ctx context.Context, agent, input, defID string) (string, error) {
		mu.Lock()
		*calls = append(*calls, agent)
		if inputs != nil {
			inputs[agent] = input
		}
		mu.Unlock()
		return fn(ctx, agent, input)
	}
}

func TestAgentRunner_WalksLinearTeamViaSpawn(t *testing.T) {
	d := mustParse(t, linearJSON)

	// Fake spawn: echoes which agent ran + the input, so we can assert the
	// output threads state to state.
	var spawned []string
	spawn := func(_ context.Context, agent, input, defID string) (string, error) {
		spawned = append(spawned, agent)
		return fmt.Sprintf("%s(%s)", agent, input), nil
	}

	task := &Task{Input: "go"}
	trace, err := Walk(context.Background(), d, task, NewAgentRunner(spawn))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(spawned) != 2 || spawned[0] != "agent-a" || spawned[1] != "agent-b" {
		t.Errorf("spawned %v, want [agent-a agent-b]", spawned)
	}
	// b's input is a's output: agent-a(go) → agent-b(agent-a(go)).
	if task.Input != "agent-b(agent-a(go))" {
		t.Errorf("final input = %q, want agent-b(agent-a(go))", task.Input)
	}
	if trace[1].Edge != "success" {
		t.Errorf("linear agent handler should advance on success, got %q", trace[1].Edge)
	}
}

func TestAgentRunner_SpawnErrorStopsWalk(t *testing.T) {
	d := mustParse(t, linearJSON)
	spawn := func(_ context.Context, agent, input, defID string) (string, error) {
		if agent == "agent-b" {
			return "", fmt.Errorf("boom")
		}
		return "ok", nil
	}
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	if err == nil || !contains(err.Error(), "boom") {
		t.Fatalf("spawn error should abort the walk, got %v", err)
	}
}

const parallelJSON = `{
  "entry":"fan",
  "states":[
    {"state":"fan","handler":{"kind":"parallel","agents":["a","b"],"consolidator":"c","wait":"all"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[{"from":"fan","to":"done","on":"success"}]}`

func TestAgentRunner_ParallelWaitAllRunsEveryAgentThenConsolidates(t *testing.T) {
	d := mustParse(t, parallelJSON)

	var mu sync.Mutex
	var calls []string
	inputs := map[string]string{}
	spawn := recordingSpawn(&mu, &calls, inputs, func(_ context.Context, agent, input string) (string, error) {
		switch agent {
		case "a":
			return "A-out", nil
		case "b":
			return "B-out", nil
		case "c":
			return "merged\nsignal: success", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	})

	task := &Task{Input: "task"}
	trace, err := Walk(context.Background(), d, task, NewAgentRunner(spawn))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Both fan-out agents ran, plus the consolidator.
	got := map[string]bool{}
	for _, c := range calls {
		got[c] = true
	}
	if !got["a"] || !got["b"] || !got["c"] {
		t.Errorf("ran %v, want a,b,c all present", calls)
	}
	// The consolidator reads the {results:[…]} envelope carrying BOTH outputs.
	env := inputs["c"]
	if !contains(env, `"results"`) || !contains(env, "A-out") || !contains(env, "B-out") {
		t.Errorf("consolidator input %q missing results envelope with both outputs", env)
	}
	// Signal line is stripped from the threaded output; edge is success → done.
	if len(trace) != 1 || trace[0].Edge != "success" || trace[0].Next != "done" {
		t.Fatalf("trace = %+v, want one step: fan --success--> done", trace)
	}
	if trace[0].Output != "merged" {
		t.Errorf("consolidator output = %q, want %q (signal line stripped)", trace[0].Output, "merged")
	}
	if task.State != "done" {
		t.Errorf("final state = %q, want done", task.State)
	}
}

func TestAgentRunner_ParallelWaitAnyReturnsOnFirstAndCancelsOthers(t *testing.T) {
	d := mustParse(t, `{
	  "entry":"fan",
	  "states":[
	    {"state":"fan","handler":{"kind":"parallel","agents":["fast","slow"],"consolidator":"c","wait":"any"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"fan","to":"done","on":"success"}]}`)

	var mu sync.Mutex
	var calls []string
	inputs := map[string]string{}
	spawn := recordingSpawn(&mu, &calls, inputs, func(ctx context.Context, agent, input string) (string, error) {
		switch agent {
		case "fast":
			return "FAST", nil
		case "slow":
			// Blocks until the first success cancels the run — no sleep, so the
			// test hangs (times out) if wait:any fails to cancel the sibling.
			<-ctx.Done()
			return "", ctx.Err()
		case "c":
			return "signal: success", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	})

	task := &Task{Input: "task"}
	if _, err := Walk(context.Background(), d, task, NewAgentRunner(spawn)); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if task.State != "done" {
		t.Errorf("final state = %q, want done", task.State)
	}
	// The envelope proves fast succeeded and slow was cancelled (not awaited).
	env := inputs["c"]
	if !contains(env, `"agent":"fast","ok":true`) {
		t.Errorf("envelope %q should carry fast as ok:true", env)
	}
	if !contains(env, `"agent":"slow","ok":false`) {
		t.Errorf("envelope %q should carry slow as ok:false (cancelled)", env)
	}
}

func TestAgentRunner_ParallelWaitAtLeastCancelsOnceThresholdMet(t *testing.T) {
	// at_least:2 of 3: two fast successes meet the threshold, so the third
	// (blocking) agent is cancelled rather than awaited.
	d := mustParse(t, `{
	  "entry":"fan",
	  "states":[
	    {"state":"fan","handler":{"kind":"parallel","agents":["a","b","slow"],"consolidator":"c","wait":"at_least:2"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"fan","to":"done","on":"success"}]}`)

	var mu sync.Mutex
	var calls []string
	inputs := map[string]string{}
	spawn := recordingSpawn(&mu, &calls, inputs, func(ctx context.Context, agent, input string) (string, error) {
		switch agent {
		case "a", "b":
			return agent + "-out", nil
		case "slow":
			<-ctx.Done()
			return "", ctx.Err()
		case "c":
			return "signal: success", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	})

	task := &Task{Input: "task"}
	if _, err := Walk(context.Background(), d, task, NewAgentRunner(spawn)); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !contains(inputs["c"], `"agent":"slow","ok":false`) {
		t.Errorf("envelope %q should carry slow as ok:false (cancelled after threshold)", inputs["c"])
	}
}

func TestAgentRunner_ParallelWaitAllAgentErrorAborts(t *testing.T) {
	d := mustParse(t, parallelJSON)
	consolidatorCalled := false
	spawn := func(_ context.Context, agent, input, defID string) (string, error) {
		switch agent {
		case "a":
			return "A-out", nil
		case "b":
			return "", fmt.Errorf("boom")
		case "c":
			consolidatorCalled = true
			return "signal: success", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	}
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	if err == nil || !contains(err.Error(), "parallel handler") || !contains(err.Error(), "boom") {
		t.Fatalf("wait:all agent error should abort with a clear error naming the failure, got %v", err)
	}
	if consolidatorCalled {
		t.Errorf("consolidator must not run when a wait:all fan-out fails")
	}
}

// pushbackJSON: impl runs coder, then judge (consolidator) selects the edge —
// success advances to done, pushback:redo loops back to impl for rework.
const pushbackJSON = `{
  "entry":"impl",
  "max_iterations":3,
  "states":[
    {"state":"impl","handler":{"kind":"agent","agent":"coder","consolidator":"judge"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"impl","to":"done","on":"success"},
    {"from":"impl","to":"impl","on":"pushback:redo"}
  ]}`

func TestAgentRunner_ConsolidatorSelectsPushbackEdgeThenSucceeds(t *testing.T) {
	d := mustParse(t, pushbackJSON)

	var mu sync.Mutex
	var calls []string
	judgeCalls := 0
	spawn := recordingSpawn(&mu, &calls, nil, func(_ context.Context, agent, input string) (string, error) {
		switch agent {
		case "coder":
			return "code", nil
		case "judge":
			judgeCalls++
			if judgeCalls == 1 {
				return "needs a test\nsignal: pushback:redo", nil
			}
			return "lgtm\nsignal: success", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	})

	task := &Task{Input: "build X"}
	trace, err := Walk(context.Background(), d, task, NewAgentRunner(spawn))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(trace) != 2 {
		t.Fatalf("trace len = %d, want 2 (impl pushed back once then succeeded)", len(trace))
	}
	if trace[0].Edge != "pushback:redo" || trace[0].Next != "impl" {
		t.Errorf("step 0 = %+v, want pushback:redo → impl", trace[0])
	}
	if trace[1].Edge != "success" || trace[1].Next != "done" {
		t.Errorf("step 1 = %+v, want success → done", trace[1])
	}
	// The pushback reason (feedback prose) threads to the re-run, signal stripped.
	if trace[0].Output != "needs a test" {
		t.Errorf("pushback output = %q, want %q (signal line stripped)", trace[0].Output, "needs a test")
	}
	if task.State != "done" {
		t.Errorf("final state = %q, want done", task.State)
	}
}

func TestAgentRunner_PushbackCycleHitsIterationCap(t *testing.T) {
	// A judge that ALWAYS pushes back never converges; the per-state cap must
	// bound the loop rather than spin forever.
	d := mustParse(t, pushbackJSON) // max_iterations:3
	spawn := func(_ context.Context, agent, input, defID string) (string, error) {
		switch agent {
		case "coder":
			return "code", nil
		case "judge":
			return "still wrong\nsignal: pushback:redo", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	}
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	var capErr *ErrIterationCap
	if !errors.As(err, &capErr) {
		t.Fatalf("non-converging pushback cycle should hit the cap, got %v", err)
	}
	if capErr.State != "impl" || capErr.Max != 3 {
		t.Errorf("cap = %+v, want state=impl max=3", capErr)
	}
}
