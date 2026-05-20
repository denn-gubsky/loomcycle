package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestDeriveAwaitedState(t *testing.T) {
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
			name:      "malformed_payload_degrades_to_running",
			ev:        store.Event{Type: "tool_call", Payload: []byte(`{not json`)},
			wantState: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotS, gotO := deriveAwaitedState(tc.ev)
			if gotS != tc.wantState || gotO != tc.wantOn {
				t.Errorf("got (%q,%q), want (%q,%q)", gotS, gotO, tc.wantState, tc.wantOn)
			}
		})
	}
}
