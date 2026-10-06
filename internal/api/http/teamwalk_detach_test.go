package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A detached walk outlives the agent that started it. Its ctx always did, but
// its members registered in the cancel registry as children of the starting
// agent, so cancelling that agent cascaded to them and the walk failed.

// soloTeam is one agent state and a terminal: the smallest walk with a member.
const soloTeam = `{"entry":"work","states":[` +
	`{"state":"work","handler":{"kind":"agent","agent":"worker"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"work","to":"done","on":"success"}]}`

// heldMemberProvider holds every call until release is closed or its ctx ends,
// and records what each call ran under.
type heldMemberProvider struct {
	up      chan struct{} // one send per call, once the call has started
	release chan struct{}

	mu       sync.Mutex
	idents   []tools.RunIdentityValue
	keyAllow []bool
}

func (p *heldMemberProvider) ID() string                  { return "stub" }
func (p *heldMemberProvider) Probe(context.Context) error { return nil }
func (p *heldMemberProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *heldMemberProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *heldMemberProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.idents = append(p.idents, tools.RunIdentity(ctx))
	p.keyAllow = append(p.keyAllow, providers.OperatorKeyAllowed(ctx))
	p.mu.Unlock()
	p.up <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "done"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

type detachHarness struct {
	t       *testing.T
	srv     *Server
	st      store.Store
	prov    *heldMemberProvider
	release func()
}

// newDetachHarness is a server whose `worker` holds until released, with the
// solo team promoted in acme. gateOn turns the operator-key restriction on.
func newDetachHarness(t *testing.T, gateOn bool) *detachHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"worker": {Model: "stub-model", SystemPrompt: "work"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.OperatorKeyRestriction = gateOn
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "detach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &heldMemberProvider{up: make(chan struct{}, 8), release: make(chan struct{})}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	seedTenantTeam(t, st, "acme", "solo", soloTeam)
	var once sync.Once
	h := &detachHarness{t: t, srv: srv, st: st, prov: prov, release: func() { once.Do(func() { close(prov.release) }) }}
	// Registered after the store's Close, so it runs first: nothing the walk
	// does may still be writing when the store goes.
	t.Cleanup(func() {
		h.release()
		cancelAllRuns(srv)
		for _, r := range h.walkRuns() {
			_, _, _ = srv.CancelTurn(context.Background(), r.ID, "test cleanup")
			h.waitEnded(r.ID)
		}
	})
	return h
}

// starter is an agent run in acme, registered for cancel the way a real run
// is, and the ctx its TeamDef call would execute under.
type starter struct {
	agentID, runID string
	ctx            context.Context
}

func (h *detachHarness) startAgent(principal *auth.Principal, restricted bool) starter {
	h.t.Helper()
	const agentID = "a_starter"
	ident := store.RunIdentity{AgentID: agentID, TenantID: "acme", UserID: "alice",
		OperatorKeyRestricted: restricted, Isolated: restricted}
	sessionID, runID, err := h.srv.openOrCreateSessionAndRun(context.Background(), "", "lead", "acme", "alice", ident)
	if err != nil {
		h.t.Fatalf("starter run: %v", err)
	}
	ctx, stop := context.WithCancelCause(context.Background())
	h.t.Cleanup(func() { stop(nil) })
	if err := h.srv.cancelReg.Register(cancel.Entry{AgentID: agentID, RunID: runID, SessionID: sessionID, UserID: "alice"}, stop); err != nil {
		h.t.Fatalf("register starter: %v", err)
	}
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		AgentID: agentID, TenantID: "acme", UserID: "alice", SessionID: sessionID, RootRunID: runID,
		OperatorKeyRestricted: restricted, Isolated: restricted,
	})
	ctx = tools.WithRunID(ctx, runID)
	if principal != nil {
		ctx = auth.WithPrincipal(ctx, *principal)
	}
	return starter{agentID: agentID, runID: runID, ctx: ctx}
}

// runDetached starts the solo team detached and returns the walk's run id.
func (h *detachHarness) runDetached(s starter) string {
	h.t.Helper()
	res, err := h.srv.TeamDef(s.ctx, json.RawMessage(`{"op":"run","name":"solo","mode":"detach"}`))
	if err != nil || res.IsError {
		h.t.Fatalf("TeamDef run detach: %v %s", err, res.Text)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.RunID == "" {
		h.t.Fatalf("no run_id in %s (%v)", res.Text, err)
	}
	return out.RunID
}

// memberUp waits for the walk's member to be inside its model call — by then
// it is registered for cancel — and returns its run row.
func (h *detachHarness) memberUp(walkID string) store.Run {
	h.t.Helper()
	select {
	case <-h.prov.up:
	case <-time.After(10 * time.Second):
		h.t.Fatal("the walk's member never started")
	}
	for _, e := range h.srv.cancelReg.ListAll() {
		if e.AgentID == "a_starter" {
			continue
		}
		run, err := h.st.GetRun(context.Background(), e.RunID)
		if err == nil && run.ParentRunID == walkID {
			return run
		}
	}
	h.t.Fatal("the member is in its model call but not registered for cancel")
	return store.Run{}
}

func (h *detachHarness) waitEnded(runID string) store.Run {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		run, err := h.st.GetRun(context.Background(), runID)
		if err == nil && run.Status != store.RunRunning {
			return run
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run %s = %+v, %v; still running", runID, run, err)
		}
	}
}

func (h *detachHarness) walkRuns() []store.Run {
	runs, err := h.st.ListActiveRunsByUser(context.Background(), "", "alice", "")
	if err != nil {
		return nil
	}
	var out []store.Run
	for _, r := range runs {
		if r.AgentID == teamWalkAgentPrefix+"solo" {
			out = append(out, r)
		}
	}
	return out
}

func TestTeamDefRunDetach_WalkCompletesAfterItsStarterIsCancelled(t *testing.T) {
	h := newDetachHarness(t, false)
	s := h.startAgent(nil, false)
	walkID := h.runDetached(s)
	member := h.memberUp(walkID)

	res, ok := h.srv.cancelReg.Cancel(s.agentID, "starter stopped")
	if !ok {
		t.Fatal("the starter was not cancellable")
	}
	for _, id := range res.Cascaded {
		if id == member.AgentID {
			t.Errorf("cancelling the starter cascaded to the detached walk's member %s", id)
		}
	}
	h.release()

	if walk := h.waitEnded(walkID); walk.Status != store.RunCompleted {
		t.Errorf("walk = %s (%s %s), want completed", walk.Status, walk.StopReason, walk.ErrorMsg)
	}
	if got := h.waitEnded(member.ID); got.Status != store.RunCompleted {
		t.Errorf("member = %s (%s %s), want completed", got.Status, got.StopReason, got.ErrorMsg)
	}
	if member.ParentAgentID != teamWalkAgentPrefix+"solo" {
		t.Errorf("member parent_agent_id = %q, want the walk's %q", member.ParentAgentID, teamWalkAgentPrefix+"solo")
	}
}

func TestTeamDefRunDetach_CancellingTheWalkByRunIDCancelsItsMember(t *testing.T) {
	h := newDetachHarness(t, false)
	s := h.startAgent(nil, false)
	walkID := h.runDetached(s)
	member := h.memberUp(walkID)

	stopped, _, err := h.srv.CancelTurn(tenantOperatorCtx("acme"), walkID, "stop the walk")
	if err != nil || !stopped {
		t.Fatalf("CancelTurn(walk) = (stopped %v, %v), want stopped", stopped, err)
	}
	if walk := h.waitEnded(walkID); walk.Status != store.RunCancelled {
		t.Errorf("walk = %s (%s), want cancelled", walk.Status, walk.ErrorMsg)
	}
	if got := h.waitEnded(member.ID); got.Status != store.RunCancelled {
		t.Errorf("member = %s (%s), want cancelled with its walk", got.Status, got.ErrorMsg)
	}
	if _, live := h.srv.cancelReg.Get(s.agentID); !live {
		t.Error("cancelling the walk reached the agent that started it")
	}
}

func TestTeamDefRunSync_MemberIsCancelledWithItsStarter(t *testing.T) {
	h := newDetachHarness(t, false)
	s := h.startAgent(nil, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.srv.TeamDef(s.ctx, json.RawMessage(`{"op":"run","name":"solo"}`))
	}()
	t.Cleanup(func() { <-done }) // runs before the store closes
	select {
	case <-h.prov.up:
	case <-time.After(10 * time.Second):
		t.Fatal("the walk's member never started")
	}
	var member cancel.Entry
	for _, e := range h.srv.cancelReg.ListAll() {
		if e.AgentID != s.agentID {
			member = e
		}
	}
	if member.ParentAgentID != s.agentID {
		t.Errorf("sync member registered under parent %q, want the starter %q", member.ParentAgentID, s.agentID)
	}

	res, ok := h.srv.cancelReg.Cancel(s.agentID, "starter stopped")
	if !ok {
		t.Fatal("the starter was not cancellable")
	}
	cascaded := false
	for _, id := range res.Cascaded {
		cascaded = cascaded || id == member.AgentID
	}
	if !cascaded {
		t.Errorf("cancelling the starter did not cascade to its synchronous walk's member (cascaded %v)", res.Cascaded)
	}
	<-done
	if got := h.waitEnded(member.RunID); got.Status != store.RunCancelled {
		t.Errorf("member = %s (%s), want cancelled with its starter", got.Status, got.ErrorMsg)
	}
}

func TestTeamDefRunDetach_MemberKeepsTheStartersAuthority(t *testing.T) {
	h := newDetachHarness(t, true)
	restricted := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	s := h.startAgent(&restricted, true)
	walkID := h.runDetached(s)
	member := h.memberUp(walkID)
	h.release()
	h.waitEnded(walkID)

	walk, err := h.st.GetRun(context.Background(), walkID)
	if err != nil {
		t.Fatal(err)
	}
	if walk.ParentRunID != s.runID {
		t.Errorf("walk parent_run_id = %q, want the starter's run %q", walk.ParentRunID, s.runID)
	}
	for _, r := range []store.Run{walk, member} {
		if r.TenantID != "acme" || r.UserID != "alice" || !r.OperatorKeyRestricted || !r.Isolated {
			t.Errorf("run %s (%s): tenant %q user %q operator_key_restricted %v isolated %v; want acme/alice, restricted, isolated",
				r.ID, r.AgentID, r.TenantID, r.UserID, r.OperatorKeyRestricted, r.Isolated)
		}
	}
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	if len(h.prov.idents) != 1 {
		t.Fatalf("model calls = %d, want 1", len(h.prov.idents))
	}
	id := h.prov.idents[0]
	if id.TenantID != "acme" || id.UserID != "alice" || !id.OperatorKeyRestricted || !id.Isolated || id.RootRunID != s.runID {
		t.Errorf("member ran as %+v; want the starter's tenant, user, restriction, isolation and root run", id)
	}
	if h.prov.keyAllow[0] {
		t.Error("the member was allowed the operator's key though its starter was not")
	}
}
