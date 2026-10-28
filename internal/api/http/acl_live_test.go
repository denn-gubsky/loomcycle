package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// schemaRecordingProvider answers from scripts in call order, as
// scriptedProvider does, and records the tool names each call was offered.
type schemaRecordingProvider struct {
	mu      sync.Mutex
	scripts [][]providers.Event
	offered [][]string
}

func (p *schemaRecordingProvider) ID() string                  { return "scripted" }
func (p *schemaRecordingProvider) Probe(context.Context) error { return nil }
func (p *schemaRecordingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *schemaRecordingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *schemaRecordingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	var names []string
	for _, ts := range req.Tools {
		names = append(names, ts.Name)
	}
	idx := len(p.offered)
	p.offered = append(p.offered, names)
	p.mu.Unlock()
	events := []providers.Event{
		{Type: providers.EventText, Text: "done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	if idx < len(p.scripts) {
		events = p.scripts[idx]
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *schemaRecordingProvider) offeredAt(i int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.offered) {
		return nil
	}
	return p.offered[i]
}

// bindingTool is a tool whose call changes the caller's rules while the run
// is in flight: it runs change once, then answers like any tool.
type bindingTool struct {
	name   string
	change func()
	once   *sync.Once
}

func (b bindingTool) Name() string                 { return b.name }
func (b bindingTool) Description() string          { return "stub " + b.name }
func (b bindingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b bindingTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	b.once.Do(b.change)
	return tools.Result{Text: b.name + " ran"}, nil
}

// liveRanTool counts its runs.
type liveRanTool struct {
	name string
	ran  *atomic.Int32
}

func (c liveRanTool) Name() string                 { return c.name }
func (c liveRanTool) Description() string          { return "stub " + c.name }
func (c liveRanTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c liveRanTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	c.ran.Add(1)
	return tools.Result{Text: c.name + " ran"}, nil
}

func toolTurn(id, name string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

type aclLiveFixture struct {
	ts       *httptest.Server
	st       store.Store
	prov     *schemaRecordingProvider
	bashRuns *atomic.Int32
}

// newACLLiveFixture is aclRunFixture's server — a `worker` holding Change,
// Bash and Agent, a `child` holding Change and Bash, the member token of
// acme/dev, the resolver in compat mode with a ttl of zero — with
// LOOMCYCLE_ACL_LIVE set to live and a Change tool that runs change the
// first time it is called.
func newACLLiveFixture(t *testing.T, live bool, change func(st store.Store), scripts ...[]providers.Event) aclLiveFixture {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Env.ACLLive = live
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{"Change", "Bash", "Agent"}, SystemPrompt: "worker"},
		"child":  {Model: "stub-model", Tools: []string{"Change", "Bash"}, SystemPrompt: "child"},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "acllive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	}); err != nil {
		t.Fatal(err)
	}
	f := aclLiveFixture{st: st, prov: &schemaRecordingProvider{scripts: scripts}, bashRuns: &atomic.Int32{}}
	reg := []tools.Tool{
		bindingTool{name: "Change", change: func() { change(st) }, once: &sync.Once{}},
		liveRanTool{name: "Bash", ran: f.bashRuns},
	}
	srv := New(cfg, &stubResolver{p: f.prov}, reg, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	f.ts = httptest.NewServer(srv.Mux())
	t.Cleanup(f.ts.Close)
	return f
}

func bindNoShell(t *testing.T) func(store.Store) {
	return func(st store.Store) {
		ctx := context.Background()
		if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)}); err != nil {
			t.Error(err)
		}
		if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "user", SubjectID: "dev"}); err != nil {
			t.Error(err)
		}
	}
}

func startWorker(t *testing.T, ts *httptest.Server, agentID string) {
	t.Helper()
	body := `{"agent":"worker","agent_id":"` + agentID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, ts, workerBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
}

func offers(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// The core behaviour end to end: with LOOMCYCLE_ACL_LIVE on, a deny on Bash
// bound to the caller while the run is in flight (by the run's own first
// tool call) withholds Bash from the NEXT model call's schema, refuses the
// model's call to it with acl_denied before it runs, and reports the change
// on the run's transcript. With the flag off the same run is exactly the
// frozen set's: Bash stays offered and runs.
func TestACLLive_ADenyBoundMidRunWithdrawsTheToolOnTheNextIteration(t *testing.T) {
	for _, live := range []bool{true, false} {
		name := map[bool]string{true: "live", false: "frozen"}[live]
		t.Run(name, func(t *testing.T) {
			f := newACLLiveFixture(t, live, bindNoShell(t), toolTurn("tu_1", "Change"), toolTurn("tu_2", "Bash"))
			startWorker(t, f.ts, "a_live_"+name)
			if !offers(f.prov.offeredAt(0), "Bash") {
				t.Fatalf("the first call should offer Bash: %v", f.prov.offeredAt(0))
			}
			evs := runEvents(t, f.st, "a_live_"+name)
			if live {
				if offers(f.prov.offeredAt(1), "Bash") {
					t.Errorf("the call after the deny still offered Bash: %v", f.prov.offeredAt(1))
				}
				if f.bashRuns.Load() != 0 || !hasEvent(evs, "tool_result", "acl_denied") {
					t.Errorf("Bash should be refused acl_denied, ran %d times; events: %s", f.bashRuns.Load(), aclEventTypes(evs))
				}
				if !hasEvent(evs, "capability_inert", `"gate":"acl"`) || !hasEvent(evs, "capability_inert", "changed during the run") {
					t.Errorf("no mid-run acl inert event; events: %s", aclEventTypes(evs))
				}
				return
			}
			if !offers(f.prov.offeredAt(1), "Bash") || f.bashRuns.Load() != 1 || hasEvent(evs, "capability_inert", `"gate":"acl"`) {
				t.Errorf("flag off: the set is frozen at the start — Bash offered %v, ran %d; events: %s", f.prov.offeredAt(1), f.bashRuns.Load(), aclEventTypes(evs))
			}
		})
	}
}

// A deny lifted mid-run does not give the run what it started without: Bash,
// withheld at the start, stays out of every later schema and does not run.
func TestACLLive_ADenyLiftedMidRunDoesNotWidenTheRun(t *testing.T) {
	retire := func(st store.Store) {
		rows, err := st.AclBindingList(context.Background(), "acme", false)
		if err != nil || len(rows) == 0 {
			t.Errorf("bindings: %v %v", rows, err)
			return
		}
		if ok, err := st.AclBindingRetire(context.Background(), "acme", rows[0].BindingID); !ok || err != nil {
			t.Error(ok, err)
		}
	}
	f := newACLLiveFixture(t, true, retire, toolTurn("tu_1", "Change"), toolTurn("tu_2", "Bash"))
	bindNoShell(t)(f.st)
	startWorker(t, f.ts, "a_lift")
	if offers(f.prov.offeredAt(0), "Bash") || offers(f.prov.offeredAt(1), "Bash") {
		t.Errorf("Bash offered after the deny was lifted: %v then %v", f.prov.offeredAt(0), f.prov.offeredAt(1))
	}
	if f.bashRuns.Load() != 0 {
		t.Errorf("Bash ran %d times; the run started without it", f.bashRuns.Load())
	}
}

// A sub-agent follows its caller's live set: a deny bound while the child
// is running reaches the child's next call (refused, withheld, reported on
// the CHILD's transcript), and its parent's next call after it returns.
func TestACLLive_ASubAgentFollowsTheCallersLiveSet(t *testing.T) {
	spawn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_p1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"work"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	childDone := []providers.Event{
		{Type: providers.EventText, Text: "child done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	// Calls in order: parent spawns; child changes the rules; child calls
	// Bash; child ends; parent ends.
	f := newACLLiveFixture(t, true, bindNoShell(t), spawn, toolTurn("tu_c1", "Change"), toolTurn("tu_c2", "Bash"), childDone)
	startWorker(t, f.ts, "a_tree")
	if !offers(f.prov.offeredAt(1), "Bash") {
		t.Fatalf("the child's first call should offer Bash: %v", f.prov.offeredAt(1))
	}
	if offers(f.prov.offeredAt(2), "Bash") || offers(f.prov.offeredAt(4), "Bash") {
		t.Errorf("after the deny Bash was still offered: child %v, parent %v", f.prov.offeredAt(2), f.prov.offeredAt(4))
	}
	if f.bashRuns.Load() != 0 {
		t.Errorf("the child ran Bash %d times after the deny", f.bashRuns.Load())
	}
	parent, err := f.st.GetRunByAgentID(context.Background(), "a_tree")
	if err != nil {
		t.Fatal(err)
	}
	kids, err := f.st.ListRunsByParentRunID(context.Background(), parent.ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("children: %v %v", kids, err)
	}
	evs, err := f.st.GetRunEventsSince(context.Background(), kids[0].ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(evs, "tool_result", "acl_denied") || !hasEvent(evs, "capability_inert", "changed during the run") {
		t.Errorf("the child's transcript should carry the refusal and the change; events: %s", aclEventTypes(evs))
	}
	if strings.Contains(aclEventTypes(evs), "error") {
		t.Errorf("the child errored: %s", aclEventTypes(evs))
	}
}
