package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// holdRecorder is a hold observer that records every report.
type holdRecorder struct {
	mu  sync.Mutex
	got []bool
}

func (h *holdRecorder) observe(held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, held)
}

func (h *holdRecorder) reports() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.got...)
}

// TestWaitProviderSlot_StopsTheClockWithoutReportingAHold — a sub-agent queued
// on a capped provider reports held(true) to the CLOCK observer on its ctx before
// it waits and held(false) when the wait ends, however it ends, so the wait does
// not count against its timeout_ms. It never reports to the review-hold
// observer: a walk's poll table would show a person deciding. An uncapped
// provider and the ancestor-held carve-out never queue, so they report nothing.
func TestWaitProviderSlot_StopsTheClockWithoutReportingAHold(t *testing.T) {
	gates := concurrency.NewProviderGates(map[string]int{"P": 1}, 0)
	s := &Server{providerGates: gates}
	other, err := gates.Acquire(context.Background(), "P")
	if err != nil {
		t.Fatalf("take P: %v", err)
	}

	// Admitted after a wait: the clock saw true, then false; the hold observer
	// saw nothing.
	rec, holds := &holdRecorder{}, &holdRecorder{}
	ctx := teamrun.WithClockObserver(teamrun.WithHoldObserver(context.Background(), holds.observe), rec.observe)
	defer func() {
		if got := holds.reports(); len(got) != 0 {
			t.Errorf("review-hold observer got %v, want nothing: a queue wait is not a hold", got)
		}
	}()
	type result struct {
		slot *providerSlot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		slot, err := s.waitProviderSlot(ctx, "P")
		done <- result{slot, err}
	}()
	waitForGate(t, gates, "P", 1, 1, 2*time.Second)
	if got := rec.reports(); !reflect.DeepEqual(got, []bool{true}) {
		t.Fatalf("while queued: reports = %v, want [true]", got)
	}
	other()
	r := <-done
	if r.err != nil {
		t.Fatalf("waitProviderSlot: %v", r.err)
	}
	if got := rec.reports(); !reflect.DeepEqual(got, []bool{true, false}) {
		t.Errorf("after admission: reports = %v, want [true false]", got)
	}

	// Ended by ctx while queued: still reported false, and nothing leaks.
	rec2 := &holdRecorder{}
	cctx, cancel := context.WithCancel(teamrun.WithClockObserver(context.Background(), rec2.observe))
	errc := make(chan error, 1)
	go func() {
		_, err := s.waitProviderSlot(cctx, "P")
		errc <- err
	}()
	waitForGate(t, gates, "P", 1, 1, 2*time.Second)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled wait returned no error")
	}
	if got := rec2.reports(); !reflect.DeepEqual(got, []bool{true, false}) {
		t.Errorf("cancelled wait: reports = %v, want [true false]", got)
	}
	r.slot.releaseCurrent()
	if st := gates.Stats()["P"]; st.Active != 0 || st.Queued != 0 {
		t.Errorf("final P: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}

	// Uncapped and carve-out: no queue, so no hold.
	rec3 := &holdRecorder{}
	ctx3 := teamrun.WithClockObserver(context.Background(), rec3.observe)
	if _, err := s.waitProviderSlot(ctx3, "uncapped"); err != nil {
		t.Fatalf("uncapped: %v", err)
	}
	slot, err := s.waitProviderSlot(withHeldProviderSlot(ctx3, "P"), "P")
	if err != nil || !slot.noop {
		t.Fatalf("ancestor-held P: slot=%+v err=%v, want the noop carve-out", slot, err)
	}
	if got := rec3.reports(); len(got) != 0 {
		t.Errorf("uncapped / carve-out reported holds %v, want none", got)
	}
}

// TestProviderSlot_SwapWaitsForNewProviderSlot — a mid-run fallback to a
// saturated capped provider waits for that provider's slot (bounded only by
// ctx) instead of proceeding uncapped, and then holds it. Before, the swap
// tried for 2 s (here: at once, the queue being full) and ran UNCAPPED.
func TestProviderSlot_SwapWaitsForNewProviderSlot(t *testing.T) {
	gates := concurrency.NewProviderGates(map[string]int{"P": 1, "Q": 1}, 0)
	s := &Server{providerGates: gates}
	slot, err := s.acquireProviderSlot(context.Background(), "P")
	if err != nil {
		t.Fatalf("acquire P: %v", err)
	}
	other, err := gates.Acquire(context.Background(), "Q")
	if err != nil {
		t.Fatalf("take Q: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		slot.swap(context.Background(), gates, "Q")
	}()
	waitForGate(t, gates, "Q", 1, 1, 2*time.Second)
	if st := gates.Stats()["P"]; st.Active != 0 {
		t.Errorf("P active = %d while swapping, want 0 (old slot released first)", st.Active)
	}
	select {
	case <-done:
		t.Fatal("swap returned while Q was still saturated (ran uncapped)")
	case <-time.After(150 * time.Millisecond):
	}
	other()
	<-done
	if st := gates.Stats()["Q"]; st.Active != 1 || st.Queued != 0 {
		t.Fatalf("after swap: Q active=%d queued=%d, want 1/0 (the run holds Q's slot)", st.Active, st.Queued)
	}
	slot.releaseCurrent()
	if st := gates.Stats()["Q"]; st.Active != 0 {
		t.Errorf("after release: Q active=%d, want 0", st.Active)
	}

	// ctx ending while the swap queues: the swap returns, the holder holds
	// nothing, and the gate's counts stay balanced.
	slot2, err := s.acquireProviderSlot(context.Background(), "P")
	if err != nil {
		t.Fatalf("acquire P again: %v", err)
	}
	other2, err := gates.Acquire(context.Background(), "Q")
	if err != nil {
		t.Fatalf("take Q again: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		slot2.swap(ctx, gates, "Q")
	}()
	waitForGate(t, gates, "Q", 1, 1, 2*time.Second)
	cancel()
	<-done2
	if slot2.providerID != "Q" {
		t.Errorf("holder providerID = %q, want Q", slot2.providerID)
	}
	slot2.releaseCurrent() // holds nothing; must not release other2's slot
	if st := gates.Stats()["Q"]; st.Active != 1 || st.Queued != 0 {
		t.Errorf("after a cancelled swap: Q active=%d queued=%d, want 1/0", st.Active, st.Queued)
	}
	other2()
}

// TestProviderSlot_SwapToAncestorHeldProviderRunsUngated — a run whose
// fallback target is a provider an ANCESTOR holds must not wait behind that
// slot: the ancestor is blocked awaiting this run, so the wait would never end.
// It runs ungated on the target, as the spawn-time carve-out does.
func TestProviderSlot_SwapToAncestorHeldProviderRunsUngated(t *testing.T) {
	gates := concurrency.NewProviderGates(map[string]int{"P": 1, "Q": 1}, 0)
	s := &Server{providerGates: gates}
	ancestor, err := gates.Acquire(context.Background(), "Q")
	if err != nil {
		t.Fatalf("ancestor takes Q: %v", err)
	}
	defer ancestor()
	slot, err := s.acquireProviderSlot(context.Background(), "P")
	if err != nil {
		t.Fatalf("acquire P: %v", err)
	}

	ctx := withHeldProviderSlot(context.Background(), "Q")
	done := make(chan struct{})
	go func() {
		defer close(done)
		slot.swap(ctx, gates, "Q")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("swap blocked behind a slot its own ancestor holds (deadlock)")
	}
	if gP, gQ := gates.Stats()["P"], gates.Stats()["Q"]; gP.Active != 0 || gQ.Active != 1 || gQ.Queued != 0 {
		t.Errorf("P active=%d, Q active=%d queued=%d; want 0, 1 (the ancestor only), 0", gP.Active, gQ.Active, gQ.Queued)
	}
	if slot.providerID != "Q" || slot.noop {
		t.Errorf("holder = %+v, want providerID Q and not a noop carve-out holder", slot)
	}
	slot.releaseCurrent() // holds nothing on Q
	if st := gates.Stats()["Q"]; st.Active != 1 {
		t.Errorf("releasing the ungated holder changed Q: active=%d, want 1", st.Active)
	}
}

// TestProviderGate_BoundedChildQueueWaitNotCountedAgainstTimeout — end to
// end: a parent spawns a child with timeout_ms=300 whose provider is saturated
// for ~900 ms. The child queues for its slot (it is part of an admitted run
// tree), the queueing does not count against its timeout_ms, and it runs to
// completion once the slot frees. This also proves the hold observer the Agent
// tool sets reaches the provider-gate wait. Before, the child failed after the
// 30 s queue timeout; with the hold reporting removed, it times out at 300 ms.
func TestProviderGate_BoundedChildQueueWaitNotCountedAgainstTimeout(t *testing.T) {
	parentProv := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_spawn", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go","timeout_ms":300}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
		},
	}
	childProv := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "child answered"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Provider: "parent-prov", Model: "m", Tools: []string{"Agent"}, SystemPrompt: "parent"},
		"child":  {Provider: "Q", Model: "m", Tools: []string{}, SystemPrompt: "child"},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "bounded_child_wait.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	gates := concurrency.NewProviderGates(map[string]int{"Q": 1}, 0)
	resolver := &multiResolver{byID: map[string]providers.Provider{"parent-prov": parentProv, "Q": childProv}}
	srv := New(cfg, resolver, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Another run holds Q's only slot for three times the child's timeout_ms.
	other, err := gates.Acquire(context.Background(), "Q")
	if err != nil {
		t.Fatalf("take Q: %v", err)
	}
	var once sync.Once
	releaseOther := func() { once.Do(other) }
	defer releaseOther()
	time.AfterFunc(900*time.Millisecond, releaseOther)

	client := &http.Client{Timeout: 30 * time.Second} // a regression fails, not hangs
	resp, err := client.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"parent-bounded","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "timed out") {
		t.Errorf("the child's queue wait counted against its timeout_ms; body=\n%s", body)
	}
	childRuns, err := st.ListRunsByParentAgentID(context.Background(), "parent-bounded")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(childRuns) != 1 || childRuns[0].Status != store.RunCompleted {
		t.Fatalf("child runs = %+v, want one completed run", childRuns)
	}
	if st := gates.Stats()["Q"]; st.Active != 0 || st.Queued != 0 {
		t.Errorf("final Q: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}
}

// pausedResumerRun files a paused run of agent "resumer" (provider
// "scripted") whose transcript ends on a pending tool_result, ready for
// ResumePausedRuns. Mirrors TestResumePausedRuns_ResumesMidConversationToCompletion.
func pausedResumerRun(t *testing.T, srv *Server) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_resume_wait", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
	})
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{
		Type:    providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read", Input: json.RawMessage(`{"path":"/x"}`)},
	})
	appendResumeEvent(t, srv, run.ID, "tool_result", providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read"},
		Text:    "FILE CONTENTS",
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	return run
}

func resumerServer(t *testing.T) (*Server, *scriptedProvider) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "resumed and done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	return srv, prov
}

// assertRunStillPending fails if the run reached a terminal state.
func assertRunStillPending(t *testing.T, srv *Server, runID string) {
	t.Helper()
	got, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status == store.RunFailed || got.Status == store.RunCompleted {
		t.Fatalf("run reached %q while its slot was taken (err=%q), want it still waiting", got.Status, got.ErrorMsg)
	}
}

// TestResumePausedRuns_SaturatedProviderWaitsInsteadOfFailing — a resumed run
// whose provider gate is saturated (queue full, where a NEW request is
// refused) waits for the slot and completes once it frees. It was admitted
// before its pause; before, it was marked failed "resume: acquire provider
// slot".
func TestResumePausedRuns_SaturatedProviderWaitsInsteadOfFailing(t *testing.T) {
	srv, prov := resumerServer(t)
	gates := concurrency.NewProviderGates(map[string]int{"scripted": 1}, 0)
	srv.SetProviderGates(gates)
	other, err := gates.Acquire(context.Background(), "scripted")
	if err != nil {
		t.Fatalf("take the provider slot: %v", err)
	}
	var once sync.Once
	defer once.Do(other)
	run := pausedResumerRun(t, srv)

	if n, warnings := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	waitForGate(t, gates, "scripted", 1, 1, 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	assertRunStillPending(t, srv, run.ID)

	once.Do(other)
	waitRunCompleted(t, srv, run.ID)
	if prov.calls.Load() < 1 {
		t.Error("provider never called — the loop wasn't resumed")
	}
}

// TestResumePausedRuns_SaturatedGlobalSlotWaitsInsteadOfFailing — the same
// for the global run cap: every slot taken and the queue full, the resumed run
// waits (no queue timeout, no per-user cap) and completes once a slot frees.
// Before, it was marked failed "resume: acquire run slot".
func TestResumePausedRuns_SaturatedGlobalSlotWaitsInsteadOfFailing(t *testing.T) {
	srv, _ := resumerServer(t)
	srv.sem = concurrency.New(1, 0, 10*time.Millisecond).WithPerUserCap(1)
	other, err := srv.sem.AcquireForUser(context.Background(), "", "alice")
	if err != nil {
		t.Fatalf("take the global slot: %v", err)
	}
	var once sync.Once
	defer once.Do(other)
	run := pausedResumerRun(t, srv)

	if n, warnings := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	waitForQueued(t, srv.sem, 1)
	time.Sleep(100 * time.Millisecond) // 10x the semaphore's queue timeout
	assertRunStillPending(t, srv, run.ID)

	once.Do(other)
	waitRunCompleted(t, srv, run.ID)
	if st := srv.sem.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("final global: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}
}

func waitForQueued(t *testing.T, sem *concurrency.Semaphore, want int) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if sem.Stats().Queued == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("global queued = %d, want %d", sem.Stats().Queued, want)
}
