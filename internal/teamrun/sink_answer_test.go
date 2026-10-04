package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// memberSpawn answers the way the server's member runner does: Output is the
// answer as the member wrote it, with the same text and its structured form
// beside it.
func memberSpawn(answer string, structured map[string]any) SpawnFunc {
	return func(_ context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
		return SpawnResult{
			Output:     answer,
			FinalText:  answer,
			Structured: structured,
			RunID:      "run_" + agent,
			Status:     "completed",
		}, nil
	}
}

// A sink message is read by a program, not a model: its output is the
// member's answer as written, with no header for a JSONPath bind to trip on.
func TestStarter_SinkOutputIsTheMembersBareAnswer(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)}}}
	r := starterRunner(ch, memberSpawn("abc", nil))

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	sinks := ch.sinks(t)
	if len(sinks) != 1 {
		t.Fatalf("published %d sink messages, want 1", len(sinks))
	}
	if sinks[0].Output != "abc" {
		t.Errorf("sink output = %q, want the bare answer %q", sinks[0].Output, "abc")
	}
	if sinks[0].Structured != nil {
		t.Errorf("sink structured = %v, want none for a member without output_format", sinks[0].Structured)
	}
	if raw := string(ch.published[0].Payload); containsKey(t, raw, "structured") {
		t.Errorf("payload %s carries a structured key with nothing in it", raw)
	}
}

// A member that had an output_format publishes its parsed answer too.
func TestStarter_SinkCarriesTheMembersStructuredResult(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)}}}
	r := starterRunner(ch, memberSpawn(`{"verdict":"ok"}`, map[string]any{"verdict": "ok"}))

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	sinks := ch.sinks(t)
	if len(sinks) != 1 || sinks[0].Structured["verdict"] != "ok" {
		t.Fatalf("sinks = %+v, want structured.verdict = ok", sinks)
	}
	if sinks[0].Output != `{"verdict":"ok"}` {
		t.Errorf("sink output = %q, want the bare answer", sinks[0].Output)
	}
}

// The Starter's own output — the results envelope a consolidator or the next
// state reads — carries each member's answer as written too: an entry's
// `agent` and `run_id` say whose it is.
func TestStarter_ResultsEnvelopeCarriesTheBareAnswer(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)}}}
	r := starterRunner(ch, memberSpawn("abc", map[string]any{"x": "y"}))

	out, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go"})
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	want := `{"results":[{"index":0,"agent":"reviewer","run_id":"run_reviewer","ok":true,"output":"abc"}]}`
	if out.Output != want {
		t.Errorf("envelope =\n  %s\nwant\n  %s", out.Output, want)
	}
}

// An agent state hands the next state the member's answer and nothing else.
func TestAgentState_ThreadsTheBareAnswerOnward(t *testing.T) {
	d := teamgraph.Definition{
		Entry: "first",
		States: []teamgraph.State{
			{ID: "first", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "writer"}},
			{ID: "second", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "editor"}},
			{ID: "done", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}},
		},
		Transitions: []teamgraph.Transition{
			{From: "first", To: "second", On: teamgraph.OnSuccess},
			{From: "second", To: "done", On: teamgraph.OnSuccess},
		},
	}
	var mu sync.Mutex
	var editorInput string
	answer := memberSpawn("abc", map[string]any{"x": "y"})
	spawn := func(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error) {
		if agent == "editor" {
			mu.Lock()
			editorInput = p.DataSlots[ThreadedOutputSlot]
			mu.Unlock()
		}
		return answer(ctx, agent, p, defID)
	}
	if _, err := Walk(context.Background(), d, &Task{Input: "go"}, NewAgentRunner(spawn)); err != nil {
		t.Fatalf("walk: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if editorInput != "abc" {
		t.Errorf("editor input = %q, want the writer's answer %q", editorInput, "abc")
	}
}

// Failure messages are unchanged: a failed member publishes no answer and no
// structured result, even when its spawner returned them with the error.
func TestStarter_ErrorSinkMessageCarriesNoAnswer(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)}}}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (SpawnResult, error) {
		return SpawnResult{Output: "partial", FinalText: "partial", Structured: map[string]any{"x": "y"}, RunID: "r1"},
			errors.New("model refused")
	})
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{}); err == nil {
		t.Fatal("a failed wave must fail the state")
	}
	sinks := ch.sinks(t)
	if len(sinks) != 1 || sinks[0].Status != SinkError || sinks[0].Output != "" || sinks[0].Structured != nil {
		t.Fatalf("sinks = %+v, want one error message with no output and no structured", sinks)
	}
	raw := string(ch.published[0].Payload)
	if containsKey(t, raw, "output") || containsKey(t, raw, "structured") {
		t.Errorf("error payload %s carries an answer", raw)
	}
}

// A rejected member's message carries the answer the reviewer turned down the
// way a successful one does — bare, with its structured form — so the binds
// that read an ok message read a rejected one too. Status and error still say
// it was rejected.
func TestStarter_RejectedSinkMessageCarriesTheBareAnswerAndStructured(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)}}}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (SpawnResult, error) {
		return SpawnResult{Output: `{"x":"y"}`, FinalText: `{"x":"y"}`,
			Structured: map[string]any{"x": "y"}, RunID: "r1", Status: MemberRejected}, nil
	})
	// A rejected member does not count toward wait=all, so the state fails;
	// its message is published either way.
	_, _ = r.RunHandler(context.Background(), starterState(), &Task{})
	sinks := ch.sinks(t)
	if len(sinks) != 1 || sinks[0].Status != SinkRejected || sinks[0].Error != "rejected by the reviewer" {
		t.Fatalf("sinks = %+v, want one rejected message", sinks)
	}
	if sinks[0].Output != `{"x":"y"}` {
		t.Errorf("rejected output = %q, want the bare answer", sinks[0].Output)
	}
	if sinks[0].Structured["x"] != "y" {
		t.Errorf("rejected structured = %v, want x=y", sinks[0].Structured)
	}
}

// End to end: a downstream Starter reading the first one's sink binds the
// answer with `$.output` and a field of the structured result with
// `$.structured.<field>` — neither reachable while the answer sat behind a
// header and the structured result was not published.
func TestWalk_DownstreamStarterBindsTheAnswerAndItsStructuredField(t *testing.T) {
	d := teamgraph.Definition{
		Entry: "first",
		States: []teamgraph.State{
			{ID: "first", Handler: teamgraph.Handler{
				Kind:   teamgraph.HandlerStarter,
				Source: &teamgraph.StarterSource{Channel: "in"},
				Fanout: &teamgraph.StarterFanout{Agent: "writer", Per: teamgraph.FanoutPerMessage, Max: 1},
				Prompt: &teamgraph.StarterPrompt{Input: StarterMessageSlot},
				Sink:   &teamgraph.StarterSink{Channel: "mid"},
			}},
			{ID: "second", Handler: teamgraph.Handler{
				Kind:   teamgraph.HandlerStarter,
				Source: &teamgraph.StarterSource{Channel: "mid"},
				Fanout: &teamgraph.StarterFanout{Agent: "reader", Per: teamgraph.FanoutPerMessage, Max: 1},
				Binds:  map[string]string{"id": "$.output", "x": "$.structured.x"},
				Prompt: &teamgraph.StarterPrompt{Input: "Read ${var.id} ${var.x}."},
			}},
			{ID: "done", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}},
		},
		Transitions: []teamgraph.Transition{
			{From: "first", To: "second", On: teamgraph.OnSuccess},
			{From: "second", To: "done", On: teamgraph.OnSuccess},
		},
	}
	var mu sync.Mutex
	var got Prompt
	writer := memberSpawn("abc", map[string]any{"x": "field"})
	spawn := func(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error) {
		if agent == "reader" {
			mu.Lock()
			got = p
			mu.Unlock()
		}
		return writer(ctx, agent, p, defID)
	}
	ch := &loopChannels{queues: map[string][]ChannelMessage{"in": {{ID: "m1", Payload: json.RawMessage(`{"go":1}`)}}}}
	if _, err := Walk(context.Background(), d, &Task{Input: "go"}, NewAgentRunner(spawn, WithChannels(ch))); err != nil {
		t.Fatalf("walk: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Values["var.id"] != "abc" {
		t.Errorf("var.id = %q, want the bare answer %q", got.Values["var.id"], "abc")
	}
	if got.Values["var.x"] != "field" {
		t.Errorf("var.x = %q, want the structured field %q", got.Values["var.x"], "field")
	}
}

// containsKey reports whether a JSON object has a top-level key.
func containsKey(t *testing.T, raw, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("payload %s is not an object: %v", raw, err)
	}
	_, ok := m[key]
	return ok
}
