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

	out2, state2, err := srv.sendResidentChild(ctx, runID, "again")
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
	if _, _, err := srv.sendResidentChild(ctx, runID, "x"); err == nil {
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

	if _, _, err := srv.sendResidentChild(intruder, runID, "x"); err == nil {
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
