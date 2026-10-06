package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// One agent state run by "worker": the smallest walk with a member to hold.
const pollWalkTeam = `{"entry":"review","states":[` +
	`{"state":"review","handler":{"kind":"agent","agent":"worker"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"review","to":"done","on":"success"}]}`

// scriptedParent answers the lead from a script that may read the request it
// answers (the run ids earlier calls returned), and holds every team member
// ("you are a child") until release is closed.
type scriptedParent struct {
	mu      sync.Mutex
	script  []func(req providers.Request) []providers.Event
	calls   []providers.Request
	release chan struct{}
	once    sync.Once
}

// unblock lets every held member finish. Idempotent, so a test registers it
// as a cleanup and may also call it.
func (p *scriptedParent) unblock() { p.once.Do(func() { close(p.release) }) }

func (p *scriptedParent) ID() string                  { return "stub" }
func (p *scriptedParent) Probe(context.Context) error { return nil }
func (p *scriptedParent) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *scriptedParent) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *scriptedParent) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		events = answer("child result")
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

func (p *scriptedParent) leadCalls() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.calls...)
}

func toolCallEv(id, name, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func fixed(evs []providers.Event) func(providers.Request) []providers.Event {
	return func(providers.Request) []providers.Event { return evs }
}

// lastToolText is the text of the last tool result in the request: the
// answer to the tool call the lead made last. A note about children may
// follow it.
func lastToolText(req providers.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, b := range req.Messages[i].Content {
			if b.Type == "tool_result" {
				return b.Text
			}
		}
	}
	return ""
}

// lastText is the text of the request's last message: a note, when one was
// appended.
func lastText(req providers.Request) string {
	m := req.Messages[len(req.Messages)-1]
	return m.Content[0].Text
}

// runIDIn is the run_id a TeamDef run answered with.
func runIDIn(text string) string {
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	return out.RunID
}

// newPollWalkServer is a server whose "lead" agent has the TeamDef and Agent
// tools and whose team "rev" runs one "worker" member.
func newPollWalkServer(t *testing.T, prov providers.Provider) (*Server, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"lead":   {Model: "stub-model", Tools: []string{"TeamDef", "Agent"}, SystemPrompt: "you are the lead"},
		"worker": {Model: "stub-model", SystemPrompt: "you are a child"},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "pollwalk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	td := &builtin.TeamDef{Store: st}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{td}, concurrency.New(8, 8, time.Second), st)
	srv.SetTeamDefTool(td)
	seedTenantTeam(t, st, "", "rev", pollWalkTeam)
	return srv, st
}

// startLead posts a run of "lead" and returns a channel the run's stream
// arrives on once it ends.
func startLead(t *testing.T, ts *httptest.Server) <-chan string {
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

// leadWaiting waits for the lead's run to wait on its children and returns
// its agent id, run id and what it waits on.
func leadWaiting(t *testing.T, srv *Server) (agentID, runID, on string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, e := range srv.cancelReg.ListAll() {
			if e.ParentAgentID != "" {
				continue
			}
			if state, waitOn := awaited.ForRun(context.Background(), srv.store, e.RunID); state == awaited.Children {
				return e.AgentID, e.RunID, waitOn
			}
		}
	}
	t.Fatal("the lead never waited on its children")
	return "", "", ""
}

// waitWalkMembers waits until the walk has at least n member runs (its own
// run excluded).
func waitWalkMembers(t *testing.T, st store.Store, walkID string, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		runs, _, err := st.ListRunsByWalk(context.Background(), "", walkID, 50, "")
		members := 0
		for _, r := range runs {
			if r.ID != walkID {
				members++
			}
		}
		if err == nil && members >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("walk %s has %d members, %v; want at least %d", walkID, members, err, n)
		}
	}
}

func waitWalkRunStatus(t *testing.T, st store.Store, id string, want store.RunStatus) store.Run {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		run, err := st.GetRun(context.Background(), id)
		if err == nil && run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s = %+v, %v; want %s", id, run, err, want)
		}
	}
}

// End to end on a real server: a lead runs a team in poll mode and ends its
// turn while the walk's member still works. The walk's run is a child of the
// lead's run under the id the lead was handed; the lead waits on it (awaited
// children, no model call), wakes once with a note naming the walk, reads its
// answer with TeamDef poll and completes.
func TestTeamWalkPoll_ParentWaitsForThePollModeWalkAndReadsItsAnswer(t *testing.T) {
	prov := &scriptedParent{release: make(chan struct{})}
	prov.script = []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"the diff","mode":"poll"}`)),
		fixed(answer("started the review")),
		fixed(toolCallEv("tu_2", "TeamDef", `{"op":"poll"}`)),
		fixed(answer("all done")),
	}
	srv, st := newPollWalkServer(t, prov)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	t.Cleanup(prov.unblock) // runs first: a failed test must not leave a member holding the server open
	body := startLead(t, ts)

	_, parentRun, on := leadWaiting(t, srv)
	walkID := runIDIn(lastToolText(prov.leadCalls()[1]))
	if walkID == "" || on != walkID {
		t.Fatalf("the lead waits on %q; the walk it started is %q", on, walkID)
	}
	walk, err := st.GetRun(context.Background(), walkID)
	if err != nil || walk.Status != store.RunRunning || walk.ParentRunID != parentRun || walk.AgentID != "team:rev" {
		t.Fatalf("walk run = %+v, %v; want team:rev running under the lead's run %s", walk, err, parentRun)
	}
	if !strings.Contains(string(walk.RunConfig), `"mode":"poll"`) {
		t.Errorf("walk run config = %s, want mode poll", walk.RunConfig)
	}
	if n := srv.liveChildren.Alive(parentRun); n != 1 {
		t.Errorf("the lead has %d live children while the walk runs, want 1", n)
	}
	if n := len(prov.leadCalls()); n != 2 {
		t.Fatalf("the lead made %d calls while waiting, want 2", n)
	}

	prov.unblock()
	stream := <-body
	if !strings.Contains(stream, "event: children_note") {
		t.Errorf("stream lacks the children note:\n%s", stream)
	}
	calls := prov.leadCalls()
	if len(calls) != 4 {
		t.Fatalf("the lead made %d calls, want 4", len(calls))
	}
	wake := lastText(calls[2])
	if !strings.Contains(wake, walkID+" (team:rev): completed") || !strings.Contains(wake, "TeamDef poll") {
		t.Errorf("wake note = %q", wake)
	}
	var polled struct {
		Walks []map[string]any `json:"walks"`
	}
	if err := json.Unmarshal([]byte(lastToolText(calls[3])), &polled); err != nil || len(polled.Walks) != 1 {
		t.Fatalf("TeamDef poll answered %q (%v)", lastToolText(calls[3]), err)
	}
	if w := polled.Walks[0]; w["run_id"] != walkID || w["state"] != "completed" || w["status"] != "completed" ||
		w["final_state"] != "done" || w["final_output"] != "child result" {
		t.Errorf("polled walk = %v", w)
	}
	waitWalkRunStatus(t, st, walkID, store.RunCompleted)
	waitWalkRunStatus(t, st, parentRun, store.RunCompleted)
	if n := srv.liveChildren.Alive(parentRun); n != 0 {
		t.Errorf("%d live children after the walk ended", n)
	}
}

// Cancelling a lead that waits for a poll-mode walk cancels the walk and its
// member.
//
// Not asserted here: that a walk the lead started DETACHED survives. Its
// member registers in the cancel registry as a child of the lead's agent (the
// walk's ctx keeps the lead's run identity), so the registry's cascade cancels
// it once it is running — behaviour that predates poll mode, left as it is.
func TestTeamWalkPoll_CancellingTheParentCancelsThePollWalk(t *testing.T) {
	prov := &scriptedParent{release: make(chan struct{})}
	prov.script = []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"b","mode":"poll"}`)),
		fixed(answer("started it")),
	}
	srv, st := newPollWalkServer(t, prov)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	t.Cleanup(prov.unblock) // runs first: a failed test must not leave a member holding the server open
	body := startLead(t, ts)

	parentAgent, parentRun, _ := leadWaiting(t, srv)
	polled := runIDIn(lastToolText(prov.leadCalls()[1]))
	if polled == "" {
		t.Fatal("no walk id")
	}
	// The walk starts its member on its own goroutine, after the lead already
	// waits on the walk. Cancelling before the member exists ends a walk with
	// no member to cancel, which is not the cascade under test.
	waitWalkMembers(t, st, polled, 1)
	if _, ok := srv.cancelReg.Cancel(parentAgent, "operator stop"); !ok {
		t.Fatal("the lead is not cancellable")
	}
	<-body
	waitWalkRunStatus(t, st, parentRun, store.RunCancelled)
	waitWalkRunStatus(t, st, polled, store.RunCancelled)
	for deadline := time.Now().Add(10 * time.Second); srv.liveChildren.Alive(parentRun) != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled walk never ended")
		}
	}
	runs, _, err := st.ListRunsByWalk(context.Background(), "", polled, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	members := 0
	for _, m := range runs {
		if m.ID == polled {
			continue
		}
		members++
		if m.Status != store.RunCancelled {
			t.Errorf("member %s of the cancelled walk = %s", m.ID, m.Status)
		}
	}
	if members != 1 {
		t.Errorf("the cancelled walk had %d members, want 1", members)
	}
}

// Agent cancel names a poll-mode walk by its run id and ends it: the walk's
// run ends cancelled with the parent's reason.
func TestTeamWalkPoll_AgentCancelEndsTheWalk(t *testing.T) {
	prov := &scriptedParent{release: make(chan struct{})}
	prov.script = []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"a","mode":"poll"}`)),
		func(req providers.Request) []providers.Event {
			return toolCallEv("tu_2", "Agent", `{"op":"cancel","child_run_ids":["`+runIDIn(lastToolText(req))+`"]}`)
		},
		fixed(answer("cancelled it")),
	}
	srv, st := newPollWalkServer(t, prov)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	t.Cleanup(prov.unblock) // runs first: a failed test must not leave a member holding the server open
	stream := <-startLead(t, ts)
	if strings.Contains(stream, "event: awaiting_children") {
		t.Error("the lead waited for a walk it had cancelled")
	}
	calls := prov.leadCalls()
	if len(calls) != 3 {
		t.Fatalf("the lead made %d calls, want 3", len(calls))
	}
	walkID := runIDIn(lastToolText(calls[1]))
	if res := lastToolText(calls[2]); !strings.Contains(res, `"state":"cancelled"`) || !strings.Contains(res, `"kind":"team"`) {
		t.Errorf("Agent cancel answered %s", res)
	}
	walk := waitWalkRunStatus(t, st, walkID, store.RunCancelled)
	if !strings.Contains(walk.StopReason, "cancelled by its parent") {
		t.Errorf("walk stop reason = %q", walk.StopReason)
	}
}
