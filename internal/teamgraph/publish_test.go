package teamgraph

import (
	"encoding/json"
	"strings"
	"testing"
)

// oneStateDef wraps a handler in the smallest valid graph: it, then a terminal.
func oneStateDef(h Handler) Definition {
	return Definition{
		Entry: "s",
		States: []State{
			{ID: "s", Handler: h},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "s", To: "done", On: OnSuccess}},
	}
}

func TestValidate_PublishAndPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		h       Handler
		wantErr string // "" = valid
	}{
		{"publish on input", Handler{Kind: HandlerInput, Publish: &InputPublish{Channel: "pcparts-in"}}, ""},
		{"publish with schema and capture", Handler{Kind: HandlerInput,
			Schema:  json.RawMessage(`{"type":"object"}`),
			Capture: map[string]string{"part": "$.part"},
			Publish: &InputPublish{Channel: "pcparts-in"}}, ""},
		{"publish with no channel", Handler{Kind: HandlerInput, Publish: &InputPublish{}}, "names no channel"},
		{"publish with a blank channel", Handler{Kind: HandlerInput, Publish: &InputPublish{Channel: "  "}}, "names no channel"},
		{"publish on agent", Handler{Kind: HandlerAgent, Agent: "a", Publish: &InputPublish{Channel: "c"}},
			"sets `publish` but is kind \"agent\""},
		{"publish on channel", Handler{Kind: HandlerChannel, Channel: "c", Publish: &InputPublish{Channel: "c"}},
			"sets `publish` but is kind \"channel\""},
		{"publish on vars", Handler{Kind: HandlerVars, Set: map[string]string{"x": "1"}, Publish: &InputPublish{Channel: "c"}},
			"sets `publish` but is kind \"vars\""},
		{"publish on starter", Handler{Kind: HandlerStarter,
			Source:  &StarterSource{Channel: "in"},
			Fanout:  &StarterFanout{Agent: "a", Max: 1},
			Publish: &InputPublish{Channel: "c"}},
			"sets `publish` but is kind \"starter\""},
		{"payload raw on channel", Handler{Kind: HandlerChannel, Channel: "c", Payload: PayloadRaw}, ""},
		{"payload envelope on channel", Handler{Kind: HandlerChannel, Channel: "c", Payload: PayloadEnvelope}, ""},
		{"invalid payload", Handler{Kind: HandlerChannel, Channel: "c", Payload: "json"}, "invalid payload \"json\""},
		{"payload on input", Handler{Kind: HandlerInput, Payload: PayloadRaw}, "sets `payload` but is kind \"input\""},
		{"payload on agent", Handler{Kind: HandlerAgent, Agent: "a", Payload: PayloadRaw}, "sets `payload` but is kind \"agent\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(oneStateDef(tc.h))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("valid definition refused: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted; want a refusal containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// An input state's publish channel is a PUBLISH reference, so the preflight
// asks for the team's channels.publish grant exactly as for a channel state.
func TestChannelRefs_InputPublishIsAPublishReference(t *testing.T) {
	refs := ChannelRefs(oneStateDef(Handler{Kind: HandlerInput, Publish: &InputPublish{Channel: "pcparts-in"}}))
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want one", refs)
	}
	if got := refs[0]; got != (ChannelRef{"pcparts-in", SidePublish, "s", "publish"}) {
		t.Errorf("ref = %+v, want a publish reference from state s, field publish", got)
	}
}

// Adding `publish` and `payload` must not move a recorded hash. The constant
// was computed from this definition BEFORE either field existed.
func TestSign_InputAndChannelStatesKeepTheRecordedHash(t *testing.T) {
	d := Definition{
		Entry: "form",
		States: []State{
			{ID: "form", Handler: Handler{Kind: HandlerInput,
				Schema:  json.RawMessage(`{"type":"object","required":["part"]}`),
				Capture: map[string]string{"part": "$.part"}}},
			{ID: "announce", Handler: Handler{Kind: HandlerChannel, Channel: "pcparts-in"}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "form", To: "announce", On: OnSuccess}, {From: "announce", To: "done", On: OnSuccess}},
		Channels:    &TeamChannels{Publish: []string{"pcparts-in"}},
	}
	const before = "sha256:2067c5da8cff484224617cfa2faac441e1af96eed70f2a78725f15239b72c50c"
	if got := Sign("t", d); got != before {
		t.Errorf("hash = %s, want the recorded %s — `publish` or `payload` is missing omitempty", got, before)
	}

	// And both are content: what a team publishes, and how, is what it does.
	withPublish := d
	withPublish.States = append([]State(nil), d.States...)
	withPublish.States[0].Handler.Publish = &InputPublish{Channel: "pcparts-in"}
	if Sign("t", withPublish) == before {
		t.Errorf("adding `publish` did not change the hash")
	}
	withRaw := d
	withRaw.States = append([]State(nil), d.States...)
	withRaw.States[1].Handler.Payload = PayloadRaw
	if Sign("t", withRaw) == before {
		t.Errorf("adding `payload` did not change the hash")
	}
}
