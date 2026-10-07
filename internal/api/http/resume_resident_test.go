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
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// residentFamily answers the lead from a script of functions and a child by
// its latest message: "first" waits for gate (when set), and every message is
// answered "reply to <message>".
type residentFamily struct {
	mu     sync.Mutex
	script []func(providers.Request) []providers.Event
	calls  []providers.Request
	gate   chan struct{}
	// capOn, when set, makes a child told it call a tool on every turn after
	// it, until its iteration limit ends it (its closing turn answers).
	capOn string
}

func (p *residentFamily) ID() string                  { return "stub" }
func (p *residentFamily) Probe(context.Context) error { return nil }
func (p *residentFamily) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *residentFamily) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *residentFamily) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		last := lastText(req)
		if last == "first" && p.gate != nil {
			select {
			case <-p.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		events = answer("reply to " + last)
		if p.capOn != "" && !closingTurnAsked(req) && requestMentions(req, p.capOn) {
			events = toolCallEv("tu_more", "Nope", `{}`)
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

// requestMentions reports whether any text block of req contains want.
func requestMentions(req providers.Request, want string) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, want) {
				return true
			}
		}
	}
	return false
}

func (p *residentFamily) leadCalls() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.calls...)
}

// End to end across two instances. On A a lead opens a resident child; the
// runtime is paused while the child's first turn runs — the child parks
// between turns and the lead at its next boundary — and snapshotted. B
// restores the snapshot and resumes both: the child comes back as a resident
// child, parked, in B's registry under its run id, so the lead's send reaches
// it, its reply comes back, a poll naming it by child_run_ids knows it, and
// close ends it. Before, it came back as a failed run nobody could reach.
func TestResumePausedRuns_AResidentChildIsReachableByItsParentAfterTheResume(t *testing.T) {
	ctx := context.Background()
	provA := &residentFamily{gate: make(chan struct{}), script: []func(providers.Request) []providers.Event{
		func(providers.Request) []providers.Event {
			return agentCall("tu_1", `{"op":"open","name":"worker","prompt":"first"}`)
		},
	}}
	srvA, _ := makeServer(t, provA, bgResumeConfig(""))
	srvA.SetSteerRegistry(steer.NewRegistry(0))
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
	_ = postLead(t, tsA)

	var childID, leadID string
	waitFor(t, "the resident child to be opened on A", func() bool {
		for _, info := range srvA.residentReg.listInfo() {
			childID = info.ChildRunID
		}
		return childID != ""
	})
	child, err := srvA.store.GetRun(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	leadID = child.ParentRunID

	paused := make(chan error, 1)
	go func() {
		_, err := srvA.pauseMgr.Pause(ctx, 5*time.Second)
		paused <- err
	}()
	waitFor(t, "the pause to be declared on A", func() bool { return srvA.pauseMgr.State() != pause.StateRunning })
	openGate() // the child ends its first turn and parks; the lead reaches its next boundary
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{leadID, childID} {
		waitFor(t, "run "+id+" to be paused on A", func() bool {
			run, err := srvA.store.GetRun(ctx, id)
			return err == nil && run.PauseState == store.PauseStatePaused
		})
	}
	_, raw, err := snapshot.Capture(ctx, srvA.store, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	provB := &residentFamily{script: []func(providers.Request) []providers.Event{
		func(providers.Request) []providers.Event {
			return agentCall("tu_2", `{"op":"send","child_run_id":"`+childID+`","prompt":"second"}`)
		},
		func(providers.Request) []providers.Event {
			return agentCall("tu_3", `{"op":"poll","child_run_ids":["`+childID+`"]}`)
		},
		func(providers.Request) []providers.Event {
			return agentCall("tu_4", `{"op":"close","child_run_id":"`+childID+`"}`)
		},
		func(providers.Request) []providers.Event { return answer("done") },
	}}
	srvB, _ := makeServer(t, provB, bgResumeConfig(""))
	srvB.SetSteerRegistry(steer.NewRegistry(0))
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
	var restored snapshotRestoreResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &restored)
	if rec.Code != http.StatusOK || restored.PausedRunsResumed != 2 {
		t.Fatalf("restore on B: status %d, resumed %d, want the lead and its resident child: %s", rec.Code, restored.PausedRunsResumed, rec.Body.String())
	}

	waitWalkRunStatus(t, srvB.store, leadID, store.RunCompleted)
	calls := provB.leadCalls()
	if len(calls) != 4 {
		t.Fatalf("the lead made %d calls on B, want 4", len(calls))
	}
	if sent := lastToolText(calls[1]); !strings.Contains(sent, "reply to second") || !strings.Contains(sent, `"state":"awaiting_input"`) {
		t.Errorf("send on B = %s, want the child's reply, parked again", sent)
	}
	if polled := lastToolText(calls[2]); strings.Contains(polled, "not a child of this run") {
		t.Errorf("poll by child_run_ids on B = %s, want the child known as the lead's", polled)
	}
	if closed := lastToolText(calls[3]); !strings.Contains(closed, `"state":"closed"`) {
		t.Errorf("close on B = %s", closed)
	}
	ended := waitWalkRunStatus(t, srvB.store, childID, store.RunCancelled)
	if !strings.Contains(ended.StopReason+ended.ErrorMsg, "closed by parent") {
		t.Errorf("the child ended %q / %q, want closed by its parent", ended.StopReason, ended.ErrorMsg)
	}
	waitFor(t, "the child to leave B's resident registry", func() bool {
		_, ok := srvB.residentReg.get(childID)
		return !ok
	})
	if n := srvB.liveChildren.Alive(leadID); n != 0 {
		t.Errorf("the lead has %d live children after closing its only one", n)
	}
}

// A resume pass resumes every paused run after its paused descendants, and
// otherwise in the order listed: a parent's first turn may address a resident
// child by id, which finds it only once the child's own resume registered it.
func TestChildrenFirst_PutsEveryRunAfterItsPausedDescendants(t *testing.T) {
	runs := []store.Run{
		{ID: "lead"},
		{ID: "other"},
		{ID: "child", ParentRunID: "lead"},
		{ID: "grandchild", ParentRunID: "child"},
		{ID: "orphan", ParentRunID: "gone"}, // its parent is not paused
		{ID: "loop_a", ParentRunID: "loop_b"},
		{ID: "loop_b", ParentRunID: "loop_a"}, // corrupt rows must not hang the pass
	}
	var got []string
	for _, r := range childrenFirst(runs) {
		got = append(got, r.ID)
	}
	pos := map[string]int{}
	for i, id := range got {
		pos[id] = i
	}
	if len(got) != len(runs) || pos["grandchild"] > pos["child"] || pos["child"] > pos["lead"] || pos["lead"] > pos["other"] {
		t.Errorf("order = %v, want grandchild before child before lead, and lead before other as listed", got)
	}
}

// A resident child resumed parked announces the park it was already in when
// its loop starts. A send its parent made before then is not ended by that
// announcement: it ends with the turn the send started, and that turn's reply.
func TestResidentChild_ResumedParkedSendIsNotEndedByTheStartupPark(t *testing.T) {
	rc := &residentChild{runID: "r_child"}
	rc.parked(time.Now())
	done := rc.beginTurn(time.Now()) // the parent's send, before the loop parks
	rc.observe(providers.Event{Type: providers.EventAwaitingInput})
	select {
	case <-done:
		t.Fatal("the send's turn ended on the startup park")
	default:
	}
	rc.observe(providers.Event{Type: providers.EventText, Text: "reply"})
	rc.observe(providers.Event{Type: providers.EventAwaitingInput})
	select {
	case <-done:
	default:
		t.Fatal("the send's turn did not end when the child parked after it")
	}
	if out, state := rc.readTurn(); out != "reply" || state != "awaiting_input" {
		t.Errorf("turn = %q, %q; want the reply, parked", out, state)
	}
}

// pauseFlips records the runs a resume flips back to running, in order.
type pauseFlips struct {
	store.Store
	mu  sync.Mutex
	ids []string
}

func (p *pauseFlips) SetRunPauseState(ctx context.Context, runID, state string) error {
	if state == store.PauseStateRunning {
		p.mu.Lock()
		p.ids = append(p.ids, runID)
		p.mu.Unlock()
	}
	return p.Store.SetRunPauseState(ctx, runID, state)
}

// ResumePausedRuns resumes a paused child before its paused parent, though the
// parent started first.
func TestResumePausedRuns_ResumesAChildBeforeItsParent(t *testing.T) {
	prov := newBGFamily()
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	appendResumeEvent(t, srv, lead.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hold the lead"}}},
	})
	markPaused(t, srv, lead)
	child := workerRun(t, srv, lead, "a_c1", "hold one", true)
	flips := &pauseFlips{Store: srv.store}
	srv.store = flips
	if n, warns := srv.ResumePausedRuns(context.Background()); n != 2 {
		t.Fatalf("resumed %d, want both (warnings: %v)", n, warns)
	}
	flips.mu.Lock()
	defer flips.mu.Unlock()
	if len(flips.ids) != 2 || flips.ids[0] != child.ID || flips.ids[1] != lead.ID {
		t.Errorf("resumed in the order %v, want the child %s then the lead %s", flips.ids, child.ID, lead.ID)
	}
}

// A resident child resumed on another instance that then stops at its
// iteration limit hands its parent that turn as an error naming the limit,
// with the turn's answer after it — as a child that never paused does. Before,
// the resumed child's end was filed as a completed turn.
func TestResumePausedRuns_AResumedResidentChildCappedAtItsLimitIsAnError(t *testing.T) {
	ctx := context.Background()
	cfgA := bgResumeConfig("")
	worker := cfgA.Agents["worker"]
	worker.MaxIterations = 3
	cfgA.Agents["worker"] = worker
	provA := &residentFamily{gate: make(chan struct{}), script: []func(providers.Request) []providers.Event{
		func(providers.Request) []providers.Event {
			return agentCall("tu_1", `{"op":"open","name":"worker","prompt":"first"}`)
		},
	}}
	srvA, _ := makeServer(t, provA, cfgA)
	srvA.SetSteerRegistry(steer.NewRegistry(0))
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
	_ = postLead(t, tsA)

	var childID string
	waitFor(t, "the resident child to be opened on A", func() bool {
		for _, info := range srvA.residentReg.listInfo() {
			childID = info.ChildRunID
		}
		return childID != ""
	})
	child, err := srvA.store.GetRun(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	leadID := child.ParentRunID

	paused := make(chan error, 1)
	go func() {
		_, err := srvA.pauseMgr.Pause(ctx, 5*time.Second)
		paused <- err
	}()
	waitFor(t, "the pause to be declared on A", func() bool { return srvA.pauseMgr.State() != pause.StateRunning })
	openGate()
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{leadID, childID} {
		waitFor(t, "run "+id+" to be paused on A", func() bool {
			run, err := srvA.store.GetRun(ctx, id)
			return err == nil && run.PauseState == store.PauseStatePaused
		})
	}
	_, raw, err := snapshot.Capture(ctx, srvA.store, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	provB := &residentFamily{capOn: "second", script: []func(providers.Request) []providers.Event{
		func(providers.Request) []providers.Event {
			return agentCall("tu_2", `{"op":"send","child_run_id":"`+childID+`","prompt":"second"}`)
		},
		func(providers.Request) []providers.Event { return answer("done") },
	}}
	cfgB := bgResumeConfig("")
	cfgB.Agents["worker"] = worker
	srvB, _ := makeServer(t, provB, cfgB)
	srvB.SetSteerRegistry(steer.NewRegistry(0))
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
	var restored snapshotRestoreResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &restored)
	if rec.Code != http.StatusOK || restored.PausedRunsResumed != 2 {
		t.Fatalf("restore on B: status %d, resumed %d, want the lead and its resident child: %s", rec.Code, restored.PausedRunsResumed, rec.Body.String())
	}

	waitWalkRunStatus(t, srvB.store, leadID, store.RunCompleted)
	calls := provB.leadCalls()
	if len(calls) != 2 {
		t.Fatalf("the lead made %d calls on B, want 2", len(calls))
	}
	sent := lastToolText(calls[1])
	if !strings.Contains(sent, "stopped at its iteration limit") || !strings.Contains(sent, "Its last answer") || !strings.Contains(sent, "reply to") {
		t.Errorf("send on B = %s, want the capped error with the turn's answer", sent)
	}
	ended := waitWalkRunStatus(t, srvB.store, childID, store.RunCompleted)
	if ended.StopReason != loop.StopReasonMaxIterations {
		t.Errorf("the child ended with stop reason %q, want %q", ended.StopReason, loop.StopReasonMaxIterations)
	}
}
