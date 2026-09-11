package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// walkRunRecorder stands in for the server's run opener.
type walkRunRecorder struct {
	mu       sync.Mutex
	opened   int
	finished int
	lastErr  error
	detach   bool
}

func (w *walkRunRecorder) open(ctx context.Context, _ string, detach bool) (context.Context, string, func(error), error) {
	w.mu.Lock()
	w.opened++
	w.detach = detach
	w.mu.Unlock()
	// Detached in the same way the server does it, so a test that outlives the
	// caller's ctx behaves like production.
	if detach {
		ctx = context.WithoutCancel(ctx)
	}
	return ctx, "r_walk1", func(err error) {
		w.mu.Lock()
		w.finished++
		w.lastErr = err
		w.mu.Unlock()
	}, nil
}

func (w *walkRunRecorder) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opened, w.finished
}

// TestTeamDefTool_Run_CarriesARunID: a walk is a run, so every run-scoped
// surface has a handle to reach it by. Reported on the synchronous path too —
// a caller holding a second connection can still arm this walk while it runs.
func TestTeamDefTool_Run_CarriesARunID(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["run_id"] != "r_walk1" {
		t.Errorf("run_id = %v, want r_walk1", out["run_id"])
	}
	if opened, finished := rec.counts(); opened != 1 || finished != 1 {
		t.Errorf("opened=%d finished=%d, want 1/1", opened, finished)
	}
	if rec.detach {
		t.Error("a plain run asked for a detached ctx")
	}
}

// TestTeamDefTool_Run_DetachReturnsTheHandleBeforeTheWalkFinishes is the shape
// the canvas needs: over HTTP op=run is synchronous, so without this there is
// no moment at which a caller holds an identity for a walk that is still
// running — nothing to arm, nothing to answer, nothing to watch.
func TestTeamDefTool_Run_DetachReturnsTheHandleBeforeTheWalkFinishes(t *testing.T) {
	tool, ctx, io, _, done := breakFixture(t)
	defer done()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open

	release := make(chan struct{})
	spawned := make(chan struct{}, 8)
	tool.Spawn = func(context.Context, string, teamrun.Prompt, string) (string, error) {
		spawned <- struct{}{}
		<-release // hold the walk open
		return "reviewed", nil
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","mode":"detach"}`))
	if res.IsError {
		t.Fatalf("detach: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["run_id"] != "r_walk1" || out["status"] != "running" {
		t.Fatalf("detach response = %v, want {run_id, status:running}", out)
	}
	// Execute RETURNED while the walk is still in flight — that is the point.
	<-spawned
	if _, finished := rec.counts(); finished != 0 {
		t.Errorf("the walk finished before Execute returned — it was not detached")
	}
	if io.count() != 0 {
		t.Errorf("the walk had already published before the handle came back")
	}
	if !rec.detach {
		t.Error("detach did not ask for a detached ctx — the walk would die with the request")
	}

	close(release)
	waitFor(t, func() bool { _, f := rec.counts(); return f == 1 })
	if io.count() != 2 {
		t.Errorf("the detached walk published %d sink messages, want 2", io.count())
	}
}

// TestTeamDefTool_Run_DetachReleasesBreakpointsWhenTheWALKEnds pins the bug
// that a `defer` would have caused: a detached walk outlives Execute, so
// releasing the armed set on return would unregister it the moment the caller
// got its run id — leaving a running walk nobody could arm.
func TestTeamDefTool_Run_DetachReleasesBreakpointsWhenTheWALKEnds(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	tool.AskHuman = func(context.Context, string) (string, error) { return "continue", nil }

	var mu sync.Mutex
	released := 0
	tool.LiveBreakpoints = func(_ context.Context, seed []string) (teamrun.BreakpointSource, func(), error) {
		src, err := teamrun.NewStaticBreakpoints(seed)
		return src, func() { mu.Lock(); released++; mu.Unlock() }, err
	}
	gate := make(chan struct{})
	tool.Spawn = func(context.Context, string, teamrun.Prompt, string) (string, error) {
		<-gate
		return "reviewed", nil
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","mode":"detach"}`))
	if res.IsError {
		t.Fatalf("detach: %s", res.Text)
	}
	mu.Lock()
	early := released
	mu.Unlock()
	if early != 0 {
		t.Fatal("the armed set was released when Execute returned — a detached walk would be unarmable")
	}
	close(gate)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return released == 1 })
}

// TestTeamDefTool_Run_DetachRefusedWithoutRunTracking: a caller that asked for
// a handle and silently got a completed walk instead has no way to notice.
func TestTeamDefTool_Run_DetachRefusedWithoutRunTracking(t *testing.T) {
	tool, ctx, _, spawned, done := breakFixture(t)
	defer done()
	tool.WalkRun = nil

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","mode":"detach"}`))
	if !res.IsError || !strings.Contains(res.Text, "run tracking") {
		t.Fatalf("want a refusal naming the missing wiring, got IsError=%v: %s", res.IsError, res.Text)
	}
	if *spawned != 0 {
		t.Errorf("a refused detach ran the walk anyway: spawned=%d", *spawned)
	}
}

func TestTeamDefTool_Run_UnknownModeIsRefused(t *testing.T) {
	tool, ctx, _, spawned, done := breakFixture(t)
	defer done()
	tool.WalkRun = (&walkRunRecorder{}).open

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","mode":"bogus"}`))
	if !res.IsError || !strings.Contains(res.Text, "unknown mode") {
		t.Fatalf("want a refusal, got IsError=%v: %s", res.IsError, res.Text)
	}
	if *spawned != 0 {
		t.Errorf("a refused mode ran the walk anyway: spawned=%d", *spawned)
	}
}

// TestTeamDefTool_Run_FinishCarriesTheWalkError: the run row must record WHY a
// walk failed, or a failed detached walk is indistinguishable from one still
// going.
func TestTeamDefTool_Run_FinishCarriesTheWalkError(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	tool.Channels = nil // a starter with no channel executor fails the walk

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x"}`))
	if !res.IsError {
		t.Fatalf("expected the walk to fail: %s", res.Text)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.finished != 1 || rec.lastErr == nil {
		t.Errorf("finish called %d times with err=%v, want 1 and an error", rec.finished, rec.lastErr)
	}
}

// waitFor polls a condition rather than sleeping a fixed interval, so the test
// is neither flaky on a slow machine nor slow on a fast one.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
