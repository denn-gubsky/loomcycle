package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// tool_modes (RFC GL): a run whose definition holds Document or Path in read
// mode has every write op refused before the tool runs — whoever makes the
// call — and its read ops run. Each test drives a real run against the real
// Document and Path tools, and reads the store afterwards with its own calls,
// so "refused" means the store did not change, not only that an error came
// back.

const modesUser = "bob"

type modesFixture struct {
	srv  *Server
	st   store.Store
	doc  *builtin.Document
	path *builtin.Path
}

func newModesFixture(t *testing.T, cfg *config.Config, prov providers.Provider) modesFixture {
	t.Helper()
	cfg.Concurrency = config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "modes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	doc := &builtin.Document{Store: st, SqlMem: mgr}
	path := &builtin.Path{Store: st}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{doc, path}, concurrency.New(8, 8, 5*time.Second), st)
	srv.sqlMem = mgr
	stopDetachedOnCleanup(t, srv)
	return modesFixture{srv: srv, st: st, doc: doc, path: path}
}

// run starts agent as modesUser and returns every tool result it reported.
func (f modesFixture) run(t *testing.T, agent string) []providers.Event {
	t.Helper()
	var mu sync.Mutex
	var results []providers.Event
	err := f.srv.RunOnce(context.Background(), runner.RunInput{
		Agent: agent, UserID: modesUser,
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}, runner.RunCallbacks{OnEvent: func(ev providers.Event) {
		if ev.Type == providers.EventToolResult {
			mu.Lock()
			results = append(results, ev)
			mu.Unlock()
		}
	}})
	if err != nil {
		t.Fatalf("RunOnce(%s): %v", agent, err)
	}
	return results
}

// storeCtx is the identity the run's user scope is keyed by, for the
// fixture's own reads of the store.
func storeCtx() context.Context {
	ctx := tools.WithAgentName(context.Background(), "inspector")
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "inspector", UserID: modesUser})
}

// documentCount is how many documents the user scope holds.
func (f modesFixture) documentCount(t *testing.T) int {
	t.Helper()
	res, err := f.doc.Execute(storeCtx(), json.RawMessage(`{"op":"query_documents","scope":"user"}`))
	if err != nil || res.IsError {
		t.Fatalf("query_documents: %v %s", err, res.Text)
	}
	var out struct {
		Documents []json.RawMessage `json:"documents"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("query_documents payload: %v (%s)", err, res.Text)
	}
	return len(out.Documents)
}

// hasPath reports whether the user scope's Path tree holds p.
func (f modesFixture) hasPath(t *testing.T, p string) bool {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"op": "stat", "scope": "user", "path": p})
	res, err := f.path.Execute(storeCtx(), in)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return !res.IsError
}

func modesCall(id, name, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: []byte(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
	}
}

var modesDone = []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}}

const (
	modesImport = `{"op":"import_md","scope":"user","title":"Notes","markdown":"# Notes\n\nhello\n"}`
	modesQuery  = `{"op":"query_documents","scope":"user"}`
	modesMkdir  = `{"op":"mkdir","scope":"user","path":"/notes"}`
	modesLs     = `{"op":"ls","scope":"user","path":"/"}`
)

// modelScript is one model turn per call, then the end of the turn.
func modelScript() *scriptedProvider {
	return &scriptedProvider{scripts: [][]providers.Event{
		modesCall("t1", "Document", modesImport),
		modesCall("t2", "Document", modesQuery),
		modesCall("t3", "Path", modesMkdir),
		modesCall("t4", "Path", modesLs),
		modesDone,
	}}
}

func TestToolModes_ReadModeRefusesWritesBeforeDispatchInAModelRun(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"reader": {Model: "stub-model", SystemPrompt: "r", Tools: []string{"Document", "Path"},
				ToolModes: map[string]string{"Document": config.ToolModeRead, "Path": config.ToolModeRead}},
		},
	}
	f := newModesFixture(t, cfg, modelScript())
	results := f.run(t, "reader")
	if len(results) != 4 {
		t.Fatalf("tool results = %d, want 4", len(results))
	}
	for i, wantRefused := range []bool{true, false, true, false} {
		r := results[i]
		refused := r.IsError && strings.Contains(r.Text, "read mode")
		if refused != wantRefused {
			t.Errorf("call %d (%s): refused = %v, want %v: %s", i+1, r.ToolUse.Name, refused, wantRefused, r.Text)
		}
	}
	if strings.Contains(results[0].Text, "import_md") == false {
		t.Errorf("the refusal does not name the op: %s", results[0].Text)
	}
	if n := f.documentCount(t); n != 0 {
		t.Errorf("the store holds %d documents after a refused import_md, want 0", n)
	}
	if f.hasPath(t, "/notes") {
		t.Error("/notes exists after a refused mkdir")
	}
}

// The control the refusal test reads against: the same calls from an agent
// with no tool_modes write as they always did, so the store checks above can
// see a write when one happens.
func TestToolModes_AbsentModesLeaveWritesAsTheyWere(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"writer": {Model: "stub-model", SystemPrompt: "w", Tools: []string{"Document", "Path"}},
		},
	}
	f := newModesFixture(t, cfg, modelScript())
	for i, r := range f.run(t, "writer") {
		if r.IsError {
			t.Errorf("call %d (%s) failed without tool_modes: %s", i+1, r.ToolUse.Name, r.Text)
		}
	}
	if n := f.documentCount(t); n != 1 {
		t.Errorf("the store holds %d documents, want 1", n)
	}
	if !f.hasPath(t, "/notes") {
		t.Error("/notes is missing after mkdir")
	}
}

// A code-js agent's tool calls reach the same dispatcher: the write throws a
// catchable error naming the mode and the read returns.
func TestToolModes_ReadModeRefusesACodeJSAgentsWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	js := `
function run(input) {
  var wrote;
  try { Document(` + modesImport + `); wrote = "wrote"; } catch (e) { wrote = "refused: " + e.message; }
  var listed = Document(` + modesQuery + `);
  return { final_text: wrote + " | " + (listed ? "listed" : "") };
}`
	if err := os.WriteFile(filepath.Join(root, "coder", "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "code-js", Model: "code-js"},
		Agents: map[string]config.AgentDef{
			"coder": {Provider: "code-js", Model: "code-js", Tools: []string{"Document"},
				ToolModes: map[string]string{"Document": config.ToolModeRead}},
		},
	}
	f := newModesFixture(t, cfg, codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 10 * time.Second}))
	results := f.run(t, "coder")
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(results))
	}
	if !results[0].IsError || !strings.Contains(results[0].Text, "read mode") {
		t.Errorf("code-js import_md = %q (error %v), want refused in read mode", results[0].Text, results[0].IsError)
	}
	if results[1].IsError {
		t.Errorf("code-js query_documents failed: %s", results[1].Text)
	}
	if n := f.documentCount(t); n != 0 {
		t.Errorf("the store holds %d documents after the code-js agent's refused write, want 0", n)
	}
}

// A sub-agent runs on its own dispatcher, built from its own definition: its
// read mode holds there. Its parent, holding the tools whole, is unaffected —
// and a mode is not inherited, as tools are not.
func TestToolModes_ReadModeHoldsInASubAgentsRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		childMode map[string]string
		wantDocs  int
	}{
		{"read-mode child", map[string]string{"Document": config.ToolModeRead}, 0},
		{"whole child (control)", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
				Agents: map[string]config.AgentDef{
					"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Document"}},
					"child": {Model: "stub-model", SystemPrompt: "c", Tools: []string{"Document"},
						ToolModes: tc.childMode},
				},
			}
			prov := &scriptedProvider{scripts: [][]providers.Event{
				modesCall("p1", "Agent", `{"name":"child","prompt":"write"}`), // parent
				modesCall("c1", "Document", modesImport),                      // child
				modesDone,                                                     // child
				modesDone,                                                     // parent
			}}
			f := newModesFixture(t, cfg, prov)
			f.run(t, "parent")
			if n := f.documentCount(t); n != tc.wantDocs {
				t.Fatalf("the store holds %d documents, want %d", n, tc.wantDocs)
			}
			if tc.childMode == nil {
				return
			}
			runs, _, err := f.st.ListRunsByUser(context.Background(), "", modesUser, nil, 10, "")
			if err != nil {
				t.Fatalf("ListRunsByUser: %v", err)
			}
			for _, r := range runs {
				if r.Agent != "child" {
					continue
				}
				got := strings.Join(eventsOf(t, f.st, r, "tool_result"), "\n")
				if !strings.Contains(got, "read mode") {
					t.Errorf("the child's tool result does not name the read mode: %s", got)
				}
				return
			}
			t.Fatal("no child run")
		})
	}
}

// A read mode is sticky down the spawn tree: a read-mode parent cannot write
// through a child whose own definition holds the tool whole. The child's
// modes are its own definition's with every parent read mode added.
func TestToolModes_AReadModeParentCannotWriteThroughAWholeChild(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Document"},
				ToolModes: map[string]string{"Document": config.ToolModeRead}},
			"child": {Model: "stub-model", SystemPrompt: "c", Tools: []string{"Document"}},
		},
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		modesCall("p1", "Agent", `{"name":"child","prompt":"write"}`), // parent
		modesCall("c1", "Document", modesImport),                      // child
		modesDone,                                                     // child
		modesDone,                                                     // parent
	}}
	f := newModesFixture(t, cfg, prov)
	f.run(t, "parent")
	if n := f.documentCount(t); n != 0 {
		t.Fatalf("the store holds %d documents: a read-mode parent wrote through its child", n)
	}
	runs, _, err := f.st.ListRunsByUser(context.Background(), "", modesUser, nil, 10, "")
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	for _, r := range runs {
		if r.Agent != "child" {
			continue
		}
		rec, ok := decodeRunConfig(r.RunConfig)
		if !ok || rec.Spawn == nil || rec.Spawn.ToolModes["Document"] != config.ToolModeRead {
			t.Errorf("the child's spawn record does not carry the parent's read mode: %s", r.RunConfig)
		}
		return
	}
	t.Fatal("no child run")
}

// A resumed sub-run is narrowed again by the parent modes its record holds;
// one with no record holds every modal tool in read mode (its parent's modes
// are unknown, and its own definition's are the widening this prevents).
func TestResumedToolModes(t *testing.T) {
	def := config.AgentDef{Tools: []string{"Document", "Path"}}
	top := store.Run{ID: "r1"}
	sub := store.Run{ID: "r2", ParentRunID: "r1"}
	read := config.ToolModeRead
	for _, tc := range []struct {
		name string
		run  store.Run
		rec  *spawnRecord
		want map[string]string
	}{
		{"top-level run", top, nil, nil},
		{"sub-run with the parent's read mode", sub, &spawnRecord{ToolModes: map[string]string{"Document": read}}, map[string]string{"Document": read}},
		{"sub-run whose parent held none", sub, &spawnRecord{}, nil},
		{"sub-run with no record", sub, nil, map[string]string{"Document": read, "Path": read}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resumedToolModes(tc.run, tc.rec, def)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Round trip: an agent created through AgentDef with tool_modes is stored,
// read back through the substrate on its run, and its mode still holds.
func TestToolModes_AgentDefCreatedModeHoldsAfterTheStoredReadBack(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{},
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		modesCall("t1", "Document", modesImport),
		modesCall("t2", "Document", modesQuery),
		modesDone,
	}}
	f := newModesFixture(t, cfg, prov)
	ad := &builtin.AgentDef{Store: f.st, Cfg: cfg}
	ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{AgentDef: tools.AgentDefPolicyValue{Scopes: []string{"any"}}, Tools: []string{"*"}})
	ctx = tools.WithCallerTier(ctx, tools.TierOperatorPlane)
	res, err := ad.Execute(ctx, json.RawMessage(`{"op":"create","name":"stored-reader","overlay":{"model":"stub-model","system_prompt":"r","tools":["Document"],"tool_modes":{"Document":"read"}}}`))
	if err != nil || res.IsError {
		t.Fatalf("AgentDef create: %v %s", err, res.Text)
	}
	results := f.run(t, "stored-reader")
	if len(results) != 2 || !results[0].IsError || !strings.Contains(results[0].Text, "read mode") || results[1].IsError {
		t.Fatalf("stored agent's results = %+v, want the write refused in read mode and the read run", results)
	}
	if n := f.documentCount(t); n != 0 {
		t.Errorf("the store holds %d documents, want 0", n)
	}
}
