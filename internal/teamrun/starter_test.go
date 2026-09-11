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

// fakeChannels is a ChannelIO over in-memory slices — enough to prove the
// read → dispatch → publish → ack sequence without a store.
type fakeChannels struct {
	mu        sync.Mutex
	inbox     []ChannelMessage
	published []struct {
		Channel string
		Payload json.RawMessage
	}
	acked   []string
	readErr error
	pubErr  error
}

func (f *fakeChannels) Read(_ context.Context, _ string, _, _, _ int) ([]ChannelMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, "", f.readErr
	}
	if len(f.inbox) == 0 {
		return nil, "", nil
	}
	return f.inbox, "cur_last", nil
}

func (f *fakeChannels) Ack(_ context.Context, _, cursor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, cursor)
	return nil
}

func (f *fakeChannels) Publish(_ context.Context, channel string, payload json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pubErr != nil {
		return f.pubErr
	}
	f.published = append(f.published, struct {
		Channel string
		Payload json.RawMessage
	}{channel, payload})
	return nil
}

func (f *fakeChannels) sinks(t *testing.T) []SinkMessage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SinkMessage, 0, len(f.published))
	for _, p := range f.published {
		var m SinkMessage
		if err := json.Unmarshal(p.Payload, &m); err != nil {
			t.Fatalf("sink payload is not a SinkMessage: %s", p.Payload)
		}
		out = append(out, m)
	}
	return out
}

func starterState() teamgraph.State {
	return teamgraph.State{ID: "wave", Handler: teamgraph.Handler{
		Kind:   teamgraph.HandlerStarter,
		Source: &teamgraph.StarterSource{Channel: "pr-events"},
		Fanout: &teamgraph.StarterFanout{Agent: "reviewer", Per: teamgraph.FanoutPerMessage, Max: 4},
		Sink:   &teamgraph.StarterSink{Channel: "verdicts"},
		Prompt: &teamgraph.StarterPrompt{System: "You review.", Input: "Review:\n" + StarterMessageSlot},
	}}
}

func starterRunner(ch *fakeChannels, spawn SpawnFunc) *agentRunner {
	r := &agentRunner{spawn: spawn, channels: ch, logf: func(string, ...any) {}}
	return r
}

// The vertical slice: read a message, dispatch one run with the payload in the
// data slot, publish exactly one sink message, then ack.
func TestStarter_ReadsDispatchesPublishesAcks(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":42}`)}}}
	var got Prompt
	r := starterRunner(ch, func(_ context.Context, agent string, p Prompt, _ string) (string, error) {
		got = p
		return "looks fine", nil
	})

	task := &Task{Input: "start", WalkID: "wlk_test"}
	out, err := r.RunHandler(context.Background(), starterState(), task)
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	if out.Output != "looks fine" {
		t.Errorf("output = %q, want the agent's", out.Output)
	}

	// The payload reaches the agent through the DATA SLOT, not the template.
	if got.Input != "Review:\n"+StarterMessageSlot {
		t.Errorf("input template was pre-substituted (%q) — the slot must be filled at assembly, after expansion", got.Input)
	}
	if got.DataSlots[StarterMessageSlot] != `{"pr":42}` {
		t.Errorf("data slot = %q, want the raw payload", got.DataSlots[StarterMessageSlot])
	}

	sinks := ch.sinks(t)
	if len(sinks) != 1 {
		t.Fatalf("published %d sink messages, want exactly 1", len(sinks))
	}
	if sinks[0].Status != SinkOK || sinks[0].Output != "looks fine" || sinks[0].Agent != "reviewer" {
		t.Errorf("sink = %+v", sinks[0])
	}
	if sinks[0].Wave == "" {
		t.Errorf("sink carries no wave id")
	}
	if len(ch.acked) != 1 || ch.acked[0] != "cur_last" {
		t.Errorf("acked = %v, want the read cursor once", ch.acked)
	}
}

// A failing agent still produces its sink message. A fan-in counts messages
// and cannot tell "still running" from "died", so a wave that can publish
// fewer messages than it spawned turns a downstream wait into a hang.
func TestStarter_AFailedRunStillPublishesToTheSink(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{}`)}}}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		return "", errors.New("model refused")
	})

	_, err := r.RunHandler(context.Background(), starterState(), &Task{})
	if err == nil {
		t.Fatalf("a failed wave must fail the state")
	}
	sinks := ch.sinks(t)
	if len(sinks) != 1 || sinks[0].Status != SinkError {
		t.Fatalf("sinks = %+v, want one error message", sinks)
	}
	if !strings.Contains(sinks[0].Error, "model refused") {
		t.Errorf("sink error = %q, want the agent's reason", sinks[0].Error)
	}
}

// And a PANICKING agent, which is the case the deferred publish exists for.
func TestStarter_APanickingRunStillPublishesToTheSink(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{}`)}}}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		panic("spawner exploded")
	})

	_, err := r.RunHandler(context.Background(), starterState(), &Task{})
	if err == nil {
		t.Fatalf("a panicking wave must fail the state, not crash the walk")
	}
	sinks := ch.sinks(t)
	if len(sinks) != 1 || sinks[0].Status != SinkError {
		t.Fatalf("sinks = %+v, want one error message", sinks)
	}
	if !strings.Contains(sinks[0].Error, "panicked") {
		t.Errorf("sink error = %q, want it to name the panic", sinks[0].Error)
	}
}

// A crash between the results and the ack REDELIVERS — at-least-once, which is
// what after_results buys. Pinned by asserting the ack is the last thing that
// happens: a failed wave must not advance the cursor.
func TestStarter_AFailedWaveDoesNotAck(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{}`)}}}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		return "", errors.New("boom")
	})
	_, _ = r.RunHandler(context.Background(), starterState(), &Task{})
	if len(ch.acked) != 0 {
		t.Errorf("a failed wave acked %v — the batch must redeliver", ch.acked)
	}
}

// An empty read is an ERROR, not a silent proceed: a walk that advanced on a
// wave nobody produced would hand the next state an answer from nowhere.
func TestStarter_NoMessageInTheWaitIsAWalkError(t *testing.T) {
	ch := &fakeChannels{}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		t.Fatal("spawned on an empty read")
		return "", nil
	})
	_, err := r.RunHandler(context.Background(), starterState(), &Task{})
	if err == nil || !strings.Contains(err.Error(), "no message") {
		t.Errorf("err = %v, want a no-message walk error", err)
	}
}

// binds project the source message into ${var.*} with the same JSONPath the
// webhook projector uses.
func TestStarter_BindsProjectTheSourceMessage(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pull_request":{"number":1180}}`)}}}
	var got Prompt
	r := starterRunner(ch, func(_ context.Context, _ string, p Prompt, _ string) (string, error) {
		got = p
		return "ok", nil
	})
	st := starterState()
	st.Handler.Binds = map[string]string{"pr": "$.pull_request.number"}

	task := &Task{}
	if _, err := r.RunHandler(context.Background(), st, task); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if task.Vars["pr"] != "1180" {
		t.Errorf("task.Vars[pr] = %q, want 1180", task.Vars["pr"])
	}
	if got.Values["var.pr"] != "1180" {
		t.Errorf("values[var.pr] = %q — the bind must reach the prompt", got.Values["var.pr"])
	}
}

// No executor wired is refused AT THE STATE. A starter whose source never fires
// looks identical to one whose source is empty, so silence is the wrong answer.
func TestStarter_NoChannelExecutorIsRefused(t *testing.T) {
	r := &agentRunner{spawn: func(context.Context, string, Prompt, string) (string, error) { return "", nil }}
	_, err := r.RunHandler(context.Background(), starterState(), &Task{})
	if err == nil || !strings.Contains(err.Error(), "no channel executor") {
		t.Errorf("err = %v, want a refusal naming the missing executor", err)
	}
}

// The publish-only `channel` kind states something and threads its input on.
func TestChannelKind_PublishesAndThreadsInputThrough(t *testing.T) {
	ch := &fakeChannels{}
	r := starterRunner(ch, nil)
	st := teamgraph.State{ID: "announce", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerChannel, Channel: "verdicts"}}

	out, err := r.RunHandler(context.Background(), st, &Task{Input: "the verdict"})
	if err != nil {
		t.Fatalf("channel publish: %v", err)
	}
	if out.Output != "the verdict" {
		t.Errorf("output = %q, want the threaded input unchanged", out.Output)
	}
	if len(ch.published) != 1 || ch.published[0].Channel != "verdicts" {
		t.Fatalf("published = %+v", ch.published)
	}
}
