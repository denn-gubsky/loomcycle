package http

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// newResidentTestServer builds a server whose stub provider parks an interactive
// run after every turn (text "ok" → end_turn), with the steer registry wired so
// a resident child can park + be steered. The provider replays its events on
// each Call, so open + N sends all park cleanly.
func newResidentTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"child": {Model: "stub-model", SystemPrompt: "be brief"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	sem := concurrency.New(8, 8, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "resident.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	return srv
}

func residentParentCtx(agentID, tenant string) context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID: agentID, TenantID: tenant,
	})
}

// waitResidentGone polls until the child is removed from the resident registry
// (its loop goroutine ran its teardown after a cancel).
func waitResidentGone(t *testing.T, srv *Server, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.residentReg.get(runID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("resident child %s was not reaped within the deadline", runID)
}

// TestResidentChild_OpenSendClose is the core RFC BK loop: open parks the child
// (state awaiting_input, first output captured), send resumes it for another
// turn, close reaps it — and a closed child is gone (send errors, re-close is a
// no-op). Everything below is real (prepareSubRun → loop → steer → park); only
// the provider is stubbed.
func TestResidentChild_OpenSendClose(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := residentParentCtx("parent-agent", "")

	runID, out, state, err := srv.openResidentChild(ctx, "child", "start", "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if runID == "" || state != "awaiting_input" || !strings.Contains(out, "ok") {
		t.Fatalf("open result: runID=%q state=%q out=%q", runID, state, out)
	}
	if n := srv.residentReg.countByParent("parent-agent"); n != 1 {
		t.Fatalf("expected 1 resident child, got %d", n)
	}

	out2, state2, err := srv.sendResidentChild(ctx, runID, "again", 0)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if state2 != "awaiting_input" || !strings.Contains(out2, "ok") {
		t.Fatalf("send result: state=%q out=%q", state2, out2)
	}

	if err := srv.closeResidentChild(ctx, runID); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitResidentGone(t, srv, runID)

	// Idempotent: closing an already-gone child is not an error.
	if err := srv.closeResidentChild(ctx, runID); err != nil {
		t.Errorf("second close should be a no-op, got %v", err)
	}
	// Sending to a gone child errors (not found).
	if _, _, err := srv.sendResidentChild(ctx, runID, "x", 0); err == nil {
		t.Error("send to a closed child should error")
	}
}

// TestResidentChild_PerRunCapErrors asserts op=open errors (not queues) once the
// opener holds the configured maximum resident children.
func TestResidentChild_PerRunCapErrors(t *testing.T) {
	srv := newResidentTestServer(t)
	srv.cfg.Env.MaxInteractiveChildren = 1
	ctx := residentParentCtx("parent-agent", "")

	runID, _, _, err := srv.openResidentChild(ctx, "child", "one", "", 0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = srv.closeResidentChild(ctx, runID) }()

	_, _, _, err = srv.openResidentChild(ctx, "child", "two", "", 0)
	if err == nil {
		t.Fatal("second open should hit the per-run cap")
	}
	if !strings.Contains(err.Error(), "cap reached") {
		t.Errorf("unexpected cap error: %v", err)
	}
}

// TestResidentChild_TenantIsolation asserts a child is addressable only within
// its opener's tenant — a different tenant gets an opaque not-found on send and
// an opaque no-op on close (which must NOT reap the owner's child).
func TestResidentChild_TenantIsolation(t *testing.T) {
	srv := newResidentTestServer(t)
	owner := residentParentCtx("parent-a", "tenant-a")
	intruder := residentParentCtx("parent-b", "tenant-b")

	runID, _, _, err := srv.openResidentChild(owner, "child", "start", "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = srv.closeResidentChild(owner, runID) }()

	if _, _, err := srv.sendResidentChild(intruder, runID, "x", 0); err == nil {
		t.Error("cross-tenant send should be refused")
	}
	if err := srv.closeResidentChild(intruder, runID); err != nil {
		t.Errorf("cross-tenant close should be an opaque no-op, got %v", err)
	}
	if _, ok := srv.residentReg.get(runID); !ok {
		t.Error("cross-tenant close must NOT reap the owner's child")
	}
}

// TestResidentChild_ParentTeardownReaps asserts the finishRunWithCancel backstop
// closes resident children a parent opened but never closed itself.
func TestResidentChild_ParentTeardownReaps(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := residentParentCtx("parent-agent", "")

	runID, _, _, err := srv.openResidentChild(ctx, "child", "start", "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv.closeResidentChildrenOf("parent-agent") // simulate the parent run ending
	waitResidentGone(t, srv, runID)
}

// TestResidentChild_IdleSweepReaps asserts an idle child (no send past its TTL)
// is reaped by the sweeper.
func TestResidentChild_IdleSweepReaps(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := residentParentCtx("parent-agent", "")

	runID, _, _, err := srv.openResidentChild(ctx, "child", "start", "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// One sweep far in the future → past any idle TTL → the child is reaped.
	srv.sweepResidentChildren(time.Now().Add(24 * time.Hour))
	waitResidentGone(t, srv, runID)
}

// gatedProvider emits a line of text, then blocks before end_turn until the test
// releases its gate (or the turn's ctx is cancelled — RFC BH turn-cancel). Lets
// a test hold a turn "running" to exercise send timeout_ms / poll / cancel.
type gatedProvider struct {
	text string
	gate chan struct{} // one token per turn that should complete
}

func (g *gatedProvider) ID() string                    { return "stub" }
func (g *gatedProvider) Probe(_ context.Context) error { return nil }
func (g *gatedProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (g *gatedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (g *gatedProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 4)
	go func() {
		defer close(ch)
		ch <- providers.Event{Type: providers.EventText, Text: g.text}
		select {
		case <-g.gate:
			ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		case <-ctx.Done():
			// turn cancelled → emit nothing more; the loop re-parks the run.
		}
	}()
	return ch, nil
}

// newGatedResidentServer builds a resident-capable server driven by a gated
// provider. gate is buffered so the caller pre-fills a token for the open turn.
func newGatedResidentServer(t *testing.T) (*Server, chan struct{}) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"child": {Model: "stub-model", SystemPrompt: "be brief"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	gate := make(chan struct{}, 4)
	sem := concurrency.New(8, 8, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "gated.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &gatedProvider{text: "working", gate: gate}}, []tools.Tool{}, sem, st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	return srv, gate
}

// TestResidentChild_SendTimeoutThenPoll: a slow turn makes send return
// state "running" + partial output; poll then awaits its completion.
func TestResidentChild_SendTimeoutThenPoll(t *testing.T) {
	srv, gate := newGatedResidentServer(t)
	ctx := residentParentCtx("parent-agent", "")

	gate <- struct{}{} // let the open turn complete + park
	runID, _, state, err := srv.openResidentChild(ctx, "child", "start", "", 0)
	if err != nil || state != "awaiting_input" {
		t.Fatalf("open: state=%q err=%v", state, err)
	}
	defer func() { _ = srv.closeResidentChild(ctx, runID) }()

	// send with a short timeout while the turn is gated → returns "running".
	out, state, err := srv.sendResidentChild(ctx, runID, "do slow work", 100)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if state != "running" {
		t.Fatalf("expected state=running on a gated turn, got %q (out=%q)", state, out)
	}
	if !strings.Contains(out, "working") {
		t.Errorf("expected partial output while running, got %q", out)
	}

	// a second send is refused while the turn is still in flight.
	if _, _, err := srv.sendResidentChild(ctx, runID, "again", 100); err == nil {
		t.Error("send during a running turn should be refused")
	}

	gate <- struct{}{} // release the turn
	out, state, err = srv.pollResidentChild(ctx, runID, 2000)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if state != "awaiting_input" {
		t.Fatalf("poll after release: state=%q out=%q", state, out)
	}
}

// TestResidentChild_CancelStopsTurn: cancel turn-cancels a running turn and the
// child re-parks (stays alive).
func TestResidentChild_CancelStopsTurn(t *testing.T) {
	srv, gate := newGatedResidentServer(t)
	ctx := residentParentCtx("parent-agent", "")

	gate <- struct{}{} // open turn completes + parks
	runID, _, state, err := srv.openResidentChild(ctx, "child", "start", "", 0)
	if err != nil || state != "awaiting_input" {
		t.Fatalf("open: state=%q err=%v", state, err)
	}
	defer func() { _ = srv.closeResidentChild(ctx, runID) }()

	// start a gated (never-released) turn → send times out "running".
	_, state, err = srv.sendResidentChild(ctx, runID, "loop forever", 100)
	if err != nil || state != "running" {
		t.Fatalf("send: state=%q err=%v", state, err)
	}

	// cancel stops the turn and re-parks the child (alive).
	_, state, err = srv.cancelResidentChildTurn(ctx, runID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if state != "awaiting_input" {
		t.Fatalf("cancel should re-park the child, got state=%q", state)
	}
	// the child is still resident (cancel != close).
	if _, ok := srv.residentReg.get(runID); !ok {
		t.Error("cancel must keep the child resident")
	}
}
