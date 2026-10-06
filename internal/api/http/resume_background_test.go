package http

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// bgFamily answers the lead from a script and each child by its prompt: a
// prompt containing "hold" waits until release is closed, any other is
// answered at once with "result of <prompt>". A prompt containing "late"
// waits for late to be closed (at most 10s) whether or not it is cancelled,
// and then fails: a child that ends only after the test says so. While late is
// set, the lead's calls after its first wait for that child to be in its call.
type bgFamily struct {
	mu       sync.Mutex
	script   [][]providers.Event
	calls    []providers.Request
	release  chan struct{}
	once     sync.Once
	late     chan struct{}
	lateUp   chan struct{}
	lateOnce sync.Once
}

func newBGFamily(script ...[]providers.Event) *bgFamily {
	return &bgFamily{script: script, release: make(chan struct{}), lateUp: make(chan struct{})}
}

// unblock lets every held child finish. Idempotent.
func (p *bgFamily) unblock() { p.once.Do(func() { close(p.release) }) }

func (p *bgFamily) ID() string                  { return "stub" }
func (p *bgFamily) Probe(context.Context) error { return nil }
func (p *bgFamily) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *bgFamily) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *bgFamily) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		prompt := req.Messages[0].Content[0].Text
		if strings.Contains(prompt, "late") && p.late != nil {
			p.lateOnce.Do(func() { close(p.lateUp) })
			select {
			case <-p.late:
			case <-time.After(10 * time.Second):
			}
			return nil, fmt.Errorf("late child gave up")
		}
		if strings.Contains(prompt, "hold") {
			select {
			case <-p.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		events = answer("result of " + prompt)
	} else {
		p.mu.Lock()
		waitLate := p.late != nil && len(p.calls) > 0
		p.mu.Unlock()
		if waitLate {
			select {
			case <-p.lateUp:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
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

func (p *bgFamily) leadCalls() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.calls...)
}

// bgResumeConfig is a lead that spawns workers, and the workers. A non-empty
// hookURL gives the lead a subagent_stop hook there.
func bgResumeConfig(hookURL string) *config.Config {
	cfg := makeBaseConfig()
	lead := config.AgentDef{Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the lead"}
	if hookURL != "" {
		lead.Hooks = hooks.EventHooks{hooks.PhaseSubagentStop: {{Inline: &hooks.Inline{Name: "check", URL: hookURL}}}}
		cfg.Hooks.PrivateHostAllowlist = []string{"127.0.0.1"} // the recorder is on loopback
	}
	cfg.Agents = map[string]config.AgentDef{
		"lead":   lead,
		"worker": {Model: "stub-model", SystemPrompt: "you are a child"},
	}
	return cfg
}

// settle stops every run the server still has and waits for them to be gone,
// so nothing writes to the test's store once it closes. Registered as a
// cleanup, it runs before the store is closed.
func settle(t *testing.T, srv *Server, prov *bgFamily) {
	t.Helper()
	t.Cleanup(func() {
		prov.unblock()
		cancelAllRuns(srv)
		for deadline := time.Now().Add(10 * time.Second); srv.cancelReg.Count() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // a cancel of a restored child reads the store on its own goroutine
	})
}

// pausedLead creates the lead's run, paused.
func pausedLead(t *testing.T, srv *Server) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_lead", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// workerRun is a worker child run of parent, its prompt recorded. paused
// leaves it for ResumePausedRuns to resume; otherwise it is left as created
// (running, with no loop behind it here: running elsewhere).
func workerRun(t *testing.T, srv *Server, parent store.Run, agentID, prompt string, paused bool) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: "alice", Model: "stub-model", ParentRunID: parent.ID, ParentAgentID: parent.AgentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: prompt}}},
	})
	if paused {
		if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

// spawnedInPoll records the lead's parallel_spawn in poll mode of the given
// children, as the live run records it: the call, a started row per child,
// and the handles it answered with.
func spawnedInPoll(t *testing.T, srv *Server, lead store.Run, ids []string) {
	t.Helper()
	appendResumeEvent(t, srv, lead.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
	})
	appendResumeEvent(t, srv, lead.ID, "tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"op":"parallel_spawn","mode":"poll","spawns":[]}`)}})
	for i, id := range ids {
		appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
			SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_1", Index: i, RunID: id, Agent: "worker", Mode: "poll", BatchID: "fan_1"}})
	}
	appendResumeEvent(t, srv, lead.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}})
	appendResumeEvent(t, srv, lead.ID, "tool_result", providers.Event{Type: providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent"}, Text: `{"batch_id":"fan_1"}`})
}

// waitedFor records the lead ending its turn and waiting for ids.
func waitedFor(t *testing.T, srv *Server, lead store.Run, ids ...string) {
	t.Helper()
	appendResumeEvent(t, srv, lead.ID, "text", providers.Event{Type: providers.EventText, Text: "started them"})
	appendResumeEvent(t, srv, lead.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}})
	appendResumeEvent(t, srv, lead.ID, string(providers.EventAwaitingChildren), providers.Event{Type: providers.EventAwaitingChildren,
		AwaitingChildren: &providers.AwaitingChildrenEventInfo{ChildRunIDs: ids}})
}

func markPaused(t *testing.T, srv *Server, run store.Run) {
	t.Helper()
	if err := srv.store.SetRunPauseState(context.Background(), run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
}

// leadEvents is the lead's transcript rows of one type.
func leadEvents(t *testing.T, srv *Server, lead store.Run, typ providers.EventType) []providers.Event {
	t.Helper()
	events, err := srv.store.GetTranscript(context.Background(), lead.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []providers.Event
	for _, e := range events {
		var pe providers.Event
		if e.RunID == lead.ID && e.Type == string(typ) && json.Unmarshal(e.Payload, &pe) == nil {
			out = append(out, pe)
		}
	}
	return out
}

// A parent that paused while it waited for its background children is resumed
// into that wait rather than refused: it makes no model call, its children
// count as its live children again, and it wakes ONCE, when the last child
// has ended, with a note naming both — then reads them and completes. Before,
// it was flagged failed as not auto-resumable and its children finished unread.
func TestResumePausedRuns_AParentWaitingForItsChildrenWaitsForThemAgain(t *testing.T) {
	prov := newBGFamily(agentCall("tu_2", `{"op":"poll"}`), answer("both done"))
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	c1 := workerRun(t, srv, lead, "a_c1", "hold one", true)
	c2 := workerRun(t, srv, lead, "a_c2", "hold two", true)
	spawnedInPoll(t, srv, lead, []string{c1.ID, c2.ID})
	waitedFor(t, srv, lead, c1.ID, c2.ID)
	markPaused(t, srv, lead)

	if n, warns := srv.ResumePausedRuns(context.Background()); n != 3 {
		t.Fatalf("resumed %d runs, want the lead and both children (warnings: %v)", n, warns)
	}
	waitFor(t, "the lead to wait for its children again", func() bool {
		return len(leadEvents(t, srv, lead, providers.EventAwaitingChildren)) == 2
	})
	if n := srv.liveChildren.Alive(lead.ID); n != 2 {
		t.Errorf("the lead has %d live children, want its 2 restored ones", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(prov.leadCalls()); n != 0 {
		t.Fatalf("the lead made %d model calls while it waited", n)
	}

	prov.unblock()
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	calls := prov.leadCalls()
	if len(calls) != 2 {
		t.Fatalf("the lead made %d calls, want 2", len(calls))
	}
	wake := lastText(calls[0])
	for _, want := range []string{"Every background child you were waiting for has ended:", c1.ID + " (worker): completed", c2.ID + " (worker): completed"} {
		if !strings.Contains(wake, want) {
			t.Errorf("wake note %q lacks %q", wake, want)
		}
	}
	if notes := leadEvents(t, srv, lead, providers.EventChildrenNote); len(notes) != 1 {
		t.Errorf("%d notes, want the one wake note", len(notes))
	}
	polled := lastToolText(calls[1])
	if !strings.Contains(polled, "result of hold one") || !strings.Contains(polled, "result of hold two") {
		t.Errorf("poll after the wake = %s, want both results", polled)
	}
	if reads := leadEvents(t, srv, lead, providers.EventSpawnChildRead); len(reads) != 2 {
		t.Errorf("%d read rows, want 2", len(reads))
	}
	if n := srv.liveChildren.Alive(lead.ID); n != 0 {
		t.Errorf("%d live children left after both ended", n)
	}
}

// What the parent was already handed, or already told of, before it paused is
// not handed over or reported again after it is resumed: a bare poll skips the
// child it read, the wake note skips the child it was told of. The child that
// ends after the resume reaches the parent through its subagent_stop hook
// once — and its result row is recorded, so a later resume does not run the
// hook again. Children that ended before the pause need no run row: their
// result is on the parent's ledger.
func TestResumePausedRuns_ReadAndNotedChildrenAreNotHandedOverAgain(t *testing.T) {
	hook := newRecordingHook(t, `{"additional_context":"(checked by ops)"}`)
	prov := newBGFamily(agentCall("tu_2", `{"op":"poll"}`), agentCall("tu_3", `{"op":"poll"}`), answer("done"))
	srv, _ := makeServer(t, prov, bgResumeConfig(hook.srv.URL))
	srv.resetTestHooks()
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	c3 := workerRun(t, srv, lead, "a_c3", "hold three", true)
	spawnedInPoll(t, srv, lead, []string{"r_fast", "r_noted", c3.ID})
	for _, id := range []string{"r_fast", "r_noted"} {
		appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildResult), providers.Event{Type: providers.EventSpawnChildResult,
			SpawnChild: &providers.SpawnChildEventInfo{RunID: id, Agent: "worker", Mode: "poll", BatchID: "fan_1",
				Ok: true, Ended: "completed", Output: "answer of " + id}})
	}
	appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildRead), providers.Event{Type: providers.EventSpawnChildRead,
		SpawnChild: &providers.SpawnChildEventInfo{RunID: "r_fast", Agent: "worker", Mode: "poll"}})
	appendResumeEvent(t, srv, lead.ID, string(providers.EventChildrenNote), providers.Event{Type: providers.EventChildrenNote,
		ChildrenNote: &providers.ChildrenNoteEventInfo{Text: "Background child r_noted (worker) finished: completed.", ChildRunIDs: []string{"r_noted"}}})
	waitedFor(t, srv, lead, c3.ID)
	markPaused(t, srv, lead)

	if n, warns := srv.ResumePausedRuns(context.Background()); n != 2 {
		t.Fatalf("resumed %d runs, want the lead and its running child (warnings: %v)", n, warns)
	}
	waitFor(t, "the lead to wait again", func() bool {
		return len(leadEvents(t, srv, lead, providers.EventAwaitingChildren)) == 2
	})
	prov.unblock()
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	calls := prov.leadCalls()
	if len(calls) != 3 {
		t.Fatalf("the lead made %d calls, want 3", len(calls))
	}
	if wake := lastText(calls[0]); !strings.Contains(wake, c3.ID) || strings.Contains(wake, "r_noted") || strings.Contains(wake, "r_fast") {
		t.Errorf("wake note = %q, want only the child that ended after the resume", wake)
	}
	first := lastToolText(calls[1])
	if !strings.Contains(first, "answer of r_noted") || !strings.Contains(first, "result of hold three") || strings.Contains(first, "r_fast") {
		t.Errorf("first bare poll = %s, want the unread children and not the one already read", first)
	}
	if !strings.Contains(first, "(checked by ops)") {
		t.Errorf("first bare poll = %s, want the hook's context on the child that ended after the resume", first)
	}
	if second := lastToolText(calls[2]); strings.Contains(second, "answer of") || strings.Contains(second, "result of") {
		t.Errorf("second bare poll = %s, want nothing handed over twice", second)
	}
	hook.mu.Lock()
	var stops []string
	for _, b := range hook.bodies {
		if strings.Contains(b, `"phase":"subagent_stop"`) {
			stops = append(stops, b)
		}
	}
	hook.mu.Unlock()
	if len(stops) != 1 || !strings.Contains(stops[0], `"subagent_run_id":"`+c3.ID+`"`) {
		t.Errorf("subagent_stop ran %d times (%v), want once, for %s", len(stops), stops, c3.ID)
	}
	var recorded bool
	for _, ev := range leadEvents(t, srv, lead, providers.EventSpawnChildResult) {
		if sc := ev.SpawnChild; sc != nil && sc.RunID == c3.ID && strings.HasSuffix(sc.Output, "(checked by ops)") {
			recorded = true
		}
	}
	if !recorded {
		t.Error("the hooked result of the child that ended after the resume was not recorded")
	}
}

// A parent paused mid-turn — not waiting — resumes as it always did, with its
// table rebuilt: poll by id reads a child that ended elsewhere while it was
// paused from that child's run row, and a bare poll then hands over only what
// it has not read.
func TestResumePausedRuns_AParentPausedMidTurnPollsItsChildren(t *testing.T) {
	ctx := context.Background()
	srv, _ := makeServer(t, &bgFamily{}, bgResumeConfig(""))
	lead := pausedLead(t, srv)
	remote := workerRun(t, srv, lead, "a_remote", "elsewhere", false)
	if err := srv.store.FinishRun(ctx, remote.ID, store.RunCompleted, "end_turn",
		store.Usage{Result: json.RawMessage(`{"final_text":"remote answer"}`)}, ""); err != nil {
		t.Fatal(err)
	}
	prov := newBGFamily(
		agentCall("tu_2", fmt.Sprintf(`{"op":"poll","child_run_ids":[%q],"wait":"all","wait_ms":10000}`, remote.ID)),
		agentCall("tu_3", `{"op":"poll"}`),
		answer("done"))
	srv.providers = &stubResolver{p: prov}
	settle(t, srv, prov)
	spawnedInPoll(t, srv, lead, []string{"r_done", remote.ID})
	appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildResult), providers.Event{Type: providers.EventSpawnChildResult,
		SpawnChild: &providers.SpawnChildEventInfo{RunID: "r_done", Agent: "worker", Mode: "poll", BatchID: "fan_1", Ok: true, Ended: "completed", Output: "answer of r_done"}})
	markPaused(t, srv, lead)

	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d runs, want the lead (warnings: %v)", n, warns)
	}
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	calls := prov.leadCalls()
	if len(calls) != 3 {
		t.Fatalf("the lead made %d calls, want 3", len(calls))
	}
	if byID := lastToolText(calls[1]); !strings.Contains(byID, "remote answer") || !strings.Contains(byID, `"state":"completed"`) {
		t.Errorf("poll by id = %s, want the remote child's answer from its run row", byID)
	}
	bare := lastToolText(calls[2])
	if !strings.Contains(bare, "answer of r_done") || strings.Contains(bare, "remote answer") {
		t.Errorf("bare poll = %s, want only the child not read yet", bare)
	}
}

// A restored child running on another replica is waited for there: the
// parent learns of its end from the run-state bus (the cluster backplane
// re-publishes a remote run's end on it) without polling the store — the
// recheck here is an hour off — and, with no bus event, from the store
// recheck alone.
func TestResumePausedRuns_LearnsOfAChildsEndOnAnotherReplica(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recheck time.Duration
		publish bool
	}{
		{"run-state bus", time.Hour, true},
		{"store recheck", 20 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prov := newBGFamily(agentCall("tu_2", `{"op":"poll"}`), answer("done"))
			srv, _ := makeServer(t, prov, bgResumeConfig(""))
			bus := runstate.NewBus()
			srv.SetRunStateBus(bus)
			srv.childRecheckFirst = tc.recheck
			settle(t, srv, prov)
			lead := pausedLead(t, srv)
			remote := workerRun(t, srv, lead, "a_remote", "elsewhere", false)
			spawnedInPoll(t, srv, lead, []string{remote.ID})
			waitedFor(t, srv, lead, remote.ID)
			markPaused(t, srv, lead)
			if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
				t.Fatalf("resumed %d, want the lead (warnings: %v)", n, warns)
			}
			waitFor(t, "the lead to wait again", func() bool {
				return len(leadEvents(t, srv, lead, providers.EventAwaitingChildren)) == 2
			})
			time.Sleep(100 * time.Millisecond) // past the watcher's first read
			if err := srv.store.FinishRun(ctx, remote.ID, store.RunCompleted, "end_turn",
				store.Usage{Result: json.RawMessage(`{"final_text":"remote answer"}`)}, ""); err != nil {
				t.Fatal(err)
			}
			if tc.publish {
				bus.Publish(runstate.RunStateEvent{RunID: remote.ID, UserID: "alice", Status: string(store.RunCompleted)})
			}
			waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
			calls := prov.leadCalls()
			if len(calls) != 2 || !strings.Contains(lastToolText(calls[1]), "remote answer") {
				t.Fatalf("calls = %d; poll = %q; want the remote child's answer", len(calls), lastToolText(calls[len(calls)-1]))
			}
		})
	}
}

// clusterCancels records the cancels the registry hands to the cluster.
type clusterCancels struct {
	mu   sync.Mutex
	sent map[string]string // agent id → reason
}

func (c *clusterCancels) CancelRemote(_ context.Context, agentID, reason string) (cancel.CancelResult, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent[agentID] = reason
	return cancel.CancelResult{Cancelled: true, Reason: reason}, true, nil
}

func (c *clusterCancels) reason(agentID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.sent[agentID]
	return r, ok
}

// Cancelling a resumed parent cancels its restored children wherever they run:
// one resumed here through the registry's cascade, one running on another
// replica by its run's id, through the cluster.
func TestResumePausedRuns_CancellingAResumedParentCancelsItsChildrenWhereverTheyRun(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily()
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	cluster := &clusterCancels{sent: map[string]string{}}
	srv.cancelReg.SetClusterCanceller(cluster)
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	local := workerRun(t, srv, lead, "a_local", "hold here", true)
	remote := workerRun(t, srv, lead, "a_remote", "elsewhere", false)
	spawnedInPoll(t, srv, lead, []string{local.ID, remote.ID})
	waitedFor(t, srv, lead, local.ID, remote.ID)
	markPaused(t, srv, lead)
	if n, warns := srv.ResumePausedRuns(ctx); n != 2 {
		t.Fatalf("resumed %d, want the lead and the local child (warnings: %v)", n, warns)
	}
	waitFor(t, "the lead to wait again and the local child to run", func() bool {
		_, running := srv.cancelReg.Get("a_local")
		return running && len(leadEvents(t, srv, lead, providers.EventAwaitingChildren)) == 2
	})

	if _, ok := srv.cancelReg.Cancel("a_lead", "operator stop"); !ok {
		t.Fatal("the resumed lead is not cancellable")
	}
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCancelled)
	waitWalkRunStatus(t, srv.store, local.ID, store.RunCancelled)
	waitFor(t, "the remote child's cancel to reach the cluster", func() bool {
		_, ok := cluster.reason("a_remote")
		return ok
	})
	if r, _ := cluster.reason("a_remote"); !strings.Contains(r, "operator stop") {
		t.Errorf("remote child cancelled with %q, want the parent's reason", r)
	}
}
