package http

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Two differences between a session continuation (RunOnce) and a resume were
// left to evidence before any change (owner ruling, 2026-11-11). These tests
// record what each path does today; neither path is changed.

// runUntrustedProbe records the untrusted input its run says it started with
// (tools.RunUntrusted), which a code-js agent reads as input.untrusted.
type runUntrustedProbe struct {
	mu   sync.Mutex
	seen [][]tools.UntrustedInput
}

func (p *runUntrustedProbe) Name() string        { return "UProbe" }
func (p *runUntrustedProbe) Description() string { return "" }
func (p *runUntrustedProbe) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (p *runUntrustedProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.RunUntrusted(ctx))
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

func untrustedTurn(text, page string) []loop.PromptSegment {
	return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
		{Type: "trusted-text", Text: text},
		{Type: "untrusted-block", Kind: "web_content", Text: page},
	}}}
}

// requestText is every message of a model request as text.
func requestText(req providers.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(contentText(m))
		b.WriteString("\n")
	}
	return b.String()
}

// #30. A session's first run read page "alpha"; the second run of the session
// starts with page "beta". Continued through RunOnce, or resumed after a pause,
// the second run's own untrusted input is "beta" alone: RunOnce leaves
// RunOptions.Untrusted nil and the loop reads it from the new segments, the
// resume rebuilds it from the run's stored user_input. Neither treats the
// earlier run's page as this run's input.
//
// What the model is sent differs, and that is the finding: the continuation
// replays the whole session, so "alpha" reaches the model, fenced as untrusted
// as it was when it was read; the resume replays only the run's own events, so
// the session's earlier turns are not in its conversation at all.
func TestEvidence_UntrustedInputOfAContinuationAndOfAResume(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"worker": {Model: "stub-model", Tools: []string{"UProbe"}, SystemPrompt: "role:worker."}}
	prov := &recordingScriptedProvider{scripts: [][]providers.Event{
		endTurn(),                                          // run 1
		toolCallTurn("tu_cont", "UProbe", `{}`), endTurn(), // run 2, continued
		toolCallTurn("tu_res", "UProbe", `{}`), endTurn(), // run 2, resumed
	}, defaultS: endTurn()}
	probe := &runUntrustedProbe{}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{probe}, concurrency.New(8, 8, time.Second), st)

	var sessionID string
	if err := srv.RunOnce(context.Background(), runner.RunInput{Agent: "worker", UserID: "alice", Segments: untrustedTurn("first", "alpha")},
		runner.RunCallbacks{OnRegistered: func(_, _, sid, _ string) { sessionID = sid }}); err != nil {
		t.Fatal(err)
	}
	if err := srv.RunOnce(context.Background(), runner.RunInput{SessionID: sessionID, UserID: "alice", Segments: untrustedTurn("second", "beta")}, runner.RunCallbacks{}); err != nil {
		t.Fatal(err)
	}
	reqs := prov.requests()
	contReq := requestText(reqs[1])

	ctx := context.Background()
	run, err := st.CreateRun(ctx, sessionID, store.RunIdentity{AgentID: "a_evidence_resume", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", untrustedTurn("second", "beta"))
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)
	resReq := requestText(prov.requests()[3])

	probe.mu.Lock()
	seen := probe.seen
	probe.mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("probe calls = %d, want 2", len(seen))
	}
	want := []tools.UntrustedInput{{Kind: "web_content", Text: "beta"}}
	for i, path := range []string{"continuation", "resume"} {
		if !equalJSON(seen[i], want) {
			t.Errorf("%s: the run's untrusted input = %+v, want %+v", path, seen[i], want)
		}
	}
	if !strings.Contains(contReq, "alpha") || !strings.Contains(contReq, "<web_content>") {
		t.Errorf("continuation: the earlier page is not in the replayed conversation, fenced:\n%s", contReq)
	}
	if strings.Contains(resReq, "alpha") || strings.Contains(resReq, "first") {
		t.Errorf("resume: the session's earlier turns are in the resumed conversation (today they are not):\n%s", resReq)
	}
	if !strings.Contains(resReq, "<web_content>") {
		t.Errorf("resume: the run's own page is not fenced as untrusted:\n%s", resReq)
	}
}

// errorEventsOf is the error events a run recorded, by text.
func errorEventsOf(t *testing.T, st store.Store, run store.Run) []string {
	t.Helper()
	var out []string
	for _, p := range eventsOf(t, st, run, string(providers.EventError)) {
		var ev providers.Event
		if err := json.Unmarshal([]byte(p), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev.Error)
	}
	return out
}

// #34. A run whose model call fails. Started through RunOnce, the loop records
// its own error event and RunOnce hands the error to the caller (OnRunError),
// which is not recorded: one error event. Resumed, the loop records its own
// and the resume records runErrorEvent too: the same failure twice. A detached
// RunOnce run (startDetachedLoop) records the second copy only when it says
// something the loop's did not (lastErrorEvent).
func TestEvidence_ErrorEventsOfAFailedRunAndOfAFailedResume(t *testing.T) {
	prov := &failingProvider{err: errors.New("boom")}
	srv := resumeAlignServer(t, prov)

	var agentID string
	_ = srv.RunOnce(context.Background(), runner.RunInput{Agent: "worker", UserID: "alice", Segments: oneUserSeg("go")},
		runner.RunCallbacks{OnRegistered: func(aid, _, _, _ string) { agentID = aid }})
	fresh, err := srv.store.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, srv.store, fresh.ID, store.RunFailed)

	run := pausedRunRow(t, srv, "worker", "a_evidence_failed_resume", false, oneUserSeg("go"))
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunFailed)

	started, resumed := errorEventsOf(t, srv.store, fresh), errorEventsOf(t, srv.store, run)
	t.Logf("RunOnce: %q", started)
	t.Logf("resume:  %q", resumed)
	if len(started) != 1 {
		t.Errorf("RunOnce recorded %d error events, want 1 (the loop's own)", len(started))
	}
	if len(resumed) != 2 || resumed[0] != resumed[1] {
		t.Errorf("resume recorded %q, want the loop's error event and the same text again", resumed)
	}
}
