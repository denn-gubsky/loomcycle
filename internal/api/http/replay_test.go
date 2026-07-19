package http

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func replayFixture(t *testing.T) (*Server, *scriptedProvider) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			// The TARGET agent the replayed session binds; needs a provider/model
			// so the compress path can summarize.
			"chat/dst": {Provider: "scripted", Model: "stub-model", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode — no principal
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "REPLAY SUMMARY"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ := makeServer(t, prov, cfg)
	return srv, prov
}

// seedSourceForReplay builds a source session (under some agent name that need
// NOT exist in cfg — replay reads the SESSION, not the source agent) with a
// multi-turn transcript. The final assistant turn carries reasoning, so the
// strip path is exercised. Returns the source session id.
func seedSourceForReplay(t *testing.T, srv *Server, turns int) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "chat/src", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_src", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	uinput := func(text string) []loop.PromptSegment {
		return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}}
	}
	for i := 1; i <= turns; i++ {
		appendResumeEvent(t, srv, run.ID, "user_input", uinput(fmt.Sprintf("question %d", i)))
		appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: fmt.Sprintf("answer %d", i)})
		// The done event carries provider-specific reasoning that must be stripped
		// on replay (cross-provider safety).
		appendResumeEvent(t, srv, run.ID, "done", providers.Event{
			Type: providers.EventDone, StopReason: "end_turn",
			Reasoning: "SECRET-THINKING", ReasoningSignature: "SIG",
		})
	}
	return sess.ID
}

// TestHandleReplay_CopiesToNewAgentSession is the core: replaying a source
// session under a different target agent mints a NEW session bound to that agent,
// durably seeded with the source transcript (reconstructible on continuation).
func TestHandleReplay_CopiesToNewAgentSession(t *testing.T) {
	srv, _ := replayFixture(t)
	srcID := seedSourceForReplay(t, srv, 3)

	rec := doJSON(t, srv, "POST", "/v1/sessions/"+srcID+"/replay", `{"agent":"chat/dst"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out connector.ReplaySessionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if out.NewSessionID == "" || out.NewSessionID == srcID {
		t.Fatalf("expected a fresh session id, got %q (src %q)", out.NewSessionID, srcID)
	}
	if out.EventsCopied == 0 {
		t.Errorf("no events copied")
	}
	// New session is bound to the TARGET agent.
	ns, err := srv.store.GetSession(context.Background(), out.NewSessionID)
	if err != nil {
		t.Fatalf("get new session: %v", err)
	}
	if ns.Agent != "chat/dst" {
		t.Errorf("new session agent = %q, want chat/dst", ns.Agent)
	}
	// The carried conversation reconstructs from the new session's own transcript.
	events, _ := srv.store.GetTranscript(context.Background(), out.NewSessionID)
	msgs := replayTranscript(events)
	if len(msgs) == 0 {
		t.Fatal("new session transcript did not reconstruct any messages")
	}
	joined := ""
	for _, m := range msgs {
		joined += firstText(m)
	}
	if !strings.Contains(joined, "question 1") || !strings.Contains(joined, "answer 3") {
		t.Errorf("carried conversation missing expected turns: %q", joined)
	}
}

// TestHandleReplay_StripsReasoning: provider-specific reasoning is removed from
// the carried assistant turns so the history is safe under a different-provider
// target. Fail-before: copying `done` events verbatim carried the seal.
func TestHandleReplay_StripsReasoning(t *testing.T) {
	srv, _ := replayFixture(t)
	srcID := seedSourceForReplay(t, srv, 2)

	rec := doJSON(t, srv, "POST", "/v1/sessions/"+srcID+"/replay", `{"agent":"chat/dst"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out connector.ReplaySessionResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	events, _ := srv.store.GetTranscript(context.Background(), out.NewSessionID)
	for _, e := range events {
		if e.Type != "done" {
			continue
		}
		var pe providers.Event
		if json.Unmarshal(e.Payload, &pe) == nil {
			if pe.Reasoning != "" || pe.ReasoningSignature != "" {
				t.Errorf("copied done event still carries reasoning: %q / %q", pe.Reasoning, pe.ReasoningSignature)
			}
		}
	}
}

// TestHandleReplay_Compress: with compress, a compaction marker is appended so the
// carried history replays as [summary + recent tail].
func TestHandleReplay_Compress(t *testing.T) {
	srv, prov := replayFixture(t)
	srcID := seedSourceForReplay(t, srv, 8) // long enough for CompactionSplit to have a middle

	rec := doJSON(t, srv, "POST", "/v1/sessions/"+srcID+"/replay", `{"agent":"chat/dst","compress":true}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out connector.ReplaySessionResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Compacted {
		t.Fatalf("expected compacted=true; got %+v", out)
	}
	if prov.calls.Load() == 0 {
		t.Error("summary provider was never called for the compress path")
	}
	events, _ := srv.store.GetTranscript(context.Background(), out.NewSessionID)
	var found bool
	for _, e := range events {
		if e.Type == string(providers.EventContextCompaction) {
			var pe providers.Event
			if json.Unmarshal(e.Payload, &pe) == nil && pe.ContextCompaction != nil &&
				strings.Contains(pe.ContextCompaction.Summary, "REPLAY SUMMARY") {
				found = true
			}
		}
	}
	if !found {
		t.Error("no context_compaction marker persisted for the compress path")
	}
}

// TestHandleReplay_UnknownSourceIs404: a missing (or cross-tenant) source folds to
// an opaque not-found.
func TestHandleReplay_UnknownSourceIs404(t *testing.T) {
	srv, _ := replayFixture(t)
	rec := doJSON(t, srv, "POST", "/v1/sessions/nonexistent/replay", `{"agent":"chat/dst"}`)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleReplay_UnknownTargetAgentIs409: replaying to an agent that doesn't
// resolve is a 409 (agent_gone).
func TestHandleReplay_UnknownTargetAgentIs409(t *testing.T) {
	srv, _ := replayFixture(t)
	srcID := seedSourceForReplay(t, srv, 2)
	rec := doJSON(t, srv, "POST", "/v1/sessions/"+srcID+"/replay", `{"agent":"chat/does-not-exist"}`)
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleReplay_MissingAgentIs400: the target agent is required.
func TestHandleReplay_MissingAgentIs400(t *testing.T) {
	srv, _ := replayFixture(t)
	srcID := seedSourceForReplay(t, srv, 2)
	rec := doJSON(t, srv, "POST", "/v1/sessions/"+srcID+"/replay", `{}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
