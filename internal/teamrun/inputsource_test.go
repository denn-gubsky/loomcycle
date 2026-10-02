package teamrun

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

func inputStarterState(per string, max int) teamgraph.State {
	return teamgraph.State{ID: "research", Handler: teamgraph.Handler{
		Kind:   teamgraph.HandlerStarter,
		Source: &teamgraph.StarterSource{Kind: teamgraph.SourceInput},
		Fanout: &teamgraph.StarterFanout{Agent: "researcher", Per: per, Max: max},
		Sink:   &teamgraph.StarterSink{Channel: "research-out"},
		Prompt: &teamgraph.StarterPrompt{Input: "Research ${var.chunk_id}. Item: " + StarterMessageSlot},
	}}
}

// slotJSON decodes one prompt's data slot, failing the test when it is not JSON.
func slotJSON(t *testing.T, p Prompt, slot string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(p.DataSlots[slot]), &v); err != nil {
		t.Fatalf("data slot %s is not JSON: %q", slot, p.DataSlots[slot])
	}
	return v
}

// A form submission is one object: exactly one run, its binds lifted into
// ${var.*}, the object itself in the data slot, one sink message, nothing read
// from or acked on a channel.
func TestInputStarter_ObjectInputDispatchesOneRunWithBinds(t *testing.T) {
	ch := &fakeChannels{}
	rec := &docSpawn{}
	r := starterRunner(ch, rec.spawn())
	st := inputStarterState(teamgraph.FanoutPerMessage, 1)
	st.Handler.Binds = map[string]string{"document_id": "$.document_id", "chunk_id": "$.chunk_id"}

	task := &Task{Input: `{"document_id":"d","chunk_id":"c"}`, WalkID: "wlk_t"}
	out, err := r.RunHandler(context.Background(), st, task)
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	if !strings.HasPrefix(out.Output, `{"results":`) {
		t.Errorf("output = %q, want the results envelope", out.Output)
	}
	if len(rec.prompts) != 1 {
		t.Fatalf("dispatched %d runs, want exactly one", len(rec.prompts))
	}
	p := rec.prompts[0]
	if got := p.Values["var.chunk_id"]; got != "c" {
		t.Errorf("${var.chunk_id} = %q, want c", got)
	}
	if got := p.Values["var.document_id"]; got != "d" {
		t.Errorf("${var.document_id} = %q, want d", got)
	}
	want := map[string]any{"document_id": "d", "chunk_id": "c"}
	if got := slotJSON(t, p, StarterMessageSlot); !reflect.DeepEqual(got, want) {
		t.Errorf("{{starter.message}} = %v, want the input object %v", got, want)
	}
	if sinks := ch.sinks(t); len(sinks) != 1 || sinks[0].Status != SinkOK {
		t.Errorf("sink messages = %+v, want one ok", sinks)
	}
	if len(ch.acked) != 0 {
		t.Errorf("acked %v — the input has no cursor", ch.acked)
	}
}

// An array is one item per element, in order.
func TestInputStarter_ArrayDispatchesOneRunPerElement(t *testing.T) {
	ch := &fakeChannels{}
	rec := &docSpawn{}
	r := starterRunner(ch, rec.spawn())

	_, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 2),
		&Task{Input: `[{"chunk_id":"a"},{"chunk_id":"b"}]`})
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	if len(rec.prompts) != 2 {
		t.Fatalf("dispatched %d runs, want one per element (2)", len(rec.prompts))
	}
	seen := map[string]bool{}
	for _, p := range rec.prompts {
		m, _ := slotJSON(t, p, StarterMessageSlot).(map[string]any)
		seen[m["chunk_id"].(string)] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("runs received %v, want both elements", seen)
	}
	if sinks := ch.sinks(t); len(sinks) != 2 {
		t.Errorf("published %d sink messages, want 2", len(sinks))
	}
}

// More items than the ceiling FAILS rather than dispatching the first `max`:
// with no cursor, the items past it would be dropped silently.
func TestInputStarter_MoreItemsThanMaxFailsNamingBoth(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())

	_, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 2),
		&Task{Input: `[1,2,3]`})
	if err == nil {
		t.Fatal("a 3-item input dispatched under max=2")
	}
	if !strings.Contains(err.Error(), "3 items") || !strings.Contains(err.Error(), "fanout.max=2") {
		t.Errorf("err = %q, want it to name both 3 and 2", err)
	}
	if len(rec.prompts) != 0 {
		t.Errorf("spawned %d runs before refusing", len(rec.prompts))
	}
}

func TestInputStarter_EmptyArrayFailsTheWalk(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())

	_, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 2), &Task{Input: ` [ ] `})
	if err == nil || !strings.Contains(err.Error(), "empty array") {
		t.Fatalf("err = %v, want a walk error for an empty array", err)
	}
	if len(rec.prompts) != 0 {
		t.Errorf("spawned %d runs for an empty array", len(rec.prompts))
	}
}

// Text that is not JSON — including no input at all — is one item
// {"text": <input>}, so a member always receives a JSON value.
func TestInputStarter_TextInputIsOneTextItem(t *testing.T) {
	for _, in := range []string{"compare these <parts> & prices", ""} {
		rec := &docSpawn{}
		r := starterRunner(&fakeChannels{}, rec.spawn())
		if _, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 1), &Task{Input: in}); err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		if len(rec.prompts) != 1 {
			t.Fatalf("input %q: dispatched %d runs, want one", in, len(rec.prompts))
		}
		want := map[string]any{"text": in}
		if got := slotJSON(t, rec.prompts[0], StarterMessageSlot); !reflect.DeepEqual(got, want) {
			t.Errorf("input %q: item = %v, want %v", in, got, want)
		}
		if strings.Contains(rec.prompts[0].DataSlots[StarterMessageSlot], `\u003c`) {
			t.Errorf("input %q: item was HTML-escaped: %s", in, rec.prompts[0].DataSlots[StarterMessageSlot])
		}
	}
}

// A JSON scalar is one item, the value itself — not wrapped as text.
func TestInputStarter_ScalarInputIsTheValue(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	if _, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 1), &Task{Input: `"just a string"`}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if got := slotJSON(t, rec.prompts[0], StarterMessageSlot); got != "just a string" {
		t.Errorf("item = %v, want the JSON string itself", got)
	}
}

// per=once is one run holding every item as a JSON array.
func TestInputStarter_PerOnceHoldsEveryItem(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	st := inputStarterState(teamgraph.FanoutPerOnce, 0)
	st.Handler.Prompt.Input = "All: " + StarterMessagesSlot

	if _, err := r.RunHandler(context.Background(), st, &Task{Input: `[{"n":1},{"n":2},{"n":3}]`}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if len(rec.prompts) != 1 {
		t.Fatalf("dispatched %d runs, want one", len(rec.prompts))
	}
	all, _ := slotJSON(t, rec.prompts[0], StarterMessagesSlot).([]any)
	if len(all) != 3 {
		t.Errorf("messages slot = %q, want all three items", rec.prompts[0].DataSlots[StarterMessagesSlot])
	}
}

// The walk's input reaches the entry Starter through Walk itself, not only
// through a hand-built task.
func TestInputStarter_WalkFeedsTheEntryTheWalkInput(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	st := inputStarterState(teamgraph.FanoutPerMessage, 1)
	st.Handler.Binds = map[string]string{"chunk_id": "$.chunk_id"}
	d := teamgraph.Definition{
		Entry:       "research",
		States:      []teamgraph.State{st, {ID: "done", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}}},
		Transitions: []teamgraph.Transition{{From: "research", To: "done", On: teamgraph.OnSuccess}},
	}
	if err := teamgraph.Validate(d); err != nil {
		t.Fatalf("definition: %v", err)
	}
	if _, err := Walk(context.Background(), d, &Task{Input: `{"chunk_id":"c"}`}, r); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(rec.prompts) != 1 || rec.prompts[0].Values["var.chunk_id"] != "c" {
		t.Errorf("prompts = %+v, want one run with ${var.chunk_id}=c", rec.prompts)
	}
}

// A sink with no channel executor is refused before anything runs; with no
// sink, none is needed.
func TestInputStarter_SinkWithoutChannelsIsRefused(t *testing.T) {
	rec := &docSpawn{}
	r := &agentRunner{spawn: rec.spawn(), logf: func(string, ...any) {}}
	if _, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 1), &Task{Input: `{}`}); err == nil ||
		!strings.Contains(err.Error(), "no channel executor is wired") {
		t.Errorf("sink without channels: err = %v", err)
	}
	if len(rec.prompts) != 0 {
		t.Errorf("spawned %d runs with no way to publish their results", len(rec.prompts))
	}
	st := inputStarterState(teamgraph.FanoutPerMessage, 1)
	st.Handler.Sink = nil
	if _, err := r.RunHandler(context.Background(), st, &Task{Input: `{}`}); err != nil {
		t.Errorf("sinkless input starter without channels: %v", err)
	}
}

// The deployment's wave ceiling applies to an input wave as to any other.
func TestInputStarter_MaxAboveTheDeploymentCeilingIsRefused(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.maxWave = 2
	_, err := r.RunHandler(context.Background(), inputStarterState(teamgraph.FanoutPerMessage, 5), &Task{Input: `{}`})
	if err == nil || !strings.Contains(err.Error(), "exceeds this deployment's ceiling") {
		t.Fatalf("err = %v, want the deployment ceiling refusal", err)
	}
}

// A cap reroute is the one target chosen at run time, so validation cannot
// refuse it: the walk does. Rerouting into the input-sourced entry would
// dispatch the capped state's input as if it were the walk's input.
func TestWalk_CapRerouteIntoAnInputStarterRefused(t *testing.T) {
	d := mustParse(t, `{
  "entry":"research",
  "max_iterations":1,
  "states":[
    {"state":"research","handler":{"kind":"starter","source":{"kind":"input"},
      "fanout":{"agent":"researcher","per":"message","max":1}}},
    {"state":"review","handler":{"kind":"agent","agent":"reviewer"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"research","to":"review","on":"success"},
    {"from":"review","to":"review","on":"pushback:redo"},
    {"from":"review","to":"done","on":"success"}
  ]}`)
	r := &fakeRunner{outcomes: map[string]Outcome{"review": {Output: "again", Edge: "pushback:redo"}}}
	asked := 0
	_, err := Walk(context.Background(), d, &Task{Input: `{"chunk_id":"c"}`}, r,
		OnCap(func(_ context.Context, _ *ErrIterationCap) (CapDecision, error) {
			// Reroute once, then abort: without the refusal the walk would
			// otherwise loop entry → review → cap forever.
			if asked++; asked > 1 {
				return CapDecision{Action: CapAbort}, nil
			}
			return CapDecision{Action: CapReroute, Reroute: "research"}, nil
		}))
	if err == nil || !strings.Contains(err.Error(), `reroute to "research" refused`) ||
		!strings.Contains(err.Error(), "route a retry to a later state") {
		t.Fatalf("err = %v, want the reroute into the input starter refused", err)
	}
	if got := strings.Join(r.calls, ","); got != "research,review" {
		t.Errorf("ran %s, want research once then review once (the entry not re-run)", got)
	}
}
