package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A transcript replay rebuilds the two turns hook decisions wrote: agent_start
// context in the prompt's user turn, and an agent_stop block's feedback turn.
// Without them a resumed run's history would not be the one the model saw —
// and two assistant turns in a row would be refused by the provider.
func TestReplayTranscript_RebuildsTheTurnsHooksWrote(t *testing.T) {
	events := []store.Event{
		mkEvent("user_input", []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "write the plan"}}}}),
		mkEvent(string(providers.EventHookDecision), providers.Event{Type: providers.EventHookDecision, HookDecision: &providers.HookDecisionInfo{
			Hook: "ops/ctx", Phase: "agent_start", Decision: "context", AdditionalContext: "the user prefers short answers"}}),
		mkEvent("text", providers.Event{Type: providers.EventText, Text: "answer 1"}),
		mkEvent("done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"}),
		mkEvent(string(providers.EventHookDecision), providers.Event{Type: providers.EventHookDecision, HookDecision: &providers.HookDecisionInfo{
			Hook: "ops/check", Phase: "agent_stop", Decision: "block", Reason: "cite a source"}}),
		mkEvent("text", providers.Event{Type: providers.EventText, Text: "answer 2"}),
		mkEvent("done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"}),
	}
	msgs := replayTranscript(events)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	if u := msgs[0]; u.Role != "user" || len(u.Content) != 2 || u.Content[1].Text != "the user prefers short answers" {
		t.Errorf("prompt = %+v", u)
	}
	if b := msgs[2]; b.Role != "user" || firstText(b) != "cite a source" {
		t.Errorf("block turn = %+v", b)
	}
	if msgs[1].Role != "assistant" || msgs[3].Role != "assistant" || firstText(msgs[3]) != "answer 2" {
		t.Errorf("assistant turns = %+v / %+v", msgs[1], msgs[3])
	}
}

// A restored hold keeps the name of the hook that took it.
func TestHeldReviewFrom_KeepsTheHookThatHeld(t *testing.T) {
	h := heldReviewFrom([]store.Event{mkEvent(string(providers.EventAwaitingReview), providers.Event{
		Type: providers.EventAwaitingReview, AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: 2, Round: 1, HeldBy: "ops/hold"}})})
	if h == nil || h.HeldBy != "ops/hold" || h.SinceTurn != 2 {
		t.Fatalf("held = %+v", h)
	}
}

// Through the real server: an agent_stop hook holds a run nobody armed for
// review. Disarming review does not release it — arming did not take it — and
// the operator's verdict on the review verb does.
func TestLifecycleHooks_AHooksHoldWaitsForAVerdictNotADisarm(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"hold","reason":"a person should read this"}`))
	}))
	defer hook.Close()
	h := newReviewHarness(t)
	if _, err := h.srv.testHooks().Register(&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: hook.URL}); err != nil {
		t.Fatal(err)
	}
	runID, _, frames, stop := h.start(`{"agent":"writer","segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
	defer stop()
	h.waitFrame(frames, "awaiting_review")
	h.waitHeld(runID, 1)
	if held, by := heldBy(t.Context(), h.st, runID); !held || by != "ops/hold" {
		t.Fatalf("held = %v by %q", held, by)
	}

	resp, err := http.Post(h.ts.URL+"/v1/runs/"+runID+"/retune", "application/json", strings.NewReader(`{"review":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(150 * time.Millisecond)
	if run, _ := h.st.GetRun(t.Context(), runID); run.Status != store.RunRunning {
		t.Fatalf("disarming review released a hook's hold: status %q", run.Status)
	}

	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("review = %d %s", code, body)
	}
	h.waitStatus(runID, store.RunCompleted)
}

// A paused run restored by resume has already started: its agent_start hooks do
// not run again. Here the hook would deny, so a resume that ran it would fail
// a run that had been allowed to start.
func TestResume_DoesNotRunAgentStartAgain(t *testing.T) {
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"decision":"deny","reason":"no second start"}`))
	}))
	defer hook.Close()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"worker": {Model: "stub-model", Tools: []string{}, SystemPrompt: "work"}}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "resume-start.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	if _, err := srv.testHooks().Register(&hooks.Hook{Owner: "ops", Name: "once", Phase: hooks.PhaseAgentStart, CallbackURL: hook.URL}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_w", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do it"}}},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d (warnings %v)", n, warns)
	}
	waitFor(t, "the resumed run to finish", func() bool {
		got, err := st.GetRun(ctx, run.ID)
		return err == nil && got.Status != store.RunRunning
	})
	if got, _ := st.GetRun(ctx, run.ID); got.Status != store.RunCompleted {
		t.Errorf("status = %s (%q)", got.Status, got.ErrorMsg)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("agent_start ran %d times on resume", n)
	}
}

// A sub-agent whose answer an agent_stop hook holds cannot be held (the Agent
// tool gives it no steer queue), so it ends rejected. Its parent is told so,
// as an error: the refused answer is not handed back as the child's output.
func TestSubAgent_ARejectedChildIsAnErrorToItsParent(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"hold","reason":"a person should read this"}`))
	}))
	defer hook.Close()
	h := newReviewHarness(t)
	if _, err := h.srv.testHooks().Register(&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: hook.URL}); err != nil {
		t.Fatal(err)
	}
	out, _, runID, err := h.srv.runSubAgent(context.Background(), "writer", "", "write the plan", "")
	if err == nil || !strings.Contains(err.Error(), "was rejected") {
		t.Fatalf("output %q, err %v; want the rejection as an error", out, err)
	}
	if run, _ := h.st.GetRun(context.Background(), runID); run.Status != store.RunRejected {
		t.Errorf("child row = %q, want rejected", run.Status)
	}
}
