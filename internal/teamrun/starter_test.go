package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
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
	// ALWAYS the results envelope, even for a wave of one: a Starter's output
	// shape must not depend on how many messages happened to arrive, or a
	// downstream consolidator works on Tuesday and breaks on Wednesday. It is
	// the same envelope a parallel handler produces, so one consolidator agent
	// reads either.
	if !strings.Contains(out.Output, `"output":"looks fine"`) || !strings.HasPrefix(out.Output, `{"results":`) {
		t.Errorf("output = %q, want the results envelope", out.Output)
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

// ---- P5: fan-out ----

func inbox(n int) []ChannelMessage {
	out := make([]ChannelMessage, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ChannelMessage{
			ID:      "m" + strconv.Itoa(i),
			Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
		})
	}
	return out
}

// N messages become N runs and N sink messages, each carrying its index and the
// wave's SIZE — the number a downstream fan-in counts to, taken from the
// runtime rather than from a literal the author would have to keep in sync.
func TestStarterFanout_OneRunAndOneSinkMessagePerMessage(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(4)}
	var mu sync.Mutex
	var seen []string
	r := starterRunner(ch, func(_ context.Context, _ string, p Prompt, _ string) (string, error) {
		mu.Lock()
		seen = append(seen, p.DataSlots[StarterMessageSlot])
		mu.Unlock()
		return "done", nil
	})

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{}); err != nil {
		t.Fatalf("wave: %v", err)
	}
	if len(seen) != 4 {
		t.Fatalf("spawned %d runs, want 4", len(seen))
	}
	sinks := ch.sinks(t)
	if len(sinks) != 4 {
		t.Fatalf("published %d sink messages, want 4 — the count IS the fan-in contract", len(sinks))
	}
	idx := map[int]bool{}
	for _, m := range sinks {
		if m.WaveSize != 4 {
			t.Errorf("sink wave_size = %d, want 4 (the runtime's count, not a literal)", m.WaveSize)
		}
		if m.Wave != sinks[0].Wave {
			t.Errorf("one wave produced two wave ids")
		}
		idx[m.Index] = true
	}
	if len(idx) != 4 {
		t.Errorf("indices = %v, want one per run", idx)
	}
	// Each run got ITS message, not a shared one.
	sort.Strings(seen)
	if seen[0] != `{"i":0}` || seen[3] != `{"i":3}` {
		t.Errorf("data slots = %v, want one message each", seen)
	}
}

// per=once is ONE run holding the whole batch, in the plural slot. The two
// shapes have separate slot names so a template says which it expects.
func TestStarterFanout_PerOnceIsOneRunWithTheBatch(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(3)}
	var got Prompt
	r := starterRunner(ch, func(_ context.Context, _ string, p Prompt, _ string) (string, error) {
		got = p
		return "done", nil
	})
	st := starterState()
	st.Handler.Fanout.Per = teamgraph.FanoutPerOnce
	st.Handler.Fanout.Max = 0
	st.Handler.Prompt.Input = "All:\n" + StarterMessagesSlot

	if _, err := r.RunHandler(context.Background(), st, &Task{}); err != nil {
		t.Fatalf("wave: %v", err)
	}
	if sinks := ch.sinks(t); len(sinks) != 1 || sinks[0].WaveSize != 1 {
		t.Fatalf("sinks = %+v, want exactly one", sinks)
	}
	all := got.DataSlots[StarterMessagesSlot]
	if !strings.HasPrefix(all, "[") || !strings.Contains(all, `{"i":2}`) {
		t.Errorf("messages slot = %q, want a JSON array of every message", all)
	}
	if _, singular := got.DataSlots[StarterMessageSlot]; singular {
		t.Errorf("per=once filled the SINGULAR slot too — a batch is not a message")
	}
}

// A definition asking for a wider wave than the deployment allows is REFUSED,
// not quietly given less. Silently capping is the shape of bug that costs real
// time: an operator sets a number and the runtime uses a different one.
func TestStarterFanout_MaxAboveTheDeploymentCeilingIsRefused(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(2)}
	spawned := 0
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		spawned++
		return "", nil
	})
	r.maxWave = 2
	st := starterState()
	st.Handler.Fanout.Max = 8

	_, err := r.RunHandler(context.Background(), st, &Task{})
	if err == nil || !strings.Contains(err.Error(), "exceeds this deployment's ceiling") {
		t.Fatalf("err = %v, want a ceiling refusal naming both numbers", err)
	}
	if spawned != 0 {
		t.Errorf("refused AFTER spawning %d runs — the refusal must come first", spawned)
	}
}

// The author's own ceiling bounds the wave even when more messages are waiting;
// the rest stay on the channel for the next pass, unacked.
func TestStarterFanout_MaxBoundsTheWaveAndLeavesTheRest(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(10)}
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		return "done", nil
	})
	st := starterState()
	st.Handler.Fanout.Max = 3

	if _, err := r.RunHandler(context.Background(), st, &Task{}); err != nil {
		t.Fatalf("wave: %v", err)
	}
	if sinks := ch.sinks(t); len(sinks) != 3 {
		t.Errorf("wave was %d wide, want the authored max of 3", len(sinks))
	}
}

// A wave where one run fails still publishes a sink message for EVERY run, so a
// downstream fan-in is unblocked by the failure instead of hanging on it.
func TestStarterFanout_AFailedRunStillLeavesTheCountWhole(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(3)}
	var mu sync.Mutex
	n := 0
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		mu.Lock()
		n++
		mine := n
		mu.Unlock()
		if mine == 2 {
			return "", errors.New("second one failed")
		}
		return "ok", nil
	})

	_, err := r.RunHandler(context.Background(), starterState(), &Task{})
	if err == nil {
		t.Fatalf("wait=all with a failure must fail the state")
	}
	sinks := ch.sinks(t)
	if len(sinks) != 3 {
		t.Fatalf("published %d sink messages for a 3-wide wave — the count is the contract", len(sinks))
	}
	errs := 0
	for _, m := range sinks {
		if m.Status == SinkError {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("%d error sinks, want exactly the failed run's", errs)
	}
}

// wait=at_least lets a wave succeed with failures, and the state still advances.
func TestStarterFanout_WaitAtLeastSucceedsBelowFull(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(3)}
	var mu sync.Mutex
	n := 0
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (string, error) {
		mu.Lock()
		n++
		mine := n
		mu.Unlock()
		if mine == 1 {
			return "", errors.New("one failed")
		}
		return "ok", nil
	})
	st := starterState()
	st.Handler.Fanout.Wait = teamgraph.WaitAtLeast + ":2"

	out, err := r.RunHandler(context.Background(), st, &Task{})
	if err != nil {
		t.Fatalf("at_least:2 with 2 successes must pass: %v", err)
	}
	if !strings.Contains(out.Output, `"ok":false`) {
		t.Errorf("the envelope hides the failure: %s", out.Output)
	}
	if len(ch.acked) != 1 {
		t.Errorf("a successful wave did not ack: %v", ch.acked)
	}
}

// A list of agents spreads the wave across roles, cycling when there are more
// messages than agents — `agents` is "these roles", not "this many runs".
func TestStarterFanout_AgentsListCyclesAcrossMessages(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(4)}
	var mu sync.Mutex
	counts := map[string]int{}
	r := starterRunner(ch, func(_ context.Context, agent string, _ Prompt, _ string) (string, error) {
		mu.Lock()
		counts[agent]++
		mu.Unlock()
		return "ok", nil
	})
	st := starterState()
	st.Handler.Fanout.Agent = ""
	st.Handler.Fanout.Agents = []string{"a", "b"}

	if _, err := r.RunHandler(context.Background(), st, &Task{}); err != nil {
		t.Fatalf("wave: %v", err)
	}
	if counts["a"] != 2 || counts["b"] != 2 {
		t.Errorf("agent spread = %v, want 2 each", counts)
	}
}
