package awaited

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestFromEvent_ReportsWhatTheOpenToolCallBlocksOn(t *testing.T) {
	cases := []struct {
		name      string
		ev        store.Event
		wantState string
		wantOn    string
	}{
		{
			name:      "non_tool_call_event_yields_running",
			ev:        store.Event{Type: "text", Payload: []byte(`{"type":"text","text":"hi"}`)},
			wantState: "",
		},
		{
			name: "channel_subscribe_yields_channel_state_with_name",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}
				}`),
			},
			wantState: "channel",
			wantOn:    "findings",
		},
		{
			name: "channel_publish_does_not_block_yields_running",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_1","name":"Channel","input":{"op":"publish","channel":"findings"}}
				}`),
			},
			wantState: "",
		},
		{
			name: "interruption_ask_yields_interrupted_state_kind_default_question",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_2","name":"Interruption","input":{"op":"ask","question":"continue?"}}
				}`),
			},
			wantState: "interrupted",
			wantOn:    "question",
		},
		{
			name: "interruption_ask_with_explicit_kind",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_3","name":"Interruption","input":{"op":"ask","kind":"approval"}}
				}`),
			},
			wantState: "interrupted",
			wantOn:    "approval",
		},
		{
			name: "interruption_notify_does_not_block_yields_running",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_4","name":"Interruption","input":{"op":"notify","message":"hi"}}
				}`),
			},
			wantState: "",
		},
		{
			name: "other_tool_call_yields_running",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_5","name":"Read","input":{"path":"/tmp/x"}}
				}`),
			},
			wantState: "",
		},
		{
			name:      "awaiting_input_yields_input_state",
			ev:        store.Event{Type: "awaiting_input", Payload: []byte(`{"type":"awaiting_input","awaiting_input":{"since_turn":2}}`)},
			wantState: "input",
		},
		{
			name:      "malformed_payload_degrades_to_running",
			ev:        store.Event{Type: "tool_call", Payload: []byte(`{not json`)},
			wantState: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotS, gotO := FromEvent(tc.ev)
			if gotS != tc.wantState || gotO != tc.wantOn {
				t.Errorf("got (%q,%q), want (%q,%q)", gotS, gotO, tc.wantState, tc.wantOn)
			}
		})
	}
}

// fakeEvents is a run's persisted event types in append order.
type fakeEvents []store.Event

func (f fakeEvents) GetLastEventForRun(context.Context, string) (store.Event, error) {
	if len(f) == 0 {
		return store.Event{}, &store.ErrNotFound{Kind: "event", ID: "r"}
	}
	return f[len(f)-1], nil
}

func (f fakeEvents) GetLastEventOfTypes(_ context.Context, _ string, types []string) (store.Event, error) {
	for i := len(f) - 1; i >= 0; i-- {
		for _, t := range types {
			if f[i].Type == t {
				return f[i], nil
			}
		}
	}
	return store.Event{}, &store.ErrNotFound{Kind: "event", ID: "r"}
}

func ev(typ, payload string) store.Event { return store.Event{Type: typ, Payload: []byte(payload)} }

// A parked interactive run reads as waiting for input until the operator's turn
// lands, however many other rows are written while it waits; a review hold
// names the hook that took it.
func TestForRun_ReportsAParkedRunAndTheHookHoldingAReview(t *testing.T) {
	parked := ev("awaiting_input", `{"type":"awaiting_input","awaiting_input":{"since_turn":1}}`)
	for _, tc := range []struct {
		name              string
		events            fakeEvents
		wantState, wantOn string
	}{
		{"parked", fakeEvents{ev("user_input", `[]`), parked}, Input, ""},
		{"parked_then_compacted", fakeEvents{ev("user_input", `[]`), parked, ev("context_compaction", `{}`)}, Input, ""},
		{"operator_turn_ends_the_park", fakeEvents{parked, ev("user_input", `[]`), ev("text", `{"type":"text"}`)}, "", ""},
		{"review_by_hook", fakeEvents{ev("awaiting_review", `{"type":"awaiting_review","awaiting_review":{"round":1,"held_by":"ops/gate"}}`), ev("limit", `{}`)}, Review, "ops/gate"},
		{"review_by_arming", fakeEvents{ev("awaiting_review", `{"type":"awaiting_review","awaiting_review":{"round":1}}`)}, Review, ""},
		{"approved_interactive_parks", fakeEvents{ev("awaiting_review", `{"type":"awaiting_review","awaiting_review":{"round":1}}`), parked}, Input, ""},
		{"no_events", fakeEvents{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotS, gotO := ForRun(context.Background(), tc.events, "r")
			if gotS != tc.wantState || gotO != tc.wantOn {
				t.Errorf("ForRun = (%q,%q), want (%q,%q)", gotS, gotO, tc.wantState, tc.wantOn)
			}
		})
	}
}
