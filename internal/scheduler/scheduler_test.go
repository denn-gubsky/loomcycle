package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// fakeRunner records every RunOnce call. Returns runErr unconditionally.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []runner.RunInput
	runErr error
	// onRun is called inside RunOnce. Tests use it to inject delay or
	// inspect the in-flight call.
	onRun func(in runner.RunInput)
}

func (f *fakeRunner) RunOnce(_ context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.mu.Unlock()
	if f.onRun != nil {
		f.onRun(in)
	}
	if cb.OnRegistered != nil {
		cb.OnRegistered("a_test", "r_test", "", "")
	}
	return f.runErr
}

func (f *fakeRunner) Calls() []runner.RunInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]runner.RunInput, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeMCP records every Call invocation.
type fakeMCP struct {
	mu    sync.Mutex
	calls []struct {
		Server string
		Tool   string
		Args   map[string]any
	}
	err error
}

func (f *fakeMCP) Call(_ context.Context, server, tool string, args map[string]any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		Server string
		Tool   string
		Args   map[string]any
	}{server, tool, args})
	return json.RawMessage(`{"ok":true}`), f.err
}

func (f *fakeMCP) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// schedulerFixture spins up an in-memory sqlite + seeded schedule +
// fake runner + Scheduler ready to test. Returns the scheduler and
// the seeded def_id.
func schedulerFixture(t *testing.T, def scheduleDef, nextRunAt time.Time) (*Scheduler, *fakeRunner, *fakeMCP, string, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	defID := "sd-test"
	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	ctx := context.Background()
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID:      defID,
		Name:       "sched-test",
		Definition: defJSON,
	}); err != nil {
		t.Fatalf("def create: %v", err)
	}
	if err := st.ScheduleDefSetActive(ctx, "sched-test", defID, "test"); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if err := st.ScheduleRunStateSeed(ctx, defID, nextRunAt); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	fr := &fakeRunner{}
	fm := &fakeMCP{}
	cfg := Config{TickInterval: 10 * time.Millisecond, FireTimeout: 5 * time.Second}
	sched := New(cfg, st, fr, nil, fm, t.Logf)
	return sched, fr, fm, defID, st
}

// fireT triggers one sweeper tick synchronously. Faster + more
// deterministic than starting the goroutine.
func fireT(t *testing.T, s *Scheduler) {
	t.Helper()
	s.tick(context.Background())
}

func TestScheduler_DueScheduleFires(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Agent:    "researcher",
		Schedule: "0 * * * *", // hourly
		Enabled:  &enabled,
	}
	sched, fr, _, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	if got := len(fr.Calls()); got != 1 {
		t.Fatalf("RunOnce calls = %d, want 1", got)
	}
	if fr.Calls()[0].Agent != "researcher" {
		t.Errorf("agent = %q, want researcher", fr.Calls()[0].Agent)
	}
}

func TestScheduler_NotYetDue(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, fr, _, _, _ := schedulerFixture(t, def, time.Now().Add(1*time.Hour))

	fireT(t, sched)
	if got := len(fr.Calls()); got != 0 {
		t.Errorf("RunOnce calls = %d, want 0 (not yet due)", got)
	}
}

func TestScheduler_DisabledDefSkipped(t *testing.T) {
	disabled := false
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &disabled}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	if got := len(fr.Calls()); got != 0 {
		t.Errorf("RunOnce calls = %d, want 0 (disabled)", got)
	}
	// next_run_at must have advanced so the row doesn't re-present.
	got, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if got.NextRunAt.Before(time.Now()) {
		t.Errorf("next_run_at = %v, expected future (skip-but-advance)", got.NextRunAt)
	}
	if got.LastStatus != "skipped_disabled" {
		t.Errorf("last_status = %q, want skipped_disabled", got.LastStatus)
	}
}

func TestScheduler_RecordsCompletion(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, _, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	got, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if got.LastStatus != "completed" {
		t.Errorf("last_status = %q, want completed", got.LastStatus)
	}
	if got.LastRunID != "r_test" {
		t.Errorf("last_run_id = %q, want r_test", got.LastRunID)
	}
}

func TestScheduler_RecordsFailure(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	fr.runErr = errors.New("provider exploded")

	fireT(t, sched)
	got, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if got.LastStatus != "failed" {
		t.Errorf("last_status = %q, want failed", got.LastStatus)
	}
	if !strings.Contains(got.LastError, "provider exploded") {
		t.Errorf("last_error = %q, want to contain 'provider exploded'", got.LastError)
	}
}

func TestScheduler_BackpressureRecordsAsSkipped(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	fr.runErr = runner.ErrBackpressure

	fireT(t, sched)
	got, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if got.LastStatus != "skipped" {
		t.Errorf("backpressure → status = %q, want skipped", got.LastStatus)
	}
}

func TestScheduler_OnCompleteChannelPublish(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Agent:    "researcher",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		UserID:   "alice",
		OnComplete: []scheduleHook{
			{Kind: "channel.publish", Channel: "results-alice", Payload: map[string]any{"top": 3}},
		},
	}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	// Verify the channel got the message.
	if got := channelMessageCount(t, st, "results-alice"); got != 1 {
		t.Errorf("channel message count = %d, want 1", got)
	}
}

// channelMessageCount counts messages on a named channel by walking
// the global ChannelStats result. Inline helper because there's no
// per-channel stats accessor in v1.
func channelMessageCount(t *testing.T, st store.Store, channel string) int64 {
	t.Helper()
	all, err := st.ChannelStats(context.Background())
	if err != nil {
		t.Fatalf("channel stats: %v", err)
	}
	for _, c := range all {
		if c.Channel == channel {
			return c.MessageCount
		}
	}
	return 0
}

func TestScheduler_OnCompleteMemorySet(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Agent:    "researcher",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		UserID:   "alice",
		OnComplete: []scheduleHook{
			{Kind: "memory.set", Scope: "user", Key: "last_search", Payload: map[string]any{"count": 7}},
		},
	}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	got, err := st.MemoryGet(context.Background(), store.MemoryScopeUser, "alice", "last_search")
	if err != nil {
		t.Fatalf("memory get: %v", err)
	}
	if !strings.Contains(string(got.Value), `"count":7`) {
		t.Errorf("memory value = %s, want to contain count:7", string(got.Value))
	}
}

func TestScheduler_OnCompleteMCPCall(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Agent:    "researcher",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		OnComplete: []scheduleHook{
			{Kind: "mcp.call", Server: "telegram", Tool: "send_message",
				Args: map[string]any{"chat_id": 123, "text": "done"}},
		},
	}
	sched, _, fm, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	if got := fm.Count(); got != 1 {
		t.Errorf("MCP calls = %d, want 1", got)
	}
}

func TestScheduler_OnCompleteSkippedOnFailedRun(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Agent:    "researcher",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		OnComplete: []scheduleHook{
			{Kind: "channel.publish", Channel: "should-not-fire"},
		},
	}
	sched, fr, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	fr.runErr = errors.New("boom")

	fireT(t, sched)
	if got := channelMessageCount(t, st, "should-not-fire"); got != 0 {
		t.Errorf("on_complete should not fire on failed run; got %d messages", got)
	}
}

func TestScheduler_AdvancesNextRunAt(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, _, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	got, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if !got.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v, expected future", got.NextRunAt)
	}
}

func TestScheduler_StartStop(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, fr, _, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	// Use a fast tick to make the assertion deterministic.
	sched.cfg.TickInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)
	defer sched.Stop()

	// Wait up to 500ms for the first fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(fr.Calls()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("scheduler never fired (waited 500ms)")
}

// TestScheduler_ConcurrentTickIsSerial verifies that multiple ticks
// don't fire the same schedule multiple times within one due-window
// (counts how many RunOnce calls happen during back-to-back ticks
// after a single fire). With the current single-tick design, a
// schedule fires once + advances next_run_at, so the second tick
// finds nothing due. Catches the regression where state-update
// would fail and the row stays due.
func TestScheduler_ConcurrentTickIsSerial(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, fr, _, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)
	fireT(t, sched)
	fireT(t, sched)
	if got := len(fr.Calls()); got != 1 {
		t.Errorf("got %d fires across 3 ticks, want 1 (post-fire next_run_at must be in the future)", got)
	}
}

// TestScheduler_RecordResultSurvivesParentCancellation regresses
// v1.x review finding #5: if the parent ctx is cancelled mid-fire
// (after listDue but before RecordResult), the original code called
// ScheduleRunStateRecordResult with the cancelled ctx and the store
// write failed silently — leaving next_run_at unadvanced, causing
// immediate re-fire on restart. Fix: when ctx.Err() != nil at
// RecordResult time, use a 5s background ctx for the store write.
//
// The shutdown race is: listDue succeeds (ctx still alive) → tick
// enters fireOne → starts RunOnce → ctx cancellation arrives (e.g.
// OS shutdown) → RunOnce returns Canceled → fireOne reaches the
// RecordResult call with ctx already done. Test uses the
// fakeRunner's onRun hook to cancel the parent ctx mid-fire,
// exactly replicating this sequence.
func TestScheduler_RecordResultSurvivesParentCancellation(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the parent ctx INSIDE RunOnce — listDue already ran
	// with a live ctx, so the row was fetched. Then RecordResult
	// must use the survival ctx to land the next_run_at advance.
	fr.onRun = func(_ runner.RunInput) {
		cancel()
	}

	sched.tick(ctx)

	// next_run_at must be in the future even though parent ctx is
	// dead. If the fix is missing, the row's next_run_at stays in
	// the past (the seeded "1 minute ago" value) and the schedule
	// re-fires immediately on restart.
	got, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !got.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v, expected future — RecordResult didn't survive ctx cancellation, schedule would re-fire immediately on restart", got.NextRunAt)
	}
}

// TestScheduler_DecodeFailureRecordedAsFailed plants a malformed JSON
// body in a schedule_run_state row + checks the sweeper records it as
// failed without crashing.
func TestScheduler_DecodeFailureRecordedAsFailed(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	// Create with a deliberately malformed Definition.
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID:      "sd-malformed",
		Name:       "sched-malformed",
		Definition: json.RawMessage(`{this is not json`),
	}); err != nil {
		t.Fatalf("def create: %v", err)
	}
	_ = st.ScheduleDefSetActive(ctx, "sched-malformed", "sd-malformed", "test")
	_ = st.ScheduleRunStateSeed(ctx, "sd-malformed", time.Now().Add(-1*time.Minute))

	sched := New(Config{TickInterval: 10 * time.Millisecond}, st, &fakeRunner{}, nil, nil, t.Logf)
	fireT(t, sched)

	got, _ := st.ScheduleRunStateGet(ctx, "sd-malformed")
	if got.LastStatus != "decode_def" {
		t.Errorf("last_status = %q, want decode_def", got.LastStatus)
	}
}

// ensure no goroutine leaks across tests.
var _ = atomic.Int32{}

// TestScheduler_TickFiresInParallel verifies the v0.12.7 concurrency
// change: when N due rows are present at tick time, they fire in
// parallel up to MaxConcurrentFires goroutines. The test stages N rows
// with a fake runner that blocks for ~100ms each; if firing were
// serial, the wall would be ~N*100ms. With concurrent fire and 8 slots
// the wall should be roughly N/8 * 100ms — the test asserts that
// observed wall < (serial-wall * 0.5) as a robust bound that doesn't
// flake under loaded CI.
func TestScheduler_TickFiresInParallel(t *testing.T) {
	const N = 24
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	defJSON, _ := json.Marshal(def)
	for i := 0; i < N; i++ {
		defID := fmt.Sprintf("sd-par-%03d", i)
		name := fmt.Sprintf("sched-par-%03d", i)
		if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: defID, Name: name, Definition: defJSON}); err != nil {
			t.Fatalf("def create %d: %v", i, err)
		}
		_ = st.ScheduleDefSetActive(ctx, name, defID, "test")
		_ = st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute))
	}
	const perFireDelay = 100 * time.Millisecond
	fr := &fakeRunner{onRun: func(_ runner.RunInput) { time.Sleep(perFireDelay) }}
	sched := New(Config{TickInterval: time.Hour, MaxConcurrentFires: 8}, st, fr, nil, nil, t.Logf)

	start := time.Now()
	sched.tick(ctx)
	wall := time.Since(start)
	serialWall := time.Duration(N) * perFireDelay
	if wall > serialWall/2 {
		t.Errorf("tick wall = %v, want < %v (serial would be %v); parallel-fire change appears to have regressed",
			wall, serialWall/2, serialWall)
	}
	if got := len(fr.Calls()); got != N {
		t.Errorf("got %d fires, want %d (some due rows were not fired)", got, N)
	}
}

// TestScheduler_TickRespectsMaxConcurrentFires verifies the slot
// semaphore actually bounds in-flight fires. Sets MaxConcurrentFires=4
// against 12 due rows + per-fire 80ms delay, then asserts wall is at
// least 3 * delay (i.e., 3 batches required to drain).
func TestScheduler_TickRespectsMaxConcurrentFires(t *testing.T) {
	const N = 12
	const cap = 4
	const perFireDelay = 80 * time.Millisecond
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	defJSON, _ := json.Marshal(def)
	for i := 0; i < N; i++ {
		defID := fmt.Sprintf("sd-cap-%03d", i)
		name := fmt.Sprintf("sched-cap-%03d", i)
		_, _ = st.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: defID, Name: name, Definition: defJSON})
		_ = st.ScheduleDefSetActive(ctx, name, defID, "test")
		_ = st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute))
	}
	fr := &fakeRunner{onRun: func(_ runner.RunInput) { time.Sleep(perFireDelay) }}
	sched := New(Config{TickInterval: time.Hour, MaxConcurrentFires: cap}, st, fr, nil, nil, t.Logf)

	start := time.Now()
	sched.tick(ctx)
	wall := time.Since(start)
	// 12 fires / 4 cap = 3 batches minimum. Allow some slack on the
	// lower bound (single batch should NOT drain everything).
	minWall := 3*perFireDelay - 20*time.Millisecond
	if wall < minWall {
		t.Errorf("tick wall = %v, want >= %v (cap=%d should serialise into 3 batches)",
			wall, minWall, cap)
	}
}

// TestScheduler_PanicInFireOneIsRecovered ensures a panic inside one
// fire doesn't propagate up to kill the sweeper. The fake runner
// panics on the first call; subsequent due rows still fire normally.
func TestScheduler_PanicInFireOneIsRecovered(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	defJSON, _ := json.Marshal(def)
	for i := 0; i < 3; i++ {
		defID := fmt.Sprintf("sd-panic-%d", i)
		name := fmt.Sprintf("sched-panic-%d", i)
		_, _ = st.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: defID, Name: name, Definition: defJSON})
		_ = st.ScheduleDefSetActive(ctx, name, defID, "test")
		_ = st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute))
	}
	var calls atomic.Int32
	fr := &fakeRunner{onRun: func(_ runner.RunInput) {
		n := calls.Add(1)
		if n == 1 {
			panic("test panic — should be recovered")
		}
	}}
	sched := New(Config{TickInterval: time.Hour, MaxConcurrentFires: 8}, st, fr, nil, nil, t.Logf)

	// The tick should return without panicking even though one
	// goroutine panicked. Survivors must still record results.
	sched.tick(ctx)
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3 (panic-recovery should allow the other fires to complete)", got)
	}
}

// TestScheduler_InFlightSuppressesDoubleFire regresses the x30000
// compound-test ceiling: when a fire takes longer than the tick
// interval, the previous fire's RecordResult hasn't yet advanced
// next_run_at, so the same row appears in the NEXT tick's due list
// and fires AGAIN. Before the in-flight tracker fix, this produced
// 2× MCP call counts at compound test scale=30000.
//
// Test shape: one schedule due in the past + a fake runner that
// blocks for 300ms + back-to-back tick() calls 50ms apart. Without
// the in-flight tracker, every tick during the in-flight window
// re-fires the same schedule. With the tracker, only the FIRST
// tick fires; subsequent ticks see the def in s.inFlight and skip.
func TestScheduler_InFlightSuppressesDoubleFire(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	defJSON, _ := json.Marshal(def)
	const defID = "sd-double-fire"
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID:      defID,
		Name:       "sched-double-fire",
		Definition: defJSON,
	}); err != nil {
		t.Fatalf("def create: %v", err)
	}
	_ = st.ScheduleDefSetActive(ctx, "sched-double-fire", defID, "test")
	_ = st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute))

	const perFireDelay = 300 * time.Millisecond
	var calls atomic.Int32
	fr := &fakeRunner{onRun: func(_ runner.RunInput) {
		calls.Add(1)
		time.Sleep(perFireDelay)
	}}
	sched := New(Config{TickInterval: time.Hour, MaxConcurrentFires: 8}, st, fr, nil, nil, t.Logf)

	// First tick fires the schedule asynchronously (via the goroutine
	// pool). The fire takes 300ms; meanwhile we issue more ticks at
	// 50ms intervals. WITHOUT the in-flight tracker, each of these
	// ticks would re-fire the same schedule (the listDue query still
	// returns it because next_run_at hasn't advanced yet).
	go sched.tick(ctx)
	for i := 0; i < 4; i++ {
		time.Sleep(50 * time.Millisecond)
		sched.tick(ctx)
	}
	// Wait for the in-flight fire to finish.
	time.Sleep(perFireDelay + 100*time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (in-flight tracker should have suppressed re-fires while the first fire was running)", got)
	}
}
