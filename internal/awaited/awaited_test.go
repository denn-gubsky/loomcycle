package awaited

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

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
			name: "channel_await_long_poll_yields_channel_state_with_its_channels",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_1","name":"Channel","input":{"op":"await","channels":["a","b","a"],"mode":"all","wait_ms":5000}}
				}`),
			},
			wantState: "channel",
			wantOn:    "a, b",
		},
		{
			name: "channel_await_without_wait_does_not_block_yields_running",
			ev: store.Event{
				Type: "tool_call",
				Payload: []byte(`{
					"type":"tool_call",
					"tool_use":{"id":"tu_1","name":"Channel","input":{"op":"await","channels":["a","b"]}}
				}`),
			},
			wantState: "",
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

// fakeEvents is a run's persisted events in append order; an event's seq is
// its position, from 1.
type fakeEvents []store.Event

func (f fakeEvents) at(i int) store.Event { e := f[i]; e.Seq = int64(i + 1); return e }

func (f fakeEvents) GetLastEventOfTypes(ctx context.Context, runID string, types []string) (store.Event, error) {
	return f.GetLastEventOfTypesBefore(ctx, runID, types, int64(len(f)+1))
}

func (f fakeEvents) GetLastEventOfTypesBefore(_ context.Context, _ string, types []string, beforeSeq int64) (store.Event, error) {
	for i := min(len(f), int(beforeSeq-1)) - 1; i >= 0; i-- {
		if slices.Contains(types, f[i].Type) {
			return f.at(i), nil
		}
	}
	return store.Event{}, &store.ErrNotFound{Kind: "event", ID: "r"}
}

func (f fakeEvents) GetRunEventsSince(_ context.Context, _ string, afterSeq int64, limit int) ([]store.Event, error) {
	var out []store.Event
	for i := int(afterSeq); i < len(f) && len(out) < limit; i++ {
		out = append(out, f.at(i))
	}
	return out, nil
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

func call(id, name, input string) store.Event {
	return ev("tool_call", `{"type":"tool_call","tool_use":{"id":"`+id+`","name":"`+name+`","input":`+input+`}}`)
}

func result(id string) store.Event {
	return ev("tool_result", `{"type":"tool_result","tool_use":{"id":"`+id+`"},"text":"ok"}`)
}

// A turn's calls are persisted as the model streams them, the turn's usage
// after them, and only then do they run — in parallel. A call is open until its
// own result, whatever was written since; with several open, the newest is
// reported, as the run-state stream announces it.
func TestForRun_ReportsTheNewestToolCallStillOpen(t *testing.T) {
	sub := call("tu_sub", "Channel", `{"op":"subscribe","channel":"findings"}`)
	ask := call("tu_ask", "Interruption", `{"op":"ask","kind":"approval"}`)
	read := call("tu_read", "Read", `{"path":"/x"}`)
	await := call("tu_await", "Channel", `{"op":"await","channels":["findings","reviews"],"wait_ms":60000}`)
	usage := ev("usage", `{"type":"usage"}`)
	retry := ev("retry", `{"type":"retry"}`)
	text := ev("text", `{"type":"text","text":"."}`)
	parked := ev("awaiting_input", `{"type":"awaiting_input","awaiting_input":{"since_turn":2}}`)
	many := func(n int, e store.Event) fakeEvents { return slices.Repeat(fakeEvents{e}, n) }
	for _, tc := range []struct {
		name              string
		events            fakeEvents
		wantState, wantOn string
	}{
		{"a_running_call_behind_its_turns_usage", fakeEvents{text, sub, usage}, Channel, "findings"},
		{"a_non_blocking_sibling_streamed_after_it", fakeEvents{sub, read, usage, result("tu_read")}, Channel, "findings"},
		{"two_open_reports_the_newest", fakeEvents{sub, ask, usage}, Interrupted, "approval"},
		{"the_later_calls_result_leaves_the_other_open", fakeEvents{sub, ask, usage, result("tu_ask")}, Channel, "findings"},
		{"the_earlier_calls_result_leaves_the_later_open", fakeEvents{sub, ask, usage, result("tu_sub")}, Interrupted, "approval"},
		{"all_returned", fakeEvents{sub, ask, usage, result("tu_ask"), result("tu_sub"), text}, "", ""},
		{"an_open_fan_in_await", fakeEvents{text, await, usage}, Channel, "findings, reviews"},
		{"a_fan_in_await_that_returned", fakeEvents{text, await, usage, result("tu_await"), text}, "", ""},
		{"an_earlier_turns_call_that_never_returned", fakeEvents{sub, usage, text, read, usage, result("tu_read")}, "", ""},
		{"an_earlier_turns_call_has_returned", fakeEvents{sub, usage, result("tu_sub"), text, read, usage}, "", ""},
		{"a_park_after_the_turn_returned", fakeEvents{sub, usage, result("tu_sub"), text, usage, parked}, Input, ""},
		// A call streamed before a retry or a cancel never runs, so no result
		// ends it.
		{"a_retried_attempts_call", fakeEvents{call("tu_sub_1", "Channel", `{"op":"subscribe","channel":"old"}`), retry,
			call("tu_sub_2", "Channel", `{"op":"subscribe","channel":"findings"}`), usage, result("tu_sub_2")}, "", ""},
		{"a_retry_that_streamed_no_call", fakeEvents{read, usage, result("tu_read"), sub, retry, text, usage}, "", ""},
		{"a_cancelled_turns_call", fakeEvents{sub, ev("turn_cancelled", `{}`)}, "", ""},
		{"a_cancelled_turns_call_before_the_next_turn", fakeEvents{sub, ev("turn_cancelled", `{}`), parked, ev("user_input", `[]`), read, usage}, "", ""},
		{"a_long_preamble_before_the_call", append(append(fakeEvents{read, usage, result("tu_read")}, many(600, text)...), sub, usage), Channel, "findings"},
		{"a_long_tail_while_the_call_is_open", append(fakeEvents{sub, usage}, many(600, ev("hook_decision", `{}`))...), Channel, "findings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotS, gotO := ForRun(context.Background(), tc.events, "r")
			if gotS != tc.wantState || gotO != tc.wantOn {
				t.Errorf("ForRun = (%q,%q), want (%q,%q)", gotS, gotO, tc.wantState, tc.wantOn)
			}
		})
	}
}

// An await names up to 32 channels from the model's input, and its awaited_on
// rides every run read and run-state frame: it is bounded, and says how many
// channels it left out.
func TestFromToolUse_BoundsAFanInAwaitsChannelList(t *testing.T) {
	var names []string
	for i := range 32 {
		names = append(names, strings.Repeat("c", 20)+strconv.Itoa(i))
	}
	long := strings.Repeat("é", 300)
	for _, tc := range []struct {
		name     string
		channels []string
		wantEnd  string
	}{
		{"many_channels", names, "+" + strconv.Itoa(32-8) + " more"},
		{"one_very_long_name", []string{long, "b"}, "…, +1 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, _ := json.Marshal(map[string]any{"op": "await", "channels": tc.channels, "wait_ms": 1000})
			state, on := FromToolUse("Channel", in)
			if state != Channel {
				t.Fatalf("state = %q, want %q", state, Channel)
			}
			if len(on) > awaitOnMax || !utf8.ValidString(on) {
				t.Errorf("awaited_on is %d bytes (valid UTF-8: %v), want at most %d: %q", len(on), utf8.ValidString(on), awaitOnMax, on)
			}
			if !strings.HasSuffix(on, tc.wantEnd) {
				t.Errorf("awaited_on = %q, want it to end %q", on, tc.wantEnd)
			}
		})
	}
}
