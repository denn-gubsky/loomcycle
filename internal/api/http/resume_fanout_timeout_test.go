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

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A wait-mode parallel_spawn child's started ledger row carries its
// timeout_ms, so a parent resumed from a snapshot can hold the child to it.
func TestResumeFanout_WaitModeStartedRowRecordsTheChildsTimeout(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.ResumeFanout = true
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "parent", MaxConcurrentChildren: 4},
		"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "child"},
	}
	done := func(text string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventText, Text: text},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: json.RawMessage(
			`{"op":"parallel_spawn","spawns":[{"name":"child","prompt":"one","timeout_ms":60000},{"name":"child","prompt":"two"}]}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}, done("child answer"), done("child answer")}, defaultS: done("parent done")}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "fanout_timeout.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"a_fan_timeout","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "parent done") {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	ctx := context.Background()
	parent, err := st.GetRunByAgentID(ctx, "a_fan_timeout")
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.GetTranscript(ctx, parent.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	bounds := map[int]int{}
	for _, e := range events {
		if e.RunID != parent.ID || e.Type != string(providers.EventSpawnChildStarted) {
			continue
		}
		var pe providers.Event
		if json.Unmarshal(e.Payload, &pe) == nil && pe.SpawnChild != nil {
			bounds[pe.SpawnChild.Index] = pe.SpawnChild.TimeoutMs
		}
	}
	if len(bounds) != 2 || bounds[0] != 60000 || bounds[1] != 0 {
		t.Errorf("started rows' timeout_ms by index = %v, want {0:60000 1:0}", bounds)
	}
}

// A wait-mode parallel_spawn child re-dispatched across a snapshot resume is
// held to its timeout_ms again: past the bound, the reconcile cancels its run
// as timed out and answers its row as the live call would have — rather than
// waiting on it for the 30-minute backstop.
func TestResumeFanout_AWaitModeChildPastItsTimeoutIsCutAndAnsweredTimedOut(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"breeder": {Provider: "scripted", Model: "stub-model", Tools: []string{"Agent"}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = true
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	// Bounded: a reconcile that does not re-arm the bound would wait here
	// until this ctx ends, not for its 30-minute backstop.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	childSess, _ := srv.store.CreateSession(ctx, "", "solver", "alice")
	child, err := srv.store.CreateRun(ctx, childSess.ID, store.RunIdentity{AgentID: "a_child_bounded", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var cause error
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: child.AgentID, RunID: child.ID, SessionID: childSess.ID, UserID: "alice", StartedAt: time.Now()},
		func(c error) { mu.Lock(); cause = c; mu.Unlock() }); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(providers.Event{
		Type:       providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: child.ID, Agent: "solver", TimeoutMs: 1},
	})
	events := []store.Event{{Type: string(providers.EventSpawnChildStarted), Payload: payload}}
	msg, err := srv.reconcileFanoutParent(ctx, store.Run{ID: "r_parent"}, events,
		fanoutParkInfo{toolUseID: "tu_fan", input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"x","timeout_ms":1}]}`)},
		func(providers.Event) {})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(msg.Content[0].Text), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if len(env.Results) != 1 {
		t.Fatalf("envelope = %s", msg.Content[0].Text)
	}
	r := env.Results[0]
	if r.Ok || r.Status != "timeout" || r.RunID != child.ID || !strings.Contains(r.Error, "timed out: timeout_ms=1") {
		t.Errorf("row = %+v, want the child timed out", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if cause == nil || !strings.Contains(cancel.ReasonFromCause(cause), "timeout_ms=1") {
		t.Errorf("child's cancel cause = %v, want its timeout", cause)
	}
}

// The fan-out twin of the restored poll-mode child: a wait-mode child the
// reconcile re-armed is not cut while it is parked for a runtime pause on the
// instance it runs on, and runs out only the time it had left once that
// instance resumes.
func TestResumeFanout_AWaitModeChildIsNotCutWhileItIsPaused(t *testing.T) {
	const bound = 800 * time.Millisecond
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"breeder": {Provider: "scripted", Model: "stub-model", Tools: []string{"Agent"}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = true
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()

	childSess, _ := srv.store.CreateSession(ctx, "", "solver", "alice")
	child, err := srv.store.CreateRun(ctx, childSess.ID, store.RunIdentity{AgentID: "a_child_paused", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: child.AgentID, RunID: child.ID, SessionID: childSess.ID, UserID: "alice", StartedAt: time.Now()},
		func(error) {}); err != nil {
		t.Fatal(err)
	}
	elsewhere := pause.NewManager(srv.store, time.Second)
	pausedAt := time.Now()
	if _, err := elsewhere.Pause(ctx, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	parkFor(t, srv, elsewhere, child)

	payload, _ := json.Marshal(providers.Event{
		Type:       providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: child.ID, Agent: "solver", TimeoutMs: int(bound / time.Millisecond)},
	})
	events := []store.Event{{Type: string(providers.EventSpawnChildStarted), Payload: payload}}
	type outcome struct {
		msg providers.Message
		err error
		at  time.Time
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := srv.reconcileFanoutParent(ctx, store.Run{ID: "r_parent"}, events,
			fanoutParkInfo{toolUseID: "tu_fan", input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"x","timeout_ms":800}]}`)},
			func(providers.Event) {})
		done <- outcome{msg, err, time.Now()}
	}()
	select {
	case o := <-done:
		t.Fatalf("the reconcile answered %v after the child started, while it was paused: %+v (%v)", o.at.Sub(child.StartedAt), o.msg, o.err)
	case <-time.After(time.Until(child.StartedAt.Add(2 * bound))):
	}
	resumedAt := time.Now()
	if _, err := elsewhere.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	o := <-done
	if o.err != nil {
		t.Fatalf("reconcile: %v", o.err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(o.msg.Content[0].Text), &env); err != nil || len(env.Results) != 1 {
		t.Fatalf("envelope = %s (%v)", o.msg.Content[0].Text, err)
	}
	if r := env.Results[0]; r.Ok || r.Status != "timeout" {
		t.Fatalf("row = %+v, want the child timed out once it ran again", r)
	}
	want := child.StartedAt.Add(bound + resumedAt.Sub(pausedAt))
	if early, late := o.at.Sub(want), o.at.Sub(want.Add(time.Second)); early < -150*time.Millisecond || late > 0 {
		t.Errorf("the child was cut %v after the resume, want about %v (what it had left when the runtime paused)",
			o.at.Sub(resumedAt), want.Sub(resumedAt))
	}
}

// The re-armed bound runs from the child's start and does not count the
// time the child was held for a verdict; while a hold is open it never runs
// out.
func TestResumedChildClock_HeldTimeIsNotCounted(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return start.Add(time.Duration(sec) * time.Second) }
	c := &resumedChildClock{bound: 10 * time.Second}
	if d, held := c.deadline(start); held || !d.Equal(at(10)) {
		t.Fatalf("fresh deadline = %v held=%v, want start+10s", d, held)
	}
	c.observe([]store.Event{{Seq: 1, Type: string(providers.EventAwaitingReview), Timestamp: at(2)}})
	if _, held := c.deadline(start); !held {
		t.Fatal("an open hold does not stop the clock")
	}
	c.observe([]store.Event{{Seq: 2, Type: "user_input", Timestamp: at(32)}})
	if d, held := c.deadline(start); held || !d.Equal(at(40)) {
		t.Errorf("deadline after a 30s hold = %v held=%v, want start+40s", d, held)
	}
	if c.seq != 2 {
		t.Errorf("events read up to %d, want 2", c.seq)
	}
}

// A recorded pause stops the re-armed clock from when the RUNTIME paused —
// the record's since, not the park — and a hold and a pause that overlap are
// left out once. A pause whose end was never recorded ends at the run's next
// working row.
func TestResumedChildClock_PausedTimeIsNotCounted(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return start.Add(time.Duration(sec) * time.Second) }
	began := func(seq int64, parked, since int) store.Event {
		b, _ := json.Marshal(pauseBeganRecord{Since: at(since)})
		return store.Event{Seq: seq, Type: eventPauseBegan, Timestamp: at(parked), Payload: b}
	}
	c := &resumedChildClock{bound: 10 * time.Second}
	c.observe([]store.Event{
		{Seq: 1, Type: "user_input", Timestamp: at(0)},
		began(2, 3, 2), // the runtime paused at 2; the run parked at 3
	})
	if _, stopped := c.deadline(start); !stopped {
		t.Fatal("an open pause does not stop the clock")
	}
	c.observe([]store.Event{{Seq: 3, Type: eventPauseEnded, Timestamp: at(7)}})
	if d, stopped := c.deadline(start); stopped || !d.Equal(at(15)) {
		t.Fatalf("deadline after a pause from 2 to 7 = %v stopped=%v, want start+15s", d, stopped)
	}
	// A hold, then a pause that began before the hold ended: one stretch.
	c.observe([]store.Event{
		{Seq: 4, Type: string(providers.EventAwaitingReview), Timestamp: at(8)},
		{Seq: 5, Type: "user_input", Timestamp: at(10)},
		began(6, 11, 9), // the runtime paused at 9, while the run was held
		{Seq: 7, Type: eventPauseEnded, Timestamp: at(12)},
	})
	if d, stopped := c.deadline(start); stopped || !d.Equal(at(19)) {
		t.Fatalf("deadline after an overlapping hold and pause (8 to 12) = %v stopped=%v, want start+19s", d, stopped)
	}
	// A pause whose end did not land ends when the run works again.
	c.observe([]store.Event{began(8, 14, 14), {Seq: 9, Type: "text", Timestamp: at(16)}})
	if d, stopped := c.deadline(start); stopped || !d.Equal(at(21)) {
		t.Errorf("deadline after a pause ended by the run's text = %v stopped=%v, want start+21s", d, stopped)
	}
	// A run started during the pause's wind-down: not stopped before its first row.
	late := &resumedChildClock{bound: 10 * time.Second}
	late.observe([]store.Event{{Seq: 1, Type: "user_input", Timestamp: at(5)}, began(2, 6, 1), {Seq: 3, Type: eventPauseEnded, Timestamp: at(8)}})
	if d, _ := late.deadline(at(5)); !d.Equal(at(18)) {
		t.Errorf("deadline of a run started at 5 in a pause from 1 to 8 = %v, want start+13s", d)
	}
}

// A stopped clock is read again no later than it could run out if the stop
// ended now, and never in a tight loop.
func TestResumedChildClock_StoppedClockIsReadBeforeItCouldRunOut(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return start.Add(time.Duration(sec) * time.Second) }
	b, _ := json.Marshal(pauseBeganRecord{Since: at(4)})
	c := &resumedChildClock{bound: 10 * time.Second}
	if got := c.nextCheck(start, at(1)); !got.Equal(at(10)) {
		t.Fatalf("running clock's next check = %v, want its deadline", got)
	}
	c.observe([]store.Event{{Seq: 1, Type: "user_input", Timestamp: at(0)}, {Seq: 2, Type: eventPauseBegan, Timestamp: at(4), Payload: b}})
	if got := c.nextCheck(start, at(20)); !got.Equal(at(26)) {
		t.Errorf("paused clock's next check at 20 = %v, want 20 + the 6s it had left", got)
	}
	spent := &resumedChildClock{bound: time.Second}
	spent.observe([]store.Event{{Seq: 1, Type: "user_input", Timestamp: at(0)}, {Seq: 2, Type: eventPauseBegan, Timestamp: at(4), Payload: b}})
	if got := spent.nextCheck(start, at(20)); got.Sub(at(20)) < stoppedClockRecheckMin {
		t.Errorf("a clock stopped past its bound is read again at %v, want at least %v later", got, stoppedClockRecheckMin)
	}
}
