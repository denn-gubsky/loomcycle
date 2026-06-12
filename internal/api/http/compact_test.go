package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func firstText(m providers.Message) string {
	if len(m.Content) == 0 {
		return ""
	}
	return m.Content[0].Text
}

// TestReplayTranscript_ContextCompactionResets: a context_compaction marker
// collapses everything before it to the summary pair, and turns AFTER the marker
// replay on top. Fail-before: replayTranscript had no such case, so a rebuild
// reconstructed the full pre-compaction history.
func TestReplayTranscript_ContextCompactionResets(t *testing.T) {
	uinput := func(text string) store.Event {
		return mkEvent("user_input", []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}},
		})
	}
	events := []store.Event{
		uinput("original question"),
		mkEvent("text", providers.Event{Type: providers.EventText, Text: "long original answer"}),
		mkEvent("done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"}),
		mkEvent(string(providers.EventContextCompaction), providers.Event{
			Type:              providers.EventContextCompaction,
			ContextCompaction: &providers.ContextCompactionEventInfo{Summary: "THE-SUMMARY"},
		}),
		uinput("follow-up"),
	}
	msgs := replayTranscript(events)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (summary pair + follow-up): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || !strings.Contains(firstText(msgs[0]), "THE-SUMMARY") {
		t.Errorf("msg[0] should be the summary user turn: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", msgs[1].Role)
	}
	if msgs[2].Role != "user" || firstText(msgs[2]) != "follow-up" {
		t.Errorf("msg[2] = %q (%s); want user 'follow-up'", firstText(msgs[2]), msgs[2].Role)
	}
	for _, m := range msgs {
		if strings.Contains(firstText(m), "original") {
			t.Errorf("pre-compaction content survived the reset: %+v", m)
		}
	}
}

func compactFixture(t *testing.T) (*Server, *scriptedProvider) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"compactor": {Provider: "scripted", Model: "stub-model", AllowedTools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "COMPACTED SUMMARY"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ := makeServer(t, prov, cfg)
	srv.SetSteerRegistry(steer.NewRegistry(8))
	return srv, prov
}

// seedConversation creates a run with a >=4-message transcript (so it's worth
// compacting). When terminal, the run is finished completed.
func seedConversation(t *testing.T, srv *Server, terminal bool) (sessID, runID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "compactor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_c", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	uinput := func(text string) []loop.PromptSegment {
		return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}}
	}
	appendResumeEvent(t, srv, run.ID, "user_input", uinput("question one"))
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "answer one"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	appendResumeEvent(t, srv, run.ID, "user_input", uinput("question two"))
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "answer two"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	if terminal {
		if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	return sess.ID, run.ID
}

func decodeCompact(t *testing.T, body []byte) compactResponse {
	t.Helper()
	var out compactResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode compact response: %v; body=%s", err, body)
	}
	return out
}

// TestHandleCompactRun_409WhenMidTurn: a LIVE run that isn't parked (mid-turn)
// is refused — compaction is gated to a safe boundary.
func TestHandleCompactRun_409WhenMidTurn(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, runID := seedConversation(t, srv, false)
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: runID, SessionID: sessID, UserID: "alice"})
	defer dereg()
	// NOT parked → IsParked false → mid-turn.

	rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/compact", `{}`)
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409 (mid-turn); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleCompactRun_LivePushesCompactControl: a parked live run gets a
// compact control pushed to its steering queue carrying the summary.
func TestHandleCompactRun_LivePushesCompactControl(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, runID := seedConversation(t, srv, false)
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: runID, SessionID: sessID, UserID: "alice"})
	defer dereg()
	srv.steerReg.SetParked(runID, true) // parked = safe boundary

	rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/compact", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCompact(t, rec.Body.Bytes())
	if !out.Compacted || out.Applied != "live" {
		t.Errorf("response = %+v, want compacted=true applied=live", out)
	}
	select {
	case m := <-q:
		if m.Kind != steer.KindCompact {
			t.Errorf("queued message Kind = %q, want %q", m.Kind, steer.KindCompact)
		}
		if m.Text != "COMPACTED SUMMARY" {
			t.Errorf("queued summary = %q, want COMPACTED SUMMARY", m.Text)
		}
	default:
		t.Fatal("no compact control delivered to the run's steering queue")
	}
}

// TestHandleCompactRun_TerminalPersistsMarker: a completed run (no live loop)
// gets a context_compaction marker persisted for the next continuation.
func TestHandleCompactRun_TerminalPersistsMarker(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, runID := seedConversation(t, srv, true) // completed → no steer entry

	rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/compact", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCompact(t, rec.Body.Bytes())
	if !out.Compacted || out.Applied != "marker" {
		t.Errorf("response = %+v, want compacted=true applied=marker", out)
	}
	// The marker is on the transcript so the next continuation rebuilds compacted.
	events, _ := srv.store.GetTranscript(context.Background(), sessID)
	var found bool
	for _, e := range events {
		if e.Type == string(providers.EventContextCompaction) {
			var pe providers.Event
			if json.Unmarshal(e.Payload, &pe) == nil && pe.ContextCompaction != nil &&
				strings.Contains(pe.ContextCompaction.Summary, "COMPACTED SUMMARY") {
				found = true
			}
		}
	}
	if !found {
		t.Error("no context_compaction marker persisted on the transcript")
	}
}

// TestHandleCompactRun_NoopWhenShort: a conversation below the threshold is a
// no-op (no model call, no marker).
func TestHandleCompactRun_NoopWhenShort(t *testing.T) {
	srv, _ := compactFixture(t)
	ctx := context.Background()
	sess, _ := srv.store.CreateSession(ctx, "", "compactor", "alice")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_s", UserID: "alice", Model: "stub-model"})
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	_ = srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	rec := doJSON(t, srv, "POST", "/v1/runs/"+run.ID+"/compact", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCompact(t, rec.Body.Bytes())
	if out.Compacted || out.Applied != "noop" {
		t.Errorf("response = %+v, want compacted=false applied=noop", out)
	}
}
