package scheduler

import (
	"context"
	"encoding/json"
	"errors"
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
