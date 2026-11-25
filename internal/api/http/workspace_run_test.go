package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// RFC GW P1, end to end inside the server: real runs, the real Workspace
// tool and a real workspace worker in process. What is asserted is the
// worker's disk and the store, not only what the tool said.

var wsAll = &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true}

type wsRunFixture struct {
	srv *Server
	st  store.Store
	h   *workspacetest.Harness
	mgr *lifecycle.Manager
	mu  sync.Mutex
	res []providers.Event
}

func newWSRunFixture(t *testing.T, agents map[string]config.AgentDef, prov providers.Provider) *wsRunFixture {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      agents,
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := workspacetest.Start(t, st, "acme", worker.Options{Capacity: 10})
	mgr := lifecycle.New(h.D, st, lifecycle.Limits{MaxPerRun: 4}, nil)
	tool := &builtin.Workspace{Manager: mgr, ExecDefault: 10 * time.Second, ExecMax: 30 * time.Second}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{tool}, concurrency.New(4, 4, time.Second), st)
	srv.SetSteerRegistry(steer.NewRegistry(8))
	srv.SetWorkspaces(mgr)
	stopDetachedOnCleanup(t, srv)
	return &wsRunFixture{srv: srv, st: st, h: h, mgr: mgr}
}

func (f *wsRunFixture) input(agent string) runner.RunInput {
	return runner.RunInput{Agent: agent, TenantID: "acme", UserID: "bob",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}}
}

func (f *wsRunFixture) callbacks() runner.RunCallbacks {
	return runner.RunCallbacks{OnEvent: func(ev providers.Event) {
		if ev.Type == providers.EventToolResult {
			f.mu.Lock()
			f.res = append(f.res, ev)
			f.mu.Unlock()
		}
	}}
}

func (f *wsRunFixture) results() []providers.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.Event(nil), f.res...)
}

// workspaceDirs lists the workspace directories on the worker's disk.
func (f *wsRunFixture) workspaceDirs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(f.h.Root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func wsCall(id, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "Workspace", Input: []byte(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
	}
}

var wsDone = []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}}

// pwdOf is the workspace directory an exec of `pwd -P` reported.
func pwdOf(t *testing.T, res providers.Event) string {
	t.Helper()
	var ea struct {
		Output string `json:"output"`
	}
	if res.IsError || json.Unmarshal([]byte(res.Text), &ea) != nil || ea.Output == "" {
		t.Fatalf("exec result = %v %s", res.IsError, res.Text)
	}
	return strings.TrimSpace(ea.Output)
}

// A run's workspace is gone from the worker's disk, and its row from the
// store, once the run ends: completed, failed, or cancelled.
func TestWorkspaceRun_ReleasedWhenItsOwnerEnds(t *testing.T) {
	agents := map[string]config.AgentDef{"builder": {Model: "stub-model", SystemPrompt: "b", Tools: []string{"Workspace"}, Workspace: wsAll}}
	opened := [][]providers.Event{wsCall("t1", `{"op":"open","name":"work"}`), wsCall("t2", `{"op":"exec","name":"work","command":"pwd -P"}`)}

	check := func(t *testing.T, f *wsRunFixture) {
		t.Helper()
		res := f.results()
		if len(res) < 2 {
			t.Fatalf("tool results = %d, want the open and the exec", len(res))
		}
		dir := pwdOf(t, res[1])
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, statErr := os.Stat(dir)
			n, _ := f.st.WorkspaceCountActiveByTenant(context.Background(), "acme")
			rows, _ := f.st.WorkspaceListByWorker(context.Background(), f.h.WorkerID)
			if os.IsNotExist(statErr) && n == 0 && len(rows) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("after the run: directory present = %v, rows = %d, dirs = %v", statErr == nil, len(rows), f.workspaceDirs(t))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	t.Run("completed", func(t *testing.T) {
		f := newWSRunFixture(t, agents, &scriptedProvider{scripts: append(opened, wsDone)})
		if err := f.srv.RunOnce(context.Background(), f.input("builder"), f.callbacks()); err != nil {
			t.Fatal(err)
		}
		check(t, f)
	})
	t.Run("failed", func(t *testing.T) {
		f := newWSRunFixture(t, agents, &scriptedProvider{scripts: append(opened,
			[]providers.Event{{Type: providers.EventError, Error: "the provider broke"}})})
		_ = f.srv.RunOnce(context.Background(), f.input("builder"), f.callbacks())
		check(t, f)
	})
	t.Run("cancelled", func(t *testing.T) {
		prov := &blockingAfterScript{scriptedProvider: scriptedProvider{scripts: opened}}
		f := newWSRunFixture(t, agents, prov)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- f.srv.RunOnce(ctx, f.input("builder"), f.callbacks()) }()
		waitUntil(t, "the exec result", func() bool { return len(f.results()) >= 2 })
		cancel()
		<-done
		check(t, f)
	})
}

// blockingAfterScript plays its script, then holds every further call until
// the caller goes away.
type blockingAfterScript struct {
	scriptedProvider
}

func (b *blockingAfterScript) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if int(b.calls.Load()) < len(b.scripts) {
		return b.scriptedProvider.Call(ctx, req)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An interactive run parked for its next message has not ended: it keeps
// its workspace while it waits, and the workspace goes when the run does
// (here, at its idle timeout).
func TestWorkspaceRun_AParkedRunKeepsItsWorkspaceUntilItEnds(t *testing.T) {
	agents := map[string]config.AgentDef{"builder": {Model: "stub-model", SystemPrompt: "b", Tools: []string{"Workspace"}, Workspace: wsAll}}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("t1", `{"op":"open","name":"work"}`), wsCall("t2", `{"op":"exec","name":"work","command":"pwd -P"}`), wsDone}})
	f.srv.cfg().Env.InteractiveIdleTimeout = 3 * time.Second
	in := f.input("builder")
	in.Interactive = true
	// RunOnce of an interactive run returns when the run ends.
	done := make(chan error, 1)
	go func() { done <- f.srv.RunOnce(context.Background(), in, f.callbacks()) }()
	waitUntil(t, "the exec result", func() bool { return len(f.results()) >= 2 })
	dir := pwdOf(t, f.results()[1])
	// Parked: the model ended its turn and the run waits for input.
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the interactive run ended before its idle timeout: %v", err)
	default:
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a parked run's workspace is gone: %v", err)
	}
	if n, _ := f.st.WorkspaceCountActiveByTenant(context.Background(), "acme"); n != 1 {
		t.Fatalf("active workspaces while parked = %d, want 1", n)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the idle end to release the workspace", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

// Narrowing reaches the tool: a sub-agent of an agent granted only read
// (and open) opens its own workspace but is refused exec, though its own
// definition grants exec. Its workspace is its own, gone when it ends, and
// never visible to its parent.
func TestWorkspaceRun_ASubAgentIsHeldToItsParentsGrant(t *testing.T) {
	agents := map[string]config.AgentDef{
		"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Open: true}},
		"child": {Model: "stub-model", SystemPrompt: "c", Tools: []string{"Workspace"}, Workspace: wsAll},
	}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		{{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "a1", Name: "Agent", Input: []byte(`{"name":"child","prompt":"go"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}},
		wsCall("c1", `{"op":"open","name":"work"}`),
		wsCall("c2", `{"op":"exec","name":"work","command":"touch made"}`),
		wsDone, // child
		wsCall("p1", `{"op":"list"}`),
		wsDone, // parent
	}})
	if err := f.srv.RunOnce(context.Background(), f.input("parent"), f.callbacks()); err != nil {
		t.Fatal(err)
	}
	// The child's tool results are on its own run, not the parent's stream.
	runs, _, err := f.st.ListRunsByUser(context.Background(), "acme", "bob", nil, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	var childRun string
	for _, r := range runs {
		if r.Agent == "child" {
			childRun = r.ID
		}
	}
	evs, err := f.st.GetRunEventsSince(context.Background(), childRun, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var childResults []string
	for _, ev := range evs {
		if ev.Type == "tool_result" {
			childResults = append(childResults, string(ev.Payload))
		}
	}
	if len(childResults) != 2 || strings.Contains(childResults[0], "error") || !strings.Contains(childResults[1], "not_granted") {
		t.Errorf("child results = %q, want the open to succeed and the exec refused not_granted", childResults)
	}
	var parentList string
	for _, r := range f.results() {
		if r.ToolUse != nil && r.ToolUse.ID == "p1" {
			parentList = r.Text
		}
	}
	if parentList != `{"workspaces":[]}` {
		t.Errorf("parent list = %s, want none: the child's workspace is not the parent's", parentList)
	}
	if dirs := f.workspaceDirs(t); len(dirs) != 0 {
		t.Errorf("workspaces left on the worker: %v", dirs)
	}
}

// The same holds after a resume: the policy a resumed sub-run is rebuilt
// with (resumedWorkspace, from its spawn record) refuses exec through the
// tool.
func TestWorkspaceRun_AResumedSubRunIsHeldToItsParentsGrant(t *testing.T) {
	f := newWSRunFixture(t, map[string]config.AgentDef{}, &scriptedProvider{})
	childDef := config.AgentDef{Workspace: wsAll}
	pol := resumedWorkspace(store.Run{ID: "r2", ParentRunID: "r1"}, &spawnRecord{Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Open: true}}, childDef)
	if pol == nil {
		t.Fatal("no policy for a sub-run")
	}
	sess, err := f.st.CreateSession(context.Background(), "acme", "child", "bob")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.st.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_child", UserID: "bob", TenantID: "acme", ParentRunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "bob", AgentID: "a_child", TenantID: "acme", RootRunID: "r1"})
	ctx = tools.WithRunPolicy(tools.WithRunID(ctx, run.ID), tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: *pol})
	var tool tools.Tool
	for _, tl := range f.srv.tools {
		if tl.Name() == "Workspace" {
			tool = tl
		}
	}
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"open","name":"work"}`)); res.IsError {
		t.Fatalf("open = %s", res.Text)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"exec","name":"work","command":"true"}`))
	if !res.IsError || !strings.Contains(res.Text, "not_granted") {
		t.Errorf("exec after the resume = %s, want not_granted", res.Text)
	}
}

// A pre-hook sees the Workspace call with its op, and a deny stops it before
// the worker runs anything.
func TestWorkspaceRun_APreHookSeesTheOpAndCanRefuseIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	agents := map[string]config.AgentDef{"builder": {Model: "stub-model", SystemPrompt: "b", Tools: []string{"Workspace"}, Workspace: wsAll}}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("t1", `{"op":"open","name":"work"}`),
		wsCall("t2", `{"op":"exec","name":"work","command":"touch `+marker+`"}`),
		wsDone}})
	var mu sync.Mutex
	var bodies []string
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(string(b), `"op":"exec"`) {
			_, _ = w.Write([]byte(`{"deny":{"text":"no commands today","is_error":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(gate.Close)
	register(t, f.srv, &hooks.Hook{Owner: "ops", Name: "gate", Phase: hooks.PhasePre, Tools: []string{"Workspace"}, CallbackURL: gate.URL})
	if err := f.srv.RunOnce(context.Background(), f.input("builder"), f.callbacks()); err != nil {
		t.Fatal(err)
	}
	res := f.results()
	if len(res) != 2 || res[0].IsError || !res[1].IsError || !strings.Contains(res[1].Text, "no commands today") {
		t.Fatalf("results = %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the denied command ran on the worker")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"op":"open"`) || !strings.Contains(bodies[1], `"name":"Workspace"`) {
		t.Errorf("the hook saw %q", bodies)
	}
}

// A code-js agent is replayed from the start on each model call; the ops its
// earlier calls made are answered from the transcript, not run again. The
// command appends a line, so a replay that ran it twice shows two.
func TestWorkspaceRun_ACodeJSReplayDoesNotRunAnOpAgain(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	js := `
function run(input) {
  Workspace({op: "open", name: "work"});
  Workspace({op: "exec", name: "work", command: "echo once >> log.txt"});
  var r = Workspace({op: "read", name: "work", path: "log.txt"});
  return { final_text: r };
}`
	if err := os.WriteFile(filepath.Join(root, "coder", "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := map[string]config.AgentDef{"coder": {Provider: "code-js", Model: "code-js", Tools: []string{"Workspace"}, Workspace: wsAll}}
	f := newWSRunFixture(t, agents, codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 10 * time.Second}))
	var final string
	cb := f.callbacks()
	inner := cb.OnEvent
	cb.OnEvent = func(ev providers.Event) {
		inner(ev)
		if ev.Type == providers.EventText {
			final += ev.Text
		}
	}
	if err := f.srv.RunOnce(context.Background(), f.input("coder"), cb); err != nil {
		t.Fatal(err)
	}
	if n := len(f.results()); n != 3 {
		t.Errorf("tool results = %d, want 3 (one per op)", n)
	}
	if !strings.Contains(final, "once") || strings.Count(final, "once") != 1 {
		t.Errorf("final text = %q, want the line written once", final)
	}
}
