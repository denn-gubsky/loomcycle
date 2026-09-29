package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// holdSeen is one hold transition as the run-state stream carried it, and the
// index of the event that caused it — a wait that ends on the wrong event
// still reads as a start and an end.
type holdSeen struct {
	at                 int
	state, on, expires string
}

func holdToolCall(id, name, input string) providers.Event {
	return providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}}
}

func holdToolResult(id, name string) providers.Event {
	return providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: id, Name: name}, Text: "ok"}
}

// drainHolds reads every event already on the subscription.
func drainHolds(t *testing.T, sub *runstate.Subscription) []runstate.RunStateEvent {
	t.Helper()
	var out []runstate.RunStateEvent
	for {
		select {
		case evt := <-sub.C:
			out = append(out, evt)
		default:
			return out
		}
	}
}

// Each wait a run enters is announced on the run-state stream once, as a
// "running" transition naming it, and its end as a "running" transition naming
// none. A run that only makes progress publishes nothing extra.
func TestRecordingEmit_AnnouncesEachWaitOnceAndItsEnd(t *testing.T) {
	const expires = "2026-09-29T12:00:00Z"
	review := providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{Round: 1, ExpiresAt: expires, HeldBy: "ops/gate"}}
	parked := providers.Event{Type: providers.EventAwaitingInput, AwaitingInput: &providers.AwaitingInputEventInfo{SinceTurn: 1}}
	compacted := providers.Event{Type: providers.EventContextCompaction}
	steered := providers.Event{Type: providers.EventSteer, UserInput: &providers.UserInputEventInfo{Text: "more"}}
	text := providers.Event{Type: providers.EventText, Text: "answer"}
	done := providers.Event{Type: providers.EventDone, StopReason: "end_turn"}
	for _, tc := range []struct {
		name   string
		events []providers.Event
		want   []holdSeen
	}{
		{"review_hold_reannounced_after_compaction_is_published_once",
			[]providers.Event{text, review, compacted, review, done},
			[]holdSeen{{1, awaited.Review, "ops/gate", expires}, {at: 4}}},
		{"interactive_park_ends_on_the_operators_turn",
			[]providers.Event{text, parked, compacted, steered, text, parked},
			[]holdSeen{{at: 1, state: awaited.Input}, {at: 3}, {at: 5, state: awaited.Input}}},
		{"approved_interactive_review_moves_to_input",
			[]providers.Event{review, parked},
			[]holdSeen{{0, awaited.Review, "ops/gate", expires}, {at: 1, state: awaited.Input}}},
		{"channel_wait_ends_on_its_own_result_not_a_siblings",
			[]providers.Event{
				holdToolCall("tu_sub", "Channel", `{"op":"subscribe","channel":"findings"}`),
				holdToolCall("tu_read", "Read", `{"path":"/x"}`),
				holdToolResult("tu_read", "Read"),
				holdToolResult("tu_sub", "Channel"),
				text,
			},
			[]holdSeen{{at: 0, state: awaited.Channel, on: "findings"}, {at: 3}}},
		{"interruption_ask_waits_until_answered",
			[]providers.Event{holdToolCall("tu_ask", "Interruption", `{"op":"ask","kind":"approval"}`), holdToolResult("tu_ask", "Interruption")},
			[]holdSeen{{at: 0, state: awaited.Interrupted, on: "approval"}, {at: 1}}},
		{"progress_only_publishes_nothing",
			[]providers.Event{text, holdToolCall("tu_1", "Read", `{}`), holdToolResult("tu_1", "Read"),
				holdToolCall("tu_2", "Channel", `{"op":"publish","channel":"c"}`), holdToolResult("tu_2", "Channel"), done},
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cleanup := channelFanFixture(t)
			defer cleanup()
			bus := runstate.NewBus()
			srv.SetRunStateBus(bus)
			sub := bus.Subscribe("")
			defer sub.Close()
			run := seedTenantRun(t, srv.store, "acme", "u1", "a_hold")
			meta := runStateMeta{RunID: run.ID, AgentID: "a_hold", Agent: "writer", UserID: "u1", TenantID: "acme",
				ParentContext: &store.ParentContext{WalkID: "r_walk"}}
			emit := srv.makeRecordingEmit(context.Background(), run.ID, tools.RunIdentityValue{}, run.SessionID, meta, func(providers.Event) {})
			var seen []holdSeen
			for i, ev := range tc.events {
				emit(ev) // the publish is synchronous, so what is on the bus now is this event's
				for _, evt := range drainHolds(t, sub) {
					if evt.Status != "running" || evt.RunID != run.ID || evt.TenantID != "acme" || evt.Agent != "writer" ||
						evt.ParentContext == nil || evt.ParentContext.WalkID != "r_walk" {
						t.Errorf("hold event %+v does not carry the run's identity", evt)
					}
					seen = append(seen, holdSeen{i, evt.AwaitedState, evt.AwaitedOn, evt.HoldExpiresAt})
				}
			}
			if len(seen) != len(tc.want) {
				t.Fatalf("published %+v, want %+v", seen, tc.want)
			}
			for i := range seen {
				if seen[i] != tc.want[i] {
					t.Errorf("transition %d = %+v, want %+v", i, seen[i], tc.want[i])
				}
			}
		})
	}
}

// runStates collects one run's run-state events off the bus as they arrive.
type runStates struct {
	t   *testing.T
	sub *runstate.Subscription
	got []runstate.RunStateEvent
}

// waitFor reads until an event of runID matches, keeping everything read.
func (r *runStates) waitFor(runID string, match func(runstate.RunStateEvent) bool) {
	r.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-r.sub.C:
			if evt.RunID != runID {
				continue
			}
			r.got = append(r.got, evt)
			if match(evt) {
				return
			}
		case <-deadline:
			r.t.Fatalf("no matching run-state event for %s; saw %+v", runID, r.got)
		}
	}
}

func holdIs(state string) func(runstate.RunStateEvent) bool {
	return func(e runstate.RunStateEvent) bool { return e.Status == "running" && e.AwaitedState == state }
}

func endsIn(s store.RunStatus) func(runstate.RunStateEvent) bool {
	return func(e runstate.RunStateEvent) bool { return e.Status == string(s) }
}

// The whole review hold through the real server: the stream says the run is
// held — with its deadline — and, on approval, that it is no longer held,
// before it completes.
func TestReview_HoldIsAnnouncedOnTheRunStateStream(t *testing.T) {
	h := newReviewHarness(t)
	bus := runstate.NewBus()
	h.srv.SetRunStateBus(bus)
	rs := &runStates{t: t, sub: bus.Subscribe("")}
	defer rs.sub.Close()

	runID, _, frames, stop := h.start(`{"agent":"writer","review":true,"review_ttl_seconds":600,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
	defer stop()
	h.waitFrame(frames, "awaiting_review")
	rs.waitFor(runID, holdIs(awaited.Review))
	held := rs.got[len(rs.got)-1]
	if held.HoldExpiresAt == "" {
		t.Errorf("the hold was announced without its deadline: %+v", held)
	} else if at, err := time.Parse(time.RFC3339, held.HoldExpiresAt); err != nil || time.Until(at) < 9*time.Minute {
		t.Errorf("hold_expires_at = %q, want ~10 minutes out", held.HoldExpiresAt)
	}

	h.waitHeld(runID, 1)
	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	rs.waitFor(runID, endsIn(store.RunCompleted))
	// held → released → completed: the release is its own "running" event.
	n := len(rs.got)
	if n < 3 || rs.got[n-2].Status != "running" || rs.got[n-2].AwaitedState != "" || rs.got[n-3].AwaitedState != awaited.Review {
		t.Errorf("transitions = %+v, want held, then running with no wait, then completed", rs.got)
	}
}

// An interactive run parked for input says so on the stream, and the
// operator's next turn clears it.
func TestInteractiveRun_ParkIsAnnouncedOnTheRunStateStream(t *testing.T) {
	h := newReviewHarness(t)
	bus := runstate.NewBus()
	h.srv.SetRunStateBus(bus)
	rs := &runStates{t: t, sub: bus.Subscribe("")}
	defer rs.sub.Close()

	runID, _, frames, stop := h.start(`{"agent":"writer","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)
	defer stop()
	h.waitFrame(frames, "awaiting_input")
	rs.waitFor(runID, holdIs(awaited.Input))

	resp, err := http.Post(h.ts.URL+"/v1/runs/"+runID+"/input", "application/json", strings.NewReader(`{"text":"keep going"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("input = %d", resp.StatusCode)
	}
	rs.waitFor(runID, holdIs(""))
	rs.waitFor(runID, holdIs(awaited.Input)) // the next turn parks again
}

// A walk's view is the run-state stream filtered to the walk. A starter member
// held for review is exactly what that view exists to show, so the hold must
// carry the member's walk lineage and pass the filter.
func TestStreamUserRunStates_WalkFilterReceivesAMembersHold(t *testing.T) {
	h := newReviewHarness(t)
	bus := runstate.NewBus()
	h.srv.SetRunStateBus(bus)

	ctx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	got := make(chan connector.RunStateEvent, 16)
	go func() {
		_ = h.srv.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{UserID: "u1", WalkID: "r_walk"},
			func(evt connector.RunStateEvent) error { got <- evt; return nil })
	}()
	for deadline := time.Now().Add(time.Second); bus.ActiveSubscriberCount() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the walk stream never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	memberCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	memberCtx = teamrun.WithReviewArming(memberCtx, func(context.Context) bool { return true })
	memberCtx = store.WithWaveTask(memberCtx, store.WaveTask{WalkID: "r_walk", WaveID: "w1", Index: 0})
	done := make(chan teamrun.SpawnResult, 1)
	go func() {
		res, _ := h.srv.runTeamMember(memberCtx, "writer", teamrun.Prompt{Input: "write the plan"}, "")
		done <- res
	}()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-got:
			if evt.AwaitedState != awaited.Review {
				continue
			}
			if evt.ParentContext == nil || evt.ParentContext.WaveID != "w1" {
				t.Errorf("the hold lost the member's wave: %+v", evt.ParentContext)
			}
			if code, body := h.review(evt.RunID, `{"decision":"approve"}`); code != http.StatusOK {
				t.Fatalf("approve = %d %s", code, body)
			}
			awaitMember(t, done)
			return
		case <-deadline:
			t.Fatal("the walk's stream never saw the member's hold")
		}
	}
}
