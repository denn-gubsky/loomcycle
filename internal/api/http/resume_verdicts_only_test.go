package http

import (
	"context"
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

// heldResumeKind is which run a paused held row is: a top-level run, a child
// the Agent tool spawned, or a team-walk member.
type heldResumeKind int

const (
	heldTopLevel heldResumeKind = iota
	heldAgentToolChild
	heldWalkMember
)

// heldKindFixture is heldRunFixture for a run of the given kind: a paused run
// held for review, whose conversation is two turns long so a compaction of it
// is not a no-op for want of history.
func heldKindFixture(t *testing.T, kind heldResumeKind) (*Server, *httptest.Server, *recordingScriptedProvider, store.Run) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"writer": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you write"},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "held-kind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	ident := store.RunIdentity{
		AgentID: "a_held", UserID: "alice", Model: "stub-model",
		RunConfig: runConfigRecord{Review: reviewRecord(true)}.marshal(),
	}
	if kind != heldTopLevel {
		psess, err := st.CreateSession(ctx, "", "writer", "alice")
		if err != nil {
			t.Fatal(err)
		}
		parent, err := st.CreateRun(ctx, psess.ID, store.RunIdentity{AgentID: "a_parent", UserID: "alice", Model: "stub-model"})
		if err != nil {
			t.Fatal(err)
		}
		ident.ParentRunID, ident.ParentAgentID = parent.ID, parent.AgentID
	}
	if kind == heldWalkMember {
		ident.ParentContext = &store.ParentContext{WalkID: "w1", State: "draft", StateVisit: 1}
	}
	sess, err := st.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, ident)
	if err != nil {
		t.Fatal(err)
	}
	turn := func(prompt, answer string) {
		appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: prompt}}},
		})
		appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: answer})
		appendResumeEvent(t, srv, run.ID, "done", providers.Event{
			Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{},
		})
	}
	turn("survey the auth flow", strings.Repeat("the auth flow goes through the gateway, then the token service. ", 40))
	turn("write the plan", "the held plan")
	appendResumeEvent(t, srv, run.ID, "awaiting_review", providers.Event{
		Type: providers.EventAwaitingReview, AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: 0, Round: 1},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	return srv, ts, prov, run
}

// resumeHeld resumes the fixture's one paused run and waits until it is held
// again.
func resumeHeld(t *testing.T, srv *Server, run store.Run) {
	t.Helper()
	if n, warns := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the resumed run to be held again", func() bool {
		return strings.Count(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_review") >= 2
	})
}

// A child the Agent tool spawned takes a verdict and nothing else while it
// runs — its parent drives it. A resume used to register it a full steer
// entry, so after a pause an operator could steer and retune a child the
// parent was still driving. It comes back as it ran: a steer and a retune are
// refused as if it were not live, and a verdict still ends the hold.
func TestResume_AnAgentToolChildStillTakesOnlyAVerdict(t *testing.T) {
	srv, ts, prov, run := heldKindFixture(t, heldAgentToolChild)
	resumeHeld(t, srv, run)

	if code, body := postInput(t, ts, run.ID, `{"text":"focus on the token service"}`); code != 404 {
		t.Errorf("steer of a resumed Agent-tool child: status %d, want 404: %s", code, body)
	}
	if code, body := postRetune(t, ts, run.ID, `{"max_tokens":512}`); code != 404 {
		t.Errorf("retune of a resumed Agent-tool child: status %d, want 404: %s", code, body)
	}

	if code := postReview(t, ts, run.ID, `{"decision":"approve"}`); code != 200 {
		t.Fatalf("verdict on a resumed Agent-tool child: status %d, want 200", code)
	}
	ctx := context.Background()
	waitFor(t, "the approved child to complete", func() bool {
		r, _ := srv.store.GetRun(ctx, run.ID)
		return r.Status == store.RunCompleted
	})
	if r, _ := srv.store.GetRun(ctx, run.ID); !strings.Contains(string(r.Result), "the held plan") {
		t.Errorf("result = %s, want the answer that was under review", r.Result)
	}
	if n := len(prov.requests()); n != 0 {
		t.Errorf("the provider was called %d time(s) — something other than the verdict reached the child", n)
	}
}

// The derivation must not reach the runs an operator does drive: a top-level
// run and a team-walk member (whose row carries its walk's id) come back with
// the full entry they had live.
func TestResume_ATopLevelRunAndAWalkMemberStillTakeASteer(t *testing.T) {
	for _, c := range []struct {
		name string
		kind heldResumeKind
	}{
		{"top-level", heldTopLevel},
		{"walk member", heldWalkMember},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, ts, prov, run := heldKindFixture(t, c.kind)
			resumeHeld(t, srv, run)
			if code, body := postRetune(t, ts, run.ID, `{"max_tokens":512}`); code != 200 {
				t.Errorf("retune: status %d, want 200: %s", code, body)
			}
			if code, body := postInput(t, ts, run.ID, `{"text":"cover the rollback"}`); code != 200 && code != 202 {
				t.Fatalf("steer: status %d, want accepted: %s", code, body)
			}
			// A message on a held run is feedback: it reaches the model.
			waitFor(t, "the steer to reach the model", func() bool { return len(prov.requests()) > 0 })
			if last := prov.requests()[0].Messages; !strings.Contains(contentText(last[len(last)-1]), "cover the rollback") {
				t.Errorf("last message = %+v, want the steer", last[len(last)-1])
			}
		})
	}
}
