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

func publishingInputState(channel string) teamgraph.State {
	return teamgraph.State{ID: "form", Handler: teamgraph.Handler{
		Kind:    teamgraph.HandlerInput,
		Capture: map[string]string{"part": "$.part"},
		Publish: &teamgraph.InputPublish{Channel: channel},
	}}
}

// decodeAs unmarshals a published payload into a Go value, so a test can say
// "this is an object" rather than compare bytes.
func decodeAs(t *testing.T, payload json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("published payload is not JSON: %s", payload)
	}
	return v
}

// The whole point: the form's object goes on the channel AS an object, so a
// downstream Starter's binds can reach its fields. The state still threads its
// input on unchanged, and its capture still binds.
func TestInputState_PublishesTheParsedObjectAndThreadsInputOn(t *testing.T) {
	ch := &fakeChannels{}
	r := starterRunner(ch, nil)
	task := &Task{Input: `{"part": "cpu", "n": 2}`}

	out, err := r.RunHandler(context.Background(), publishingInputState("pcparts-in"), task)
	if err != nil {
		t.Fatalf("input publish: %v", err)
	}
	if out.Output != task.Input {
		t.Errorf("output = %q, want the input threaded on unchanged", out.Output)
	}
	if task.Vars["part"] != "cpu" {
		t.Errorf("capture part = %q, want cpu", task.Vars["part"])
	}
	if len(ch.published) != 1 || ch.published[0].Channel != "pcparts-in" {
		t.Fatalf("published = %+v, want one message on pcparts-in", ch.published)
	}
	obj, ok := decodeAs(t, ch.published[0].Payload).(map[string]any)
	if !ok {
		t.Fatalf("published %s, want a JSON object — not a string holding one", ch.published[0].Payload)
	}
	if obj["part"] != "cpu" || obj["n"] != float64(2) {
		t.Errorf("published object = %v, want the input's fields", obj)
	}
}

func TestInputState_PublishesNonJSONInputAsText(t *testing.T) {
	ch := &fakeChannels{}
	r := starterRunner(ch, nil)

	if _, err := r.RunHandler(context.Background(), publishingInputState("pcparts-in"), &Task{Input: "a quiet gaming pc"}); err != nil {
		t.Fatalf("input publish: %v", err)
	}
	if len(ch.published) != 1 {
		t.Fatalf("published = %+v, want one message", ch.published)
	}
	if got := string(ch.published[0].Payload); got != `{"text":"a quiet gaming pc"}` {
		t.Errorf("published %s, want {\"text\": <input>}", got)
	}
}

// A publish that fails fails the state — and before its capture, because the
// publish goes first.
func TestInputState_PublishFailureFailsTheState(t *testing.T) {
	ch := &fakeChannels{pubErr: errors.New("channel is full")}
	r := starterRunner(ch, nil)
	task := &Task{Input: `{"part":"cpu"}`}

	_, err := r.RunHandler(context.Background(), publishingInputState("pcparts-in"), task)
	if err == nil || !strings.Contains(err.Error(), "channel is full") || !strings.Contains(err.Error(), `"pcparts-in"`) {
		t.Fatalf("err = %v, want the publish failure naming the channel", err)
	}
	if _, bound := task.Vars["part"]; bound {
		t.Errorf("capture ran after a failed publish")
	}
}

func TestInputState_PublishWithNoChannelExecutorIsRefused(t *testing.T) {
	r := &agentRunner{logf: func(string, ...any) {}}
	_, err := r.RunHandler(context.Background(), publishingInputState("pcparts-in"), &Task{Input: "x"})
	if err == nil || !strings.Contains(err.Error(), "no channel executor") {
		t.Errorf("err = %v, want a refusal naming the missing executor", err)
	}
}

// Without `publish` an input state is what it always was: no executor needed,
// nothing published.
func TestInputState_WithoutPublishPublishesNothing(t *testing.T) {
	ch := &fakeChannels{}
	r := starterRunner(ch, nil)
	st := publishingInputState("")
	st.Handler.Publish = nil

	if _, err := r.RunHandler(context.Background(), st, &Task{Input: `{"part":"cpu"}`}); err != nil {
		t.Fatalf("input: %v", err)
	}
	if len(ch.published) != 0 {
		t.Errorf("published %+v from an input state with no publish", ch.published)
	}
}

func TestChannelKind_RawPayloadPublishesTheJSONValue(t *testing.T) {
	ch := &fakeChannels{}
	r := starterRunner(ch, nil)
	st := teamgraph.State{ID: "announce", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerChannel, Channel: "pcparts-in", Payload: teamgraph.PayloadRaw}}

	out, err := r.RunHandler(context.Background(), st, &Task{Input: `{"part":"gpu"}`})
	if err != nil {
		t.Fatalf("channel publish: %v", err)
	}
	if out.Output != `{"part":"gpu"}` {
		t.Errorf("output = %q, want the input threaded on unchanged", out.Output)
	}
	if obj, ok := decodeAs(t, ch.published[0].Payload).(map[string]any); !ok || obj["part"] != "gpu" {
		t.Errorf("published %s, want the input's object itself", ch.published[0].Payload)
	}

	// Text that is not JSON follows the same rule as an input state's publish.
	ch.published = nil
	if _, err := r.RunHandler(context.Background(), st, &Task{Input: "plain words"}); err != nil {
		t.Fatalf("channel publish: %v", err)
	}
	if got := string(ch.published[0].Payload); got != `{"text":"plain words"}` {
		t.Errorf("published %s, want {\"text\": <input>}", got)
	}
}

// Every existing channel state publishes the envelope, and a consumer may
// match its bytes: omitting `payload`, or naming the default, must not move
// one of them.
func TestChannelKind_EnvelopeBytesAreUnchanged(t *testing.T) {
	for _, payload := range []string{"", teamgraph.PayloadEnvelope} {
		ch := &fakeChannels{}
		r := starterRunner(ch, nil)
		st := teamgraph.State{ID: "announce", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerChannel, Channel: "verdicts", Payload: payload}}

		if _, err := r.RunHandler(context.Background(), st, &Task{Input: `{"part":"gpu"}`}); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		const want = `{"output":"{\"part\":\"gpu\"}","state":"announce"}`
		if got := string(ch.published[0].Payload); got != want {
			t.Errorf("payload %q published %s, want the envelope byte-identical to before: %s", payload, got, want)
		}
	}
}

// loopChannels delivers what is published to a channel to the next read of
// that channel, so a walk can publish and read itself back.
type loopChannels struct {
	mu     sync.Mutex
	queues map[string][]ChannelMessage
}

func (l *loopChannels) Read(_ context.Context, channel string, _, _, _ int) ([]ChannelMessage, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.queues[channel], "cur", nil
}

func (l *loopChannels) Ack(context.Context, string, string) error { return nil }

func (l *loopChannels) Publish(_ context.Context, channel string, payload json.RawMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queues == nil {
		l.queues = map[string][]ChannelMessage{}
	}
	l.queues[channel] = append(l.queues[channel], ChannelMessage{ID: "m", Payload: payload})
	return nil
}

// End to end: an input state publishes the form, and a Starter reading that
// channel binds one of its fields — which the envelope made impossible.
func TestWalk_StarterBindsAFieldOfAnInputPublishedMessage(t *testing.T) {
	d := teamgraph.Definition{
		Entry: "form",
		States: []teamgraph.State{
			publishingInputState("pcparts-in"),
			{ID: "research", Handler: teamgraph.Handler{
				Kind:   teamgraph.HandlerStarter,
				Source: &teamgraph.StarterSource{Channel: "pcparts-in"},
				Fanout: &teamgraph.StarterFanout{Agent: "researcher", Per: teamgraph.FanoutPerMessage, Max: 1},
				Binds:  map[string]string{"item": "$.part"},
				Prompt: &teamgraph.StarterPrompt{Input: "Research ${var.item}."},
				Sink:   &teamgraph.StarterSink{Channel: "pcparts-research"},
			}},
			{ID: "done", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}},
		},
		Transitions: []teamgraph.Transition{
			{From: "form", To: "research", On: teamgraph.OnSuccess},
			{From: "research", To: "done", On: teamgraph.OnSuccess},
		},
	}
	if err := teamgraph.Validate(d); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	var mu sync.Mutex
	var got Prompt
	spawn := textSpawn(func(_ context.Context, _ string, p Prompt, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		got = p
		return "researched", nil
	})
	r := NewAgentRunner(spawn, WithChannels(&loopChannels{}))

	if _, err := Walk(context.Background(), d, &Task{Input: `{"part":"cpu"}`}, r); err != nil {
		t.Fatalf("walk: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Values["var.item"] != "cpu" {
		t.Errorf("var.item = %q, want the field the Starter bound from the published object", got.Values["var.item"])
	}
}
