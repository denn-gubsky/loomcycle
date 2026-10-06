package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// walkCross answers the lead from a script and a team member by its input:
// "slow" waits for gate, then calls a tool — the boundary after it is where a
// pause parks it — and answers "result of slow" on any later call; anything
// else is answered at once with "result of <input>".
type walkCross struct {
	mu     sync.Mutex
	script []func(providers.Request) []providers.Event
	calls  []providers.Request
	gate   chan struct{}
}

func (p *walkCross) ID() string                  { return "stub" }
func (p *walkCross) Probe(context.Context) error { return nil }
func (p *walkCross) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *walkCross) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *walkCross) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		input := req.Messages[0].Content[0].Text
		switch {
		case !strings.Contains(input, "slow"):
			events = answer("result of " + input)
		case len(req.Messages) == 1 && p.gate != nil:
			select {
			case <-p.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			events = toolCallEv("tu_member", "Nope", `{}`)
		default:
			events = answer("result of slow")
		}
	} else {
		p.mu.Lock()
		i := len(p.calls)
		p.calls = append(p.calls, req)
		p.mu.Unlock()
		if i >= len(p.script) {
			return nil, context.Canceled
		}
		events = p.script[i](req)
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *walkCross) leadCalls() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.calls...)
}

// End to end across two instances. On A a lead runs two walks in poll mode:
// one ends at once, the other's member is still working when A is paused and
// snapshotted. On B, the lead's TeamDef poll reads the walk that ended — its
// whole answer, steps included, from its parent's ledger — and the walk that
// did not, which a snapshot cannot carry (it runs no loop of its own), as
// failed with why: it was interrupted and must be run again. Before, it read
// as a run that "never started or was not carried over".
func TestResumePausedRuns_AWalkReadsItsAnswerOrWhyItWasInterruptedOnAnotherInstance(t *testing.T) {
	ctx := context.Background()
	provA := &walkCross{gate: make(chan struct{}), script: []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"fast","mode":"poll"}`)),
		fixed(toolCallEv("tu_2", "TeamDef", `{"op":"run","name":"rev","input":"slow","mode":"poll"}`)),
		fixed(answer("waiting for the slow walk")),
	}}
	srvA, _ := newPollWalkServer(t, provA)
	srvA.SetPauseManager(pause.NewManager(srvA.store, 5*time.Second))
	tsA := httptest.NewServer(srvA.Mux())
	defer tsA.Close()
	defer func() {
		cancelAllRuns(srvA)
		for deadline := time.Now().Add(10 * time.Second); srvA.cancelReg.Count() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(provA.gate) }) }
	defer openGate()
	_ = startLead(t, tsA)

	leadAgent, leadID, on := leadWaiting(t, srvA)
	lead := store.Run{ID: leadID, SessionID: sessionOf(t, srvA, leadID)}
	var fastWalk, slowWalk string
	waitFor(t, "the fast walk's end to be recorded on the lead", func() bool {
		for _, ev := range leadEvents(t, srvA, lead, providers.EventSpawnChildResult) {
			fastWalk = ev.SpawnChild.RunID
		}
		return fastWalk != ""
	})
	for _, id := range strings.Split(on, ", ") {
		if id != fastWalk {
			slowWalk = id
		}
	}
	if slowWalk == "" || strings.Count(on, "r_") > 2 {
		t.Fatalf("the lead waits on %q with %q ended, want the slow walk", on, fastWalk)
	}

	paused := make(chan error, 1)
	go func() {
		_, err := srvA.pauseMgr.Pause(ctx, 5*time.Second)
		paused <- err
	}()
	waitFor(t, "the pause to be declared on A", func() bool { return srvA.pauseMgr.State() != pause.StateRunning })
	openGate() // the slow walk's member reaches its next boundary, where the pause parks it
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the lead to be paused on A", func() bool {
		run, err := srvA.store.GetRun(ctx, leadID)
		return err == nil && run.PauseState == store.PauseStatePaused
	})
	var member string
	for _, parentAgent := range []string{leadAgent, "team:rev"} {
		runs, _ := srvA.store.ListRunsByParentAgentID(ctx, parentAgent)
		for _, r := range runs {
			if r.ParentRunID == slowWalk {
				member = r.ID
			}
		}
	}
	waitFor(t, "the slow walk's member to be paused on A", func() bool {
		run, err := srvA.store.GetRun(ctx, member)
		return err == nil && run.PauseState == store.PauseStatePaused
	})
	_, raw, err := snapshot.Capture(ctx, srvA.store, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	provB := &walkCross{script: []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_3", "TeamDef", `{"op":"poll"}`)),
		fixed(answer("read them")),
	}}
	srvB, _ := newPollWalkServer(t, provB)
	t.Cleanup(func() {
		cancelAllRuns(srvB)
		for deadline := time.Now().Add(10 * time.Second); srvB.cancelReg.Count() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	})
	reqBody, _ := json.Marshal(map[string]any{"json": json.RawMessage(raw)})
	req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(reqBody))
	req.SetPathValue("id", "inline")
	rec := httptest.NewRecorder()
	srvB.handleRestoreSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore on B: status %d: %s", rec.Code, rec.Body.String())
	}

	waitWalkRunStatus(t, srvB.store, leadID, store.RunCompleted)
	calls := provB.leadCalls()
	if len(calls) != 2 {
		t.Fatalf("the lead made %d calls on B, want 2", len(calls))
	}
	if wake := lastText(calls[0]); !strings.Contains(wake, slowWalk+" (team:rev): failed") {
		t.Errorf("wake note on B = %q, want the slow walk ended", wake)
	}
	var polled struct {
		Walks []struct {
			RunID string `json:"run_id"`
			State string `json:"state"`
			Error string `json:"error"`
		} `json:"walks"`
	}
	text := lastToolText(calls[1])
	if err := json.Unmarshal([]byte(text), &polled); err != nil {
		t.Fatalf("TeamDef poll on B = %s: %v", text, err)
	}
	for _, c := range polled.Walks {
		switch c.RunID {
		case fastWalk:
			if c.State != "completed" || !strings.Contains(text, `"steps":[`) || !strings.Contains(text, "result of fast") {
				t.Errorf("the fast walk on B = %+v, want its whole answer (in %s)", c, text)
			}
		case slowWalk:
			if c.State != "failed" || !strings.Contains(c.Error, "the walk was interrupted") || !strings.Contains(c.Error, "run it again") {
				t.Errorf("the slow walk on B = %+v, want failed as interrupted", c)
			}
		}
	}
	if len(polled.Walks) != 2 {
		t.Errorf("TeamDef poll on B = %s, want both walks", text)
	}
	// The slow walk's member travelled (it was paused); its walk did not.
	// Nothing would read it, so it is not resumed on B.
	ended, err := srvB.store.GetRun(ctx, member)
	if err != nil || ended.Status != store.RunCancelled || !strings.Contains(ended.StopReason, "its team walk "+slowWalk+" is not here") {
		t.Errorf("the slow walk's member on B = %+v, %v; want cancelled, its walk gone", ended, err)
	}
}

// On the same database — the instance restarted — a walk still running when
// its parent paused left its run row running, with no walk behind it. The
// parent's resume closes the row as interrupted, so the run reads what
// happened rather than running until the stale sweeper fails it, and the
// parent reads the same reason.
func TestResumePausedRuns_AWalkLeftRunningByARestartIsClosedAsInterrupted(t *testing.T) {
	ctx := context.Background()
	prov := &walkCross{script: []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_2", "TeamDef", `{"op":"poll","wait":"all","wait_ms":5000}`)),
		fixed(answer("read it")),
	}}
	srv, st := newPollWalkServer(t, prov)
	t.Cleanup(func() {
		cancelAllRuns(srv)
		for deadline := time.Now().Add(10 * time.Second); srv.cancelReg.Count() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	})
	sess, err := st.CreateSession(ctx, "", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_lead", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	wsess, err := st.CreateSession(ctx, "", "team:rev", "alice")
	if err != nil {
		t.Fatal(err)
	}
	walk, err := st.CreateRun(ctx, wsess.ID, store.RunIdentity{AgentID: "team:rev", UserID: "alice", ParentRunID: lead.ID})
	if err != nil {
		t.Fatal(err)
	}
	member := memberRun(t, srv, walk, "a_member", "slow member")
	appendResumeEvent(t, srv, lead.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
	})
	appendResumeEvent(t, srv, lead.ID, "tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "TeamDef", Input: json.RawMessage(`{"op":"run","name":"rev","mode":"poll"}`)}})
	appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_1", RunID: walk.ID, Agent: "team:rev", Mode: "poll", Kind: "team", Team: "rev"}})
	appendResumeEvent(t, srv, lead.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}})
	appendResumeEvent(t, srv, lead.ID, "tool_result", providers.Event{Type: providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "TeamDef"}, Text: `{"run_id":"` + walk.ID + `"}`})
	markPaused(t, srv, lead)

	// The walk is not paused, so the member is not ordered before the lead:
	// the lead's resume closes the walk first, and the member is not resumed.
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want the lead (warnings: %v)", n, warns)
	}
	ended, err := st.GetRun(ctx, walk.ID)
	if err != nil || ended.Status != store.RunFailed || !strings.Contains(ended.ErrorMsg, "the walk was interrupted") {
		t.Fatalf("the walk's row after the resume = %+v, %v; want failed as interrupted", ended, err)
	}
	waitWalkRunStatus(t, st, lead.ID, store.RunCompleted)
	if polled := lastToolText(prov.leadCalls()[1]); !strings.Contains(polled, `"state":"failed"`) || !strings.Contains(polled, "the walk was interrupted") {
		t.Errorf("TeamDef poll = %s, want the walk failed as interrupted", polled)
	}
	// Its member is not left running for a walk that will never read it.
	gone := waitWalkRunStatus(t, st, member.ID, store.RunCancelled)
	if !strings.Contains(gone.StopReason, "its parent run "+walk.ID+" ended (failed)") {
		t.Errorf("the member ended %q, want cancelled naming its walk's end", gone.StopReason)
	}
}
