package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// interruptionFixture builds an Interruption tool backed by a
// SQLite store + a real channels.Bus, with a ctx pre-populated with
// a run row + a permissive policy (Enabled=true, kinds=question).
// Tests override individual ctx values where they want to exercise
// specific gates.
func interruptionFixture(t *testing.T) (*Interruption, context.Context, string, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "qa-agent", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_test",
		UserID:  "alice",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	tool := &Interruption{
		Store:             s,
		Bus:               channels.NewBus(),
		DefaultTimeout:    0, // no implicit timeout
		MaxTimeout:        time.Minute,
		HeartbeatInterval: 0, // ticker disabled by default in tests (RunID empty would also disable)
	}
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithRunID(ctx, run.ID)
	ctx = tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: true})
	return tool, ctx, run.ID, func() { _ = s.Close() }
}

func TestInterruption_DefaultDeny(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()
	// Strip the permissive policy → default-deny.
	ctx = tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: false})

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"hi?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected is_error=true on default-deny, got result: %+v", res)
	}
	if !strings.Contains(res.Text, "not enabled") {
		t.Errorf("expected refusal message; got %q", res.Text)
	}
}

func TestInterruption_AskBlockUntilResolve(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	// Kick off ask in a goroutine — it blocks on bus.Wait.
	resCh := make(chan tools.Result, 1)
	go func() {
		res, err := tool.Execute(ctx, json.RawMessage(`{
			"op":"ask",
			"question":"Proceed with delete?",
			"options":["Yes","No"],
			"timeout_ms":5000
		}`))
		if err != nil {
			t.Errorf("Execute: %v", err)
		}
		resCh <- res
	}()

	// Wait briefly so the create + bus.Wait runs.
	time.Sleep(50 * time.Millisecond)

	// Find the pending row + resolve it (simulating the HTTP
	// resolve handler doing the work).
	pending, err := tool.Store.InterruptListByRun(ctx, tools.RunID(ctx), store.InterruptStatusPending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending interrupt, got %d", len(pending))
	}
	id := pending[0].InterruptID
	if err := tool.Store.InterruptResolve(ctx, id, "Yes", store.InterruptResolvedByWebUI, nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	tool.Bus.Notify("intr:" + id)

	select {
	case res := <-resCh:
		if res.IsError {
			t.Fatalf("expected non-error result; got %+v", res)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("result not JSON: %v (raw %q)", err, res.Text)
		}
		if out["answer"] != "Yes" {
			t.Errorf("answer=%v, want Yes", out["answer"])
		}
		if out["resolved_by"] != store.InterruptResolvedByWebUI {
			t.Errorf("resolved_by=%v", out["resolved_by"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not wake after resolve")
	}
}

func TestInterruption_AskTimeout(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	// 50 ms timeout; no resolve will come.
	start := time.Now()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"Hi?","timeout_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected is_error=true on timeout; got %+v", res)
	}
	if !strings.Contains(res.Text, "timed_out") {
		t.Errorf("expected timed_out marker; got %q", res.Text)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Errorf("timeout fired at %v (expected ~50ms)", d)
	}

	// Storage row should be finalised with status=timed_out.
	rows, _ := tool.Store.InterruptListByRun(ctx, tools.RunID(ctx), store.InterruptStatusTimedOut)
	if len(rows) != 1 {
		t.Errorf("expected 1 timed_out row, got %d", len(rows))
	}
}

func TestInterruption_AskCtxCancelMidBlock(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(ctx)
	resCh := make(chan tools.Result, 1)
	go func() {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"Hi?","timeout_ms":5000}`))
		resCh <- res
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case res := <-resCh:
		if !res.IsError {
			t.Errorf("expected is_error on ctx cancel; got %+v", res)
		}
		if !strings.Contains(res.Text, "cancelled") {
			t.Errorf("expected cancelled marker; got %q", res.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not return after ctx cancel")
	}
}

func TestInterruption_NotifyIsFireAndForget(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	start := time.Now()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"notify","message":"All done.","priority":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("notify blocked for %v; expected near-instant", d)
	}
	if res.IsError {
		t.Errorf("notify returned error: %+v", res)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	if out["status"] != "delivered" {
		t.Errorf("status=%v, want delivered", out["status"])
	}
}

func TestInterruption_Cancel(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	// Kick off ask, then cancel it from another op call.
	resCh := make(chan tools.Result, 1)
	go func() {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"Hi?","timeout_ms":5000}`))
		resCh <- res
	}()
	time.Sleep(30 * time.Millisecond)

	pending, _ := tool.Store.InterruptListByRun(ctx, tools.RunID(ctx), store.InterruptStatusPending)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	id := pending[0].InterruptID

	// Agent-side cancel — should transition the row + notify the
	// blocked waiter to return promptly.
	cres, err := tool.Execute(ctx, json.RawMessage(`{
		"op":"cancel",
		"interruption_id":"`+id+`"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cres.IsError {
		t.Errorf("cancel returned error: %+v", cres)
	}

	select {
	case res := <-resCh:
		if !res.IsError {
			t.Errorf("expected ask to return is_error after cancel; got %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not unblock after cancel op")
	}
}

func TestInterruption_OptionsRejectInvalidAnswerAtResolveLayer(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	// This test runs purely against the tool's create path + the
	// storage layer's validation isn't tested here (that's the
	// HTTP handler's job). What we DO assert: the options list
	// makes it into the stored row so the resolve handler can
	// validate against it.
	resCh := make(chan tools.Result, 1)
	go func() {
		res, _ := tool.Execute(ctx, json.RawMessage(`{
			"op":"ask",
			"question":"Yes or no?",
			"options":["Yes","No"],
			"timeout_ms":2000
		}`))
		resCh <- res
	}()
	time.Sleep(50 * time.Millisecond)

	pending, _ := tool.Store.InterruptListByRun(ctx, tools.RunID(ctx), store.InterruptStatusPending)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	if string(pending[0].Options) == "" || string(pending[0].Options) == "null" {
		t.Errorf("options not persisted: %q", string(pending[0].Options))
	}
	var opts []string
	if err := json.Unmarshal(pending[0].Options, &opts); err != nil || len(opts) != 2 || opts[0] != "Yes" {
		t.Errorf("options round-trip wrong: %v %v", opts, err)
	}
	// Clean up by cancelling the pending row.
	_ = tool.Store.InterruptFinish(ctx, pending[0].InterruptID, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
	tool.Bus.Notify("intr:" + pending[0].InterruptID)
	<-resCh
}

func TestInterruption_MaxPendingEnforced(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()
	// Cap at 1 pending per run.
	ctx = tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: true, MaxPending: 1})

	// First ask — creates pending row + blocks. Run in goroutine.
	resCh := make(chan tools.Result, 1)
	go func() {
		r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"first","timeout_ms":2000}`))
		resCh <- r
	}()
	time.Sleep(50 * time.Millisecond)

	// Second ask — should refuse with max_pending error.
	r2, err := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"second","timeout_ms":2000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.IsError {
		t.Errorf("expected is_error on second ask; got %+v", r2)
	}
	if !strings.Contains(r2.Text, "max_pending") {
		t.Errorf("expected max_pending refusal; got %q", r2.Text)
	}
	// Clean up — cancel the first.
	pending, _ := tool.Store.InterruptListByRun(ctx, tools.RunID(ctx), store.InterruptStatusPending)
	for _, p := range pending {
		_ = tool.Store.InterruptFinish(ctx, p.InterruptID, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
		tool.Bus.Notify("intr:" + p.InterruptID)
	}
	<-resCh
}

func TestInterruption_EmitsPendingSSEEvent(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	// The emitter fires from the goroutine that calls tool.Execute below;
	// the assertion loop runs from the test goroutine. Both touch the
	// `events` slice, so guard it with a mutex — without this, -race
	// flags the unsynchronised slice access (CI run 25993107457).
	var (
		eventsMu sync.Mutex
		events   []providers.Event
	)
	ctx = tools.WithEventEmitter(ctx, func(e providers.Event) {
		eventsMu.Lock()
		events = append(events, e)
		eventsMu.Unlock()
	})

	resCh := make(chan tools.Result, 1)
	go func() {
		r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"Hi?","timeout_ms":2000}`))
		resCh <- r
	}()
	time.Sleep(50 * time.Millisecond)

	// Snapshot under the lock so the iteration below operates on a
	// stable slice header that won't race with further emitter writes.
	eventsMu.Lock()
	snapshot := append([]providers.Event(nil), events...)
	eventsMu.Unlock()

	// At least one EventInterruptionPending must have fired.
	found := false
	for _, e := range snapshot {
		if e.Type == providers.EventInterruptionPending && e.Interruption != nil {
			found = true
			if e.Interruption.Question != "Hi?" {
				t.Errorf("event Question=%q, want Hi?", e.Interruption.Question)
			}
			if e.Interruption.Kind != store.InterruptKindQuestion {
				t.Errorf("event Kind=%q, want question", e.Interruption.Kind)
			}
			break
		}
	}
	if !found {
		t.Error("EventInterruptionPending not emitted")
	}
	// Cleanup.
	pending, _ := tool.Store.InterruptListByRun(ctx, tools.RunID(ctx), store.InterruptStatusPending)
	for _, p := range pending {
		_ = tool.Store.InterruptFinish(ctx, p.InterruptID, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
		tool.Bus.Notify("intr:" + p.InterruptID)
	}
	<-resCh
}

func TestInterruption_RunIDMissingFromCtx(t *testing.T) {
	tool, _, _, cleanup := interruptionFixture(t)
	defer cleanup()
	// Build a ctx with policy but NO RunID — should surface a clear
	// "server wiring bug" error instead of silently creating
	// dangling rows.
	ctx := context.Background()
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: true})

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"ask","question":"Hi?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected is_error on missing RunID; got %+v", res)
	}
	if !strings.Contains(res.Text, "run id missing") {
		t.Errorf("expected run-id error; got %q", res.Text)
	}
}

func TestInterruption_StoreAlreadyTerminalNotPanic(t *testing.T) {
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()

	// Cancel against an already-terminal row returns a non-error
	// tool result with was_pending=false.
	id := store.MintInterruptID(time.Now())
	if _, err := tool.Store.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: id, RunID: tools.RunID(ctx), UserID: "alice", AgentID: "a_test",
		Question: "Hi?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = tool.Store.InterruptFinish(ctx, id, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"cancel","interruption_id":"`+id+`"}`))
	if res.IsError {
		t.Errorf("cancel of already-terminal should NOT be is_error; got %+v", res)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	if out["was_pending"] != false {
		t.Errorf("expected was_pending=false; got %v", out["was_pending"])
	}
}

// Ensure errors.Is / errors.As behave on the new ErrInterruptAlreadyTerminal
// sentinel — surfaces in cancel-of-already-cancelled path.
func TestStoreSentinelErrCheck(t *testing.T) {
	err := store.ErrInterruptAlreadyTerminal
	if !errors.Is(err, store.ErrInterruptAlreadyTerminal) {
		t.Error("errors.Is failed on direct sentinel")
	}
}
