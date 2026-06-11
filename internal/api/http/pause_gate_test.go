package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// failSetPauseStore fails every SetRunPauseState write (simulating a store
// outage during pause). Embeds store.Store so it satisfies the interface; only
// SetRunPauseState is exercised by the gate, so the nil embed is never reached.
type failSetPauseStore struct{ store.Store }

func (failSetPauseStore) SetRunPauseState(context.Context, string, string) error {
	return errors.New("store down")
}

// TestPauseGate_StoreWriteFailure_NotCreditedToBarrier (review finding #1): when
// the durable pause_state='paused' write fails, the run still parks (stops
// executing) but is NOT counted as quiesced — so paused_runs_count / snapshot
// never disagree with the store. Pause times out with a warning instead.
func TestPauseGate_StoreWriteFailure_NotCreditedToBarrier(t *testing.T) {
	realStore, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = realStore.Close() }()
	mgr := pause.NewManager(realStore, time.Second) // mgr reads via the real store
	runID := "r_failwrite"
	mgr.RegisterRun(runID)
	defer mgr.DeregisterRun(runID)

	// Gate writes to a store whose SetRunPauseState always fails.
	gate := &pauseGate{mgr: mgr, store: failSetPauseStore{}, runID: runID}

	parkDone := make(chan struct{})
	go func() {
		for mgr.State() == pause.StateRunning {
			time.Sleep(2 * time.Millisecond)
		}
		_ = gate.Park(context.Background()) // blocks (quiesces) until resume
		close(parkDone)
	}()

	res, err := mgr.Pause(context.Background(), 250*time.Millisecond)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if res.PausedRunsCount != 0 {
		t.Errorf("PausedRunsCount = %d, want 0 (a failed durable write must not credit the barrier)", res.PausedRunsCount)
	}
	if len(res.Warnings) == 0 {
		t.Error("want a warning naming the run that did not durably park")
	}

	// Resume releases the still-blocked gate.
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case <-parkDone:
	case <-time.After(2 * time.Second):
		t.Fatal("gate.Park never returned after resume")
	}
}

// panicProvider panics on Call — to exercise the detached interactive run's
// panic-recovery path (review finding #3).
type panicProvider struct{}

func (panicProvider) ID() string                  { return "panic" }
func (panicProvider) Probe(context.Context) error { return nil }
func (panicProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (panicProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (panicProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	panic("boom from provider")
}

// TestInteractiveRun_PanicRecovered_NoBarrierLeak (review finding #3): a panic
// in a DETACHED interactive run must be recovered (no process crash) and must
// still deregister the run — otherwise it leaks in the pause barrier and every
// future Pause times out waiting for the ghost. Verified by a clean Pause
// (no warning) after the panicking run settles.
func TestInteractiveRun_PanicRecovered_NoBarrierLeak(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi", UnboundedIterations: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "panic.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: panicProvider{}}, []tools.Tool{}, sem, st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	mgr := pause.NewManager(st, 200*time.Millisecond)
	srv.SetPauseManager(mgr)

	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post run: %v", err)
	}
	// Drain the SSE stream to completion (the run panics → recovered → marked
	// failed → terminal → the store-tail closes). If the panic crashed the
	// process or wedged teardown, this read (or the whole test) would hang.
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// The panicked run must have deregistered: a clean Pause (no warning) proves
	// the pause barrier has no ghost run. A leak would time out + warn.
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, perr := mgr.Pause(context.Background(), 150*time.Millisecond)
		if perr != nil {
			t.Fatalf("Pause: %v", perr)
		}
		if len(res.Warnings) == 0 {
			break // clean — no leaked run
		}
		_, _ = mgr.Resume(context.Background())
		if time.Now().After(deadline) {
			t.Fatalf("Pause kept warning (%v) — panicked run leaked in the barrier", res.Warnings)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, _ = mgr.Resume(context.Background())
}

// TestHandleRuns_503WhilePaused asserts the runtime-pause admission gate
// (RFC X / F41): a new POST /v1/runs is rejected with 503 while the runtime is
// paused, and admitted again after resume.
func TestHandleRuns_503WhilePaused(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "pausegate.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
	mgr := pause.NewManager(st, 200*time.Millisecond)
	srv.SetPauseManager(mgr)

	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	post := func() (int, string) {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"agent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
		))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Pause → new runs rejected with 503.
	if _, err := mgr.Pause(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if code, body := post(); code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/runs while paused = %d (%s), want 503", code, body)
	}

	// Resume → admitted again (200, SSE stream).
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if code, _ := post(); code != http.StatusOK {
		t.Fatalf("POST /v1/runs after resume = %d, want 200", code)
	}
}

// TestRunOnce_RuntimePausedRejected asserts the RunOnce admission gate (the
// gRPC / webhook / A2A / scheduler surface) returns runner.ErrRuntimePaused
// while paused.
func TestRunOnce_RuntimePausedRejected(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "pausegate2.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
	mgr := pause.NewManager(st, 200*time.Millisecond)
	srv.SetPauseManager(mgr)

	if _, err := mgr.Pause(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got := srv.pausedRunErr(); got == nil {
		t.Fatal("pausedRunErr() = nil while paused, want runner.ErrRuntimePaused")
	}
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := srv.pausedRunErr(); got != nil {
		t.Errorf("pausedRunErr() = %v after resume, want nil", got)
	}
}
