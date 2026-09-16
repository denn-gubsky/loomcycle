package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// parkedRunFixture stands up a server holding ONE paused run whose transcript
// ends on an ASSISTANT turn — the shape of a chat that was sitting idle waiting
// for its operator when the runtime paused.
func parkedRunFixture(t *testing.T, interactive bool) (*Server, *httptest.Server, *recordingScriptedProvider, store.Run) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"chatty": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you chat"},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "parked.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetSteerRegistry(steer.NewRegistry(0)) // parking IS blocking on this queue
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "chatty", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_parked", UserID: "alice", Model: "stub-model", Interactive: interactive,
	})
	if err != nil {
		t.Fatal(err)
	}

	// user turn, then the assistant's reply — the conversation ends with the
	// model having spoken and nothing pending.
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{
		Type: providers.EventText, Text: "hello — what would you like to do?",
	})
	// The assistant turn is only committed by its `done` event — replayTranscript
	// flushes there. Without it the reply stays unflushed and the conversation
	// replays as a bare user turn, which is a DIFFERENT case than the one under
	// test (and is what this fixture got wrong first time round).
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{
		Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	return srv, ts, prov, run
}

// waitFor polls until cond holds, or fails with msg.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// RFC DD Gap 3. A run parked awaiting its operator used to be REFUSED on
// resume and marked failed — "your conversation is marked failed, re-attach and
// steer" for the chat someone was in the middle of. It is now restored to what
// it was actually doing: waiting.
//
// The assertion that matters is not that the resume returned 1. It is that the
// run parks WITHOUT calling the provider (a run handed a conversation with
// nothing to answer would send the model a trailing assistant turn), and then
// continues on the operator's next turn through the ordinary steer endpoint.
func TestResume_IdleInteractiveRunParksInsteadOfFailing(t *testing.T) {
	srv, ts, prov, run := parkedRunFixture(t, true)
	ctx := context.Background()

	n, warns := srv.ResumePausedRuns(ctx)
	if n != 1 {
		t.Fatalf("resumed %d runs, want 1 (warnings: %v) — the parked chat was refused", n, warns)
	}

	// It is parked: an awaiting_input marker reached the transcript, and the
	// model was NOT called.
	waitFor(t, "the resumed run to park at awaiting_input", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_input")
	})
	if got := prov.requests(); len(got) != 0 {
		t.Fatalf("a parked run called the provider %d time(s) — it was handed a "+
			"conversation ending on an assistant turn with nothing to answer", len(got))
	}

	// It did not fail.
	got, err := srv.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == store.RunFailed {
		t.Errorf("run status = %s (%q), want a live parked run", got.Status, got.ErrorMsg)
	}

	// And it continues on the operator's next turn, through the endpoint an
	// operator actually uses.
	resp, err := http.Post(ts.URL+"/v1/runs/"+run.ID+"/input", "application/json",
		strings.NewReader(`{"text":"summarise what we said"}`))
	if err != nil {
		t.Fatalf("steer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		t.Fatalf("steer status = %d — the resumed run was not re-attachable", resp.StatusCode)
	}

	waitFor(t, "the resumed run to act on the operator's turn", func() bool {
		return len(prov.requests()) > 0
	})
	req := prov.requests()[0]
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || !strings.Contains(contentText(last), "summarise what we said") {
		t.Errorf("the continued turn did not carry the operator's message; last message = %+v", last)
	}
	// The pre-pause conversation is still there — it resumed the chat, not started one.
	if !strings.Contains(messagesText(req.Messages), "hello — what would you like to do?") {
		t.Error("the resumed turn lost the conversation it was parked in the middle of")
	}
}

// The carve-out, pinned deliberately: a NON-interactive run has no operator to
// wait for, so parking it would swap a loud failure for a run that idles
// forever holding a concurrency slot. It stays refused.
func TestResume_IdleNonInteractiveRunStillRefuses(t *testing.T) {
	srv, _, prov, run := parkedRunFixture(t, false)
	ctx := context.Background()

	n, warns := srv.ResumePausedRuns(ctx)
	if n != 0 {
		t.Errorf("re-dispatched %d non-interactive run(s), want 0", n)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "no pending turn") {
		t.Errorf("warnings = %v, want one naming the missing pending turn", warns)
	}
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for a refused run", len(got))
	}
	got, err := srv.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunFailed {
		t.Errorf("status = %s, want failed — a refused run must not stay a running zombie", got.Status)
	}
}

// Parking means BLOCKING ON THE STEER QUEUE, and makeSteer hands back a nil
// queue when no registry is wired — the loop then skips the park it has no
// queue for. So "interactive" alone is not enough to park on: without the
// registry the run would sail past into a model call carrying a trailing
// assistant turn, which is the malformed request the refusal exists to prevent.
//
// This is not hypothetical. The first version of this change guarded on
// run.Interactive alone; the fixture happened to omit the registry and the run
// silently called the provider instead of parking. Refuse loudly instead.
func TestResume_IdleInteractiveRunWithNoSteerRegistryStillRefuses(t *testing.T) {
	srv, _, prov, run := parkedRunFixture(t, true)
	srv.SetSteerRegistry(nil)

	n, warns := srv.ResumePausedRuns(context.Background())
	if n != 0 {
		t.Errorf("re-dispatched %d run(s) with no steer queue to park on, want 0", n)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "no pending turn") {
		t.Errorf("warnings = %v, want one naming the missing pending turn", warns)
	}
	// The point of the refusal: the provider is never handed a conversation
	// ending on an assistant turn.
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) with a trailing assistant turn", len(got))
	}
	got, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
}

// The other exit from a start-park: the operator cancels instead of replying.
//
// Covers the parkAbandoned branch, which nothing else reaches — deleting it
// only broke the build, and a compile error is not coverage. The run must reach
// a terminal state on the end_turn it had already reached before the pause,
// WITHOUT the model ever being called: it never had a turn to answer.
func TestResume_ParkedRunCancelledWhileWaitingEndsCleanly(t *testing.T) {
	srv, ts, prov, run := parkedRunFixture(t, true)
	ctx := context.Background()

	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the resumed run to park", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_input")
	})

	resp, err := http.Post(ts.URL+"/v1/agents/"+run.AgentID+"/cancel", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	resp.Body.Close()

	waitFor(t, "the cancelled parked run to reach a terminal state", func() bool {
		got, err := srv.store.GetRun(ctx, run.ID)
		return err == nil && got.Status != store.RunRunning
	})
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for a run that was cancelled "+
			"while waiting and never had a turn to answer", len(got))
	}
}

// A paused run with a PENDING turn is unaffected: it re-enters the loop and
// answers, exactly as before. Without this the two tests above would pass on an
// implementation that parked everything.
func TestResume_RunWithAPendingTurnStillRunsImmediately(t *testing.T) {
	srv, _, prov, run := parkedRunFixture(t, true)

	// Append an operator turn, so the conversation now ends on a user message.
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "carry on"}}},
	})

	if n, warns := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the run to answer its pending turn without parking first", func() bool {
		return len(prov.requests()) > 0
	})
}

// contentText flattens a message's text blocks.
func contentText(m providers.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
		b.WriteString(" ")
	}
	return b.String()
}

func messagesText(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(contentText(m))
		b.WriteString("\n")
	}
	return b.String()
}

var _ = json.Marshal
