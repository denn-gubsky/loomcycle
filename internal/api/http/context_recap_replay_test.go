package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A context_recap marker (RFC CR L1) must replay like context_compaction: the
// span before the kept tail collapses to the task + the running recap + the last-N
// verbatim, so a rebuild (resume / crash recovery / transcript view) reconstructs
// the SAME distilled fed history the live loop produced — not the full transcript.
//
// Fail-before: replayTranscript had no context_recap case, so a resumed recap-mode
// run replayed the entire history (the distillation was lost on resume).
func TestReplayTranscript_ContextRecapMarker(t *testing.T) {
	srv, _ := replayFixture(t)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "chat/src", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	uinput := func(text string) []map[string]any {
		return []map[string]any{{"role": "user", "content": []map[string]any{{"type": "trusted-text", "text": text}}}}
	}
	// Three turns accumulate: [user(the task), asst(answer1), user(q2), asst(answer2),
	// user(q3), asst(answer3)].
	appendResumeEvent(t, srv, run.ID, "user_input", uinput("the task"))
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "answer1"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	appendResumeEvent(t, srv, run.ID, "user_input", uinput("q2"))
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "answer2"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	appendResumeEvent(t, srv, run.ID, "user_input", uinput("q3"))
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "answer3"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})

	// The recap marker: keep the task (pinned) + the last 2 accumulated messages,
	// replace the middle with the running recap.
	appendResumeEvent(t, srv, run.ID, string(providers.EventContextRecap), providers.Event{
		Type: providers.EventContextRecap,
		ContextRecap: &providers.ContextRecapEventInfo{
			Recap: "THE RUNNING RECAP", KeepN: 2, KeepFirst: true, Trigger: "auto", Reasoning: "recap",
		},
	})

	events, err := srv.store.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := replayTranscript(events)
	if len(msgs) == 0 {
		t.Fatal("replay produced no messages")
	}
	// First turn is the raw pinned task (its own user turn).
	if msgs[0].Role != "user" || strings.TrimSpace(firstText(msgs[0])) != "the task" {
		t.Errorf("msg[0] should be the pinned task, got %q (%s)", firstText(msgs[0]), msgs[0].Role)
	}
	// The recap is present as an assistant turn.
	joined := ""
	sawRecap := false
	for _, m := range msgs {
		joined += firstText(m)
		if m.Role == "assistant" && strings.Contains(firstText(m), "THE RUNNING RECAP") {
			sawRecap = true
		}
	}
	if !sawRecap {
		t.Errorf("the running recap did not replay as an assistant turn: %q", joined)
	}
	// The recapped-away early turn is gone from the fed history (kept only in the
	// retained transcript, not fed).
	if strings.Contains(joined, "answer1") {
		t.Errorf("recapped-away turn 'answer1' leaked into the replayed fed history: %q", joined)
	}
	// The kept tail survived.
	if !strings.Contains(joined, "answer3") {
		t.Errorf("kept tail 'answer3' missing from the replayed history: %q", joined)
	}
}
