package teamgraph

import (
	"strings"
	"testing"
)

// starterDef wraps one starter state in the smallest valid graph.
func starterDef(h Handler) Definition {
	return Definition{
		Entry: "wave",
		States: []State{
			{ID: "wave", Handler: h},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "wave", To: "done", On: OnSuccess}},
	}
}

func okStarter() Handler {
	return Handler{
		Kind:   HandlerStarter,
		Source: &StarterSource{Channel: "pr-events"},
		Fanout: &StarterFanout{Agent: "reviewer", Per: FanoutPerMessage, Max: 8},
		Sink:   &StarterSink{Channel: "verdicts"},
	}
}

func TestValidate_StarterHappyPath(t *testing.T) {
	if err := Validate(starterDef(okStarter())); err != nil {
		t.Fatalf("a minimal starter must validate: %v", err)
	}
}

// Each of these is a definition that would otherwise dispatch the wrong number
// of runs, dispatch them nowhere, or wait for something that cannot arrive.
func TestValidate_StarterRefusals(t *testing.T) {
	mut := func(f func(h *Handler)) Handler {
		h := okStarter()
		f(&h)
		return h
	}
	cases := []struct {
		name string
		h    Handler
		want string
	}{
		{"no source", mut(func(h *Handler) { h.Source = nil }), "requires `source.channel`"},
		{"empty source channel", mut(func(h *Handler) { h.Source.Channel = "  " }), "requires `source.channel`"},
		// `all` counts CHANNELS; over the one channel a starter reads it returns
		// after the FIRST message, so it silently yields one of N.
		{"wait=all", mut(func(h *Handler) { h.Source.Wait = WaitAll }), "counts CHANNELS"},
		{"wait=at_least without n", mut(func(h *Handler) { h.Source.Wait = WaitAtLeast }), "requires `n` >= 1"},
		{"unknown wait", mut(func(h *Handler) { h.Source.Wait = "eventually" }), "invalid wait"},
		{"no fanout", mut(func(h *Handler) { h.Fanout = nil }), "requires `fanout`"},
		{"both agent and agents", mut(func(h *Handler) { h.Fanout.Agents = []string{"a"} }), "exactly one of"},
		{"neither agent nor agents", mut(func(h *Handler) { h.Fanout.Agent = "" }), "exactly one of"},
		{"empty agent name in agents", mut(func(h *Handler) { h.Fanout.Agent = ""; h.Fanout.Agents = []string{" "} }), "empty agent name"},
		// Dynamic fan-out is a spawn amplifier — the ceiling is not optional.
		{"per=message without max", mut(func(h *Handler) { h.Fanout.Max = 0 }), "requires `max` >= 1"},
		{"per=once with max", mut(func(h *Handler) { h.Fanout.Per = FanoutPerOnce }), "`max` means nothing"},
		{"unknown per", mut(func(h *Handler) { h.Fanout.Per = "each" }), "invalid per"},
		{"sink with no channel", mut(func(h *Handler) { h.Sink = &StarterSink{} }), "names no channel"},
		{"unknown ack", mut(func(h *Handler) { h.Ack = "never" }), "invalid ack"},
		{"agents on the handler itself", mut(func(h *Handler) { h.Agent = "x" }), "names its agents in `fanout`"},
		{"negative batch", mut(func(h *Handler) { h.Source.Batch = -1 }), "must be >= 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(starterDef(c.h))
			if err == nil {
				t.Fatalf("accepted; want a refusal mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refused with %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// A starter's fields on any other kind would read as configured and do nothing
// — the silent-setting failure `set` and `schema` are already guarded against.
func TestValidate_StarterFieldsOnAnotherKindAreRefused(t *testing.T) {
	cases := []struct {
		name string
		h    Handler
		want string
	}{
		{"source", Handler{Kind: HandlerAgent, Agent: "a", Source: &StarterSource{Channel: "c"}}, "starter only"},
		{"fanout", Handler{Kind: HandlerAgent, Agent: "a", Fanout: &StarterFanout{Agent: "b", Max: 1}}, "starter only"},
		{"sink", Handler{Kind: HandlerAgent, Agent: "a", Sink: &StarterSink{Channel: "c"}}, "starter only"},
		{"binds", Handler{Kind: HandlerAgent, Agent: "a", Binds: map[string]string{"x": "$.y"}}, "starter only"},
		{"ack", Handler{Kind: HandlerAgent, Agent: "a", Ack: AckAfterRead}, "starter only"},
		{"prompt", Handler{Kind: HandlerAgent, Agent: "a", Prompt: &StarterPrompt{Input: "hi"}}, "`system_prompt` + `input_template`"},
		{"channel on an agent", Handler{Kind: HandlerAgent, Agent: "a", Channel: "c"}, "names its channels in `source`/`sink`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(starterDef(c.h))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want a refusal mentioning %q", err, c.want)
			}
		})
	}
}

// The `channel` kind PUBLISHES. Reading is a starter's job, and a channel node
// that set a source would be a second reader competing on the same cursor.
func TestValidate_ChannelKindPublishesOnly(t *testing.T) {
	if err := Validate(starterDef(Handler{Kind: HandlerChannel, Channel: "verdicts"})); err != nil {
		t.Fatalf("a publish-only channel node must validate: %v", err)
	}
	for _, c := range []struct {
		name string
		h    Handler
		want string
	}{
		{"no channel", Handler{Kind: HandlerChannel}, "requires `channel`"},
		{"with an agent", Handler{Kind: HandlerChannel, Channel: "c", Agent: "a"}, "it publishes, it does not run"},
		{"with a source", Handler{Kind: HandlerChannel, Channel: "c", Source: &StarterSource{Channel: "x"}}, "is a `starter`, not a `channel`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(starterDef(c.h))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want a refusal mentioning %q", err, c.want)
			}
		})
	}
}

// binds share the capture validator, so a malformed JSONPath is refused at
// authoring rather than binding nothing at run time.
func TestValidate_StarterBindsAreValidatedLikeCapture(t *testing.T) {
	h := okStarter()
	h.Binds = map[string]string{"pr": "not a path"}
	if err := Validate(starterDef(h)); err == nil {
		t.Errorf("a malformed binds path was accepted")
	}
}
