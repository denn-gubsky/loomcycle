package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func textTurnOf(text string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// messageShape is a message as role + its blocks, each as type:text-or-id.
func messageShape(m providers.Message) string {
	parts := []string{m.Role}
	for _, c := range m.Content {
		switch c.Type {
		case "tool_use":
			parts = append(parts, "tool_use:"+c.ToolUseID)
		case "tool_result":
			parts = append(parts, "tool_result:"+c.ToolUseID+":"+c.Text)
		default:
			parts = append(parts, c.Type+":"+c.Text)
		}
	}
	return strings.Join(parts, " ")
}

// historySessionRun starts a session with one completed turn through RunOnce
// ("question one" → "answer one"), then writes its second run paused mid-turn:
// the user's "question two", a tool call and its result.
func historySessionRun(t *testing.T, srv *Server) (sessionID string, run store.Run) {
	t.Helper()
	ctx := context.Background()
	if err := srv.RunOnce(ctx, runner.RunInput{Agent: "worker", UserID: "alice", Segments: oneUserSeg("question one")},
		runner.RunCallbacks{OnRegistered: func(_, _, sid, _ string) { sessionID = sid }}); err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sessionID, store.RunIdentity{AgentID: "a_history_resume", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", oneUserSeg("question two"))
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{
		Type:    providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_two", Name: "Read", Input: json.RawMessage(`{}`)},
	})
	appendResumeEvent(t, srv, run.ID, "tool_result", providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_two", Name: "Read"},
		Text:    "progress two",
	})
	return sessionID, run
}

func pauseRun(t *testing.T, srv *Server, run store.Run) {
	t.Helper()
	if err := srv.store.SetRunPauseState(context.Background(), run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
}

// A continuation in an existing session that pauses mid-turn and resumes sends
// the model what RunOnce would have sent it: the session's earlier turns, then
// its own progress. It sent only its own run's events before, so a resumed
// second turn had forgotten the first.
func TestResume_ContinuationReplaysTheSessionsEarlierTurns(t *testing.T) {
	prov := &recordingScriptedProvider{scripts: [][]providers.Event{textTurnOf("answer one")}, defaultS: endTurn()}
	srv := resumeAlignServer(t, prov)
	_, run := historySessionRun(t, srv)
	pauseRun(t, srv, run)
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)

	reqs := prov.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2 (turn 1, the resumed turn 2)", len(reqs))
	}
	var got []string
	for _, m := range reqs[1].Messages {
		got = append(got, messageShape(m))
	}
	want := []string{
		"user text:question one",
		"assistant text:answer one",
		"user text:question two",
		"assistant tool_use:tu_two",
		"user tool_result:tu_two:progress two",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("resumed request messages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The history a resume replays stops at its run's start: a run of the session
// that came after it is not part of its conversation.
func TestResume_ContinuationDoesNotReplayALaterRunOfTheSession(t *testing.T) {
	prov := &recordingScriptedProvider{scripts: [][]providers.Event{textTurnOf("answer one")}, defaultS: endTurn()}
	srv := resumeAlignServer(t, prov)
	sessionID, run := historySessionRun(t, srv)
	ctx := context.Background()
	later, err := srv.store.CreateRun(ctx, sessionID, store.RunIdentity{AgentID: "a_history_later", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, later.ID, "user_input", oneUserSeg("question three"))
	appendResumeEvent(t, srv, later.ID, "text", providers.Event{Type: providers.EventText, Text: "answer three"})
	appendResumeEvent(t, srv, later.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	if err := srv.store.FinishRun(ctx, later.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	pauseRun(t, srv, run)
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)

	reqs := prov.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	text := requestText(reqs[1])
	if strings.Contains(text, "question three") || strings.Contains(text, "answer three") {
		t.Errorf("the resumed run replayed a later run of its session:\n%s", text)
	}
	if !strings.Contains(text, "question one") || !strings.Contains(text, "progress two") {
		t.Errorf("the resumed run lost its own history:\n%s", text)
	}
}

// A stateful continuation that pauses before recording a Σ of its own resumes
// from the session's Σ, as RunOnce started it (statefulContinuationSeed), and
// looks at its own message. It resumed from an empty Σ before, having
// forgotten what the session's earlier turns established.
func TestResume_StatefulContinuationResumesFromTheSessionsState(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"statefulchat": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you chat",
			Context: &config.Context{Mode: &mode}},
	}
	prov := &sigmaFedProvider{}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "statefulchat", "alice")
	if err != nil {
		t.Fatal(err)
	}
	first, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_sf_first", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []store.Event{
		userInputEv(t, "explain DDRAM"),
		markerEv(t, map[string]any{"goal": "explain DDRAM"}, "Echo"),
		callEv(t, "es-act-0", "Echo"),
		resultEv(t, "es-act-0", "Echo", "a capacitor per bit", false),
		markerEv(t, map[string]any{"goal": "explain DDRAM", "found": "capacitors"}, ""),
		textEv(t, "ANSWER ONE: DDRAM stores bits in capacitors"),
	} {
		if err := srv.store.AppendEvent(ctx, first.ID, ev.Type, ev.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.store.FinishRun(ctx, first.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_sf_second", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	ev := userInputEv(t, "and what about refresh?")
	if err := srv.store.AppendEvent(ctx, run.ID, ev.Type, ev.Payload); err != nil {
		t.Fatal(err)
	}
	pauseRun(t, srv, run)
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)

	if !prov.sawFed(`"found":"capacitors"`) {
		t.Errorf("the resumed continuation was not fed the session's Σ:\n%v", prov.fed)
	}
	if !prov.sawFed("and what about refresh?") {
		t.Errorf("the resumed continuation was not fed its own message:\n%v", prov.fed)
	}
}

// The session's last Σ was recorded by an earlier run that ended mid-action
// (cancelled after its action returned, no answer after it). The resumed
// continuation is fed that Σ and its OWN message, as RunOnce starts it
// (statefulContinuationSeed passes the message as fresh). Read from the
// session history with no fresh message, the seed took the earlier run's
// action result as the observation and dropped the continuation's message.
func TestResume_StatefulContinuationAfterAnEarlierRunEndedMidActionKeepsItsMessage(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"statefulchat": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you chat",
			Context: &config.Context{Mode: &mode}},
	}
	prov := &sigmaFedProvider{}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "statefulchat", "alice")
	if err != nil {
		t.Fatal(err)
	}
	first, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_sf_mid_first", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []store.Event{
		userInputEv(t, "explain DDRAM"),
		markerEv(t, map[string]any{"goal": "explain DDRAM", "found": "capacitors"}, "Echo"),
		callEv(t, "es-act-0", "Echo"),
		resultEv(t, "es-act-0", "Echo", "a capacitor per bit", false),
	} {
		if err := srv.store.AppendEvent(ctx, first.ID, ev.Type, ev.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.store.FinishRun(ctx, first.ID, store.RunCancelled, "cancelled", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_sf_mid_second", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	ev := userInputEv(t, "and what about refresh?")
	if err := srv.store.AppendEvent(ctx, run.ID, ev.Type, ev.Payload); err != nil {
		t.Fatal(err)
	}
	pauseRun(t, srv, run)
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)

	if !prov.sawFed(`"found":"capacitors"`) {
		t.Errorf("the resumed continuation was not fed the session's Σ:\n%v", prov.fed)
	}
	if !prov.sawFed("and what about refresh?") {
		t.Errorf("the resumed continuation lost its own message:\n%v", prov.fed)
	}
}
