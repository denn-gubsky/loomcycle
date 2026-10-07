package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// postLead posts a run of "lead" and returns a channel its stream arrives on
// once the run ends.
func postLead(t *testing.T, ts *httptest.Server) <-chan string {
	t.Helper()
	body := make(chan string, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"lead","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
		if err != nil {
			body <- "post: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	return body
}

func streamOf(t *testing.T, body <-chan string) string {
	t.Helper()
	select {
	case s := <-body:
		return s
	case <-time.After(15 * time.Second):
		t.Fatal("the lead's run did not end")
		return ""
	}
}

// The runtime pausing and resuming on the same instance does not disturb a
// parent waiting for its children: it is recorded paused while the pause
// lasts, goes back to waiting, and wakes once when they end.
func TestPollMode_AWaitingParentWakesOnceAcrossARuntimePause(t *testing.T) {
	prov := &familyProvider{
		release: make(chan struct{}),
		parent: [][]providers.Event{
			agentCall("tu_1", `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"worker","prompt":"one"},{"name":"worker","prompt":"two"}]}`),
			answer("started them"),
			agentCall("tu_2", `{"op":"poll"}`),
			answer("both done"),
		},
	}
	srv, _ := makeServer(t, prov, familyConfig())
	srv.SetPauseManager(pause.NewManager(srv.store, 200*time.Millisecond))
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)
	var once sync.Once
	release := func() { once.Do(func() { close(prov.release) }) }
	defer release()
	body := postLead(t, ts)

	_, parentRun, _ := leadWaiting(t, srv)
	ctx := context.Background()
	// The children are inside a model call and cannot park; the parent can.
	if _, err := srv.pauseMgr.Pause(ctx, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the waiting parent to be recorded paused", func() bool {
		run, err := srv.store.GetRun(ctx, parentRun)
		return err == nil && run.PauseState == store.PauseStatePaused
	})
	if _, err := srv.pauseMgr.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	stream := streamOf(t, body)
	if n := strings.Count(stream, "event: children_note"); n != 1 {
		t.Errorf("%d notes on the stream, want the one wake note:\n%s", n, stream)
	}
	if n := len(prov.parentCalls()); n != 4 {
		t.Errorf("the parent made %d calls, want 4", n)
	}
	waitWalkRunStatus(t, srv.store, parentRun, store.RunCompleted)
}

// crossFamily is one instance's provider for the cross-instance test. The
// lead answers from a script. A child prompted "fast" answers at once; one
// prompted "slow" answers its first call with a tool call once gate is
// closed — the boundary after it is where a pause parks it — and on any
// later call answers "result of slow" (the instance it resumes on).
type crossFamily struct {
	mu        sync.Mutex
	script    [][]providers.Event
	calls     []providers.Request
	gate      chan struct{}
	slowCalls int
}

func (p *crossFamily) ID() string                  { return "stub" }
func (p *crossFamily) Probe(context.Context) error { return nil }
func (p *crossFamily) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *crossFamily) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *crossFamily) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		prompt := req.Messages[0].Content[0].Text
		switch {
		case prompt != "slow":
			events = answer("result of " + prompt)
		case len(req.Messages) == 1:
			p.mu.Lock()
			p.slowCalls++
			p.mu.Unlock()
			select {
			case <-p.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			events = toolCallEv("tu_child", "Nope", `{}`)
		default:
			events = answer("result of slow")
		}
	} else {
		p.mu.Lock()
		i := len(p.calls)
		p.calls = append(p.calls, req)
		if i < len(p.script) {
			events = p.script[i]
		}
		p.mu.Unlock()
		if events == nil {
			return nil, context.Canceled
		}
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// slowInFlight reports whether the slow child's first call has reached the
// gate.
func (p *crossFamily) slowInFlight() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slowCalls > 0
}

func (p *crossFamily) leadCalls() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.calls...)
}

// End to end across two instances. On A a lead starts two children in poll
// mode, reads the fast one's result, and waits for the slow one. A is paused
// while it waits — the slow child parks at its next boundary — and
// snapshotted; B restores the snapshot. B rebuilds the lead's table from its
// ledger (the fast child's run is not in the snapshot: only its result row
// is), resumes the slow child, and the lead — resumed into its wait — wakes
// once when the slow child ends there. A bare poll on B then hands over only
// the slow child, through the lead's subagent_stop hook, which saw each child
// exactly once across both instances.
func TestResumePausedRuns_AParentPausedOnOneInstanceWakesOnAnother(t *testing.T) {
	ctx := context.Background()
	hook := newRecordingHook(t, `{"additional_context":"(checked by ops)"}`)

	provA := &crossFamily{gate: make(chan struct{}), script: [][]providers.Event{
		agentCall("tu_1", `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"worker","prompt":"fast"},{"name":"worker","prompt":"slow"}]}`),
		agentCall("tu_2", `{"op":"poll","wait":"any","wait_ms":10000}`),
		answer("waiting for the slow one"),
	}}
	srvA, _ := makeServer(t, provA, bgResumeConfig(hook.srv.URL))
	srvA.resetTestHooks()
	srvA.SetPauseManager(pause.NewManager(srvA.store, 5*time.Second))
	tsA := httptest.NewServer(srvA.Mux())
	defer tsA.Close()
	defer func() {
		// A's copies are abandoned: B owns the work now. They end cancelled on
		// A's own store before it closes.
		cancelAllRuns(srvA)
		for deadline := time.Now().Add(10 * time.Second); srvA.cancelReg.Count() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(provA.gate) }) }
	defer openGate()
	bodyA := postLead(t, tsA)

	_, leadID, on := leadWaiting(t, srvA)
	if strings.Count(on, "r_") != 1 {
		t.Fatalf("the lead on A waits on %q, want the slow child alone", on)
	}
	slowID := on
	var fastID string
	for _, ev := range leadEvents(t, srvA, store.Run{ID: leadID, SessionID: sessionOf(t, srvA, leadID)}, providers.EventSpawnChildRead) {
		fastID = ev.SpawnChild.RunID
	}
	if fastID == "" || fastID == slowID {
		t.Fatalf("the lead on A read %q before it waited, want the fast child", fastID)
	}

	// The pause must find the slow child inside its first call, so it parks at
	// the boundary after it. Paused before that call, it would resume on B
	// still owing its first call, which waits on a gate B does not have.
	waitFor(t, "the slow child's first call on A", provA.slowInFlight)
	paused := make(chan error, 1)
	go func() {
		_, err := srvA.pauseMgr.Pause(ctx, 5*time.Second)
		paused <- err
	}()
	waitFor(t, "the pause to be declared on A", func() bool { return srvA.pauseMgr.State() != pause.StateRunning })
	openGate() // the slow child reaches its next boundary, where the pause parks it
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{leadID, slowID} {
		waitFor(t, "run "+id+" to be paused on A", func() bool {
			run, err := srvA.store.GetRun(ctx, id)
			return err == nil && run.PauseState == store.PauseStatePaused
		})
	}
	_, raw, err := snapshot.Capture(ctx, srvA.store, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	provB := &crossFamily{script: [][]providers.Event{
		agentCall("tu_3", `{"op":"poll"}`),
		answer("all done"),
	}}
	srvB, _ := makeServer(t, provB, bgResumeConfig(hook.srv.URL))
	srvB.resetTestHooks()
	srvB.SetRunStateBus(runstate.NewBus())
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
		t.Fatalf("restore on B: status %d, resumed %d, want the lead and the slow child: %s", rec.Code, restored.PausedRunsResumed, rec.Body.String())
	}

	waitWalkRunStatus(t, srvB.store, leadID, store.RunCompleted)
	waitWalkRunStatus(t, srvB.store, slowID, store.RunCompleted)
	calls := provB.leadCalls()
	if len(calls) != 2 {
		t.Fatalf("the lead made %d calls on B, want 2", len(calls))
	}
	if wake := lastText(calls[0]); !strings.Contains(wake, slowID+" (worker): completed") || strings.Contains(wake, fastID) {
		t.Errorf("wake note on B = %q, want the slow child alone", wake)
	}
	polled := lastToolText(calls[1])
	if !strings.Contains(polled, "result of slow") || !strings.Contains(polled, "(checked by ops)") || strings.Contains(polled, fastID) {
		t.Errorf("bare poll on B = %s, want the slow child's hooked result and not the fast child it read on A", polled)
	}
	hook.mu.Lock()
	stops := map[string]int{}
	for _, b := range hook.bodies {
		if !strings.Contains(b, `"phase":"subagent_stop"`) {
			continue
		}
		for _, id := range []string{fastID, slowID} {
			if strings.Contains(b, `"subagent_run_id":"`+id+`"`) {
				stops[id]++
			}
		}
	}
	hook.mu.Unlock()
	if stops[fastID] != 1 || stops[slowID] != 1 {
		t.Errorf("subagent_stop per child = %v, want each child once across both instances", stops)
	}
	_ = bodyA
}

// sessionOf is a run's session id.
func sessionOf(t *testing.T, srv *Server, runID string) string {
	t.Helper()
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return run.SessionID
}

// A team walk run in poll mode records its whole answer — its steps, which its
// own run row does not hold — on its parent's ledger when it ends, so a parent
// resumed after the walk ended reads the full answer with TeamDef poll. The
// parent here is the transcript of a real lead whose walk ended, resumed as a
// paused run of its own.
func TestResumePausedRuns_APollModeWalkIsReadWholeAfterTheResume(t *testing.T) {
	ctx := context.Background()
	prov := &scriptedParent{release: make(chan struct{})}
	prov.script = []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"the diff","mode":"poll"}`)),
		fixed(answer("started the review")),
		// The third call ends the live lead (no script left), after its wake.
	}
	srv, st := newPollWalkServer(t, prov)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	t.Cleanup(prov.unblock)
	body := startLead(t, ts)
	_, liveLead, _ := leadWaiting(t, srv)
	prov.unblock()
	streamOf(t, body)

	live, err := st.GetRun(ctx, liveLead)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.GetTranscript(ctx, live.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, "", "lead", live.UserID)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_resumed_lead", UserID: live.UserID, Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	walkID := ""
	for _, e := range events {
		if e.RunID != liveLead || e.Type == "error" {
			continue
		}
		if err := st.AppendEvent(ctx, lead.ID, e.Type, e.Payload); err != nil {
			t.Fatal(err)
		}
		var pe providers.Event
		if e.Type == string(providers.EventSpawnChildResult) && json.Unmarshal(e.Payload, &pe) == nil && pe.SpawnChild.Kind == "team" {
			walkID = pe.SpawnChild.RunID
		}
	}
	if walkID == "" {
		t.Fatal("the walk's end was not recorded on the lead's ledger")
	}
	markPaused(t, srv, lead)

	prov.mu.Lock()
	before := len(prov.calls)
	prov.script = append(prov.script,
		func(providers.Request) []providers.Event { return nil }, // the live lead's third call, already made
		fixed(toolCallEv("tu_2", "TeamDef", `{"op":"poll"}`)),
		fixed(answer("read it")))
	prov.mu.Unlock()
	if before != 3 {
		t.Fatalf("the live lead made %d calls, want 3", before)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want the lead (warnings: %v)", n, warns)
	}
	waitWalkRunStatus(t, st, lead.ID, store.RunCompleted)
	polled := lastToolText(prov.leadCalls()[4])
	for _, want := range []string{`"run_id":"` + walkID + `"`, `"steps":[`, `"output":"child result"`, `"final_output":"child result"`} {
		if !strings.Contains(polled, want) {
			t.Errorf("TeamDef poll after the resume = %s, want %s", polled, want)
		}
	}
}
