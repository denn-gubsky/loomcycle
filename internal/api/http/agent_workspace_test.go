package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// workspaceProbe records the workspace grant on ctx at each call, which is
// what the Workspace tool will read.
type workspaceProbe struct {
	mu   sync.Mutex
	seen []tools.WorkspacePolicyValue
}

func (p *workspaceProbe) Name() string                 { return "probe" }
func (p *workspaceProbe) Description() string          { return "" }
func (p *workspaceProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *workspaceProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.PolicyOf(ctx).Workspace)
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// RFC GW, the crossing: a top-level run holds its definition's workspace,
// and the sub-agent it spawns holds only what both its own definition and
// the parent run hold, under the stricter engine. The parent's grant is
// recorded on the child's run for a resume.
func TestRunOnce_WorkspaceNarrowsFromParentToSubAgent(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent", "probe"}, SystemPrompt: "parent",
				Workspace: &config.AgentWorkspace{Ops: []string{"read", "exec"}, Engine: "container"}},
			// Lists write and open, which the parent does not hold, and
			// accepts the host engine, which the parent does not.
			"child": {Model: "stub-model", Tools: []string{"probe"}, SystemPrompt: "child",
				Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Engine: "host"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	call := func(id, name, input string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: []byte(input)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
		}
	}
	done := []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		call("tu1", "probe", `{}`),                             // parent
		call("tu2", "Agent", `{"name":"child","prompt":"go"}`), // parent
		call("tu3", "probe", `{}`),                             // child
		done,                                                   // child
		done,                                                   // parent
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	probe := &workspaceProbe{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{probe}, concurrency.New(4, 4, time.Second), st)
	if err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "parent", TenantID: "acme", UserID: "bob",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "start"}}}},
	}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	want := tools.WorkspacePolicyValue{Read: true, Exec: true, Engine: "container"}
	if len(probe.seen) != 2 {
		t.Fatalf("probe calls = %d, want 2 (parent and child)", len(probe.seen))
	}
	if got := probe.seen[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("parent's workspace = %+v, want %+v", got, want)
	}
	if got := probe.seen[1]; !reflect.DeepEqual(got, want) {
		t.Errorf("child's workspace = %+v, want %+v (its own narrowed to the parent's)", got, want)
	}

	runs, _, err := st.ListRunsByUser(context.Background(), "acme", "bob", nil, 10, "")
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	var child *store.Run
	for i := range runs {
		if runs[i].Agent == "child" {
			child = &runs[i]
		}
	}
	if child == nil {
		t.Fatalf("no child run among %d runs", len(runs))
	}
	rec, ok := decodeRunConfig(child.RunConfig)
	if !ok || rec.Spawn == nil {
		t.Fatalf("child run has no spawn record: %s", child.RunConfig)
	}
	if got := tools.WorkspaceFor(rec.Spawn.Workspace); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded parent workspace = %+v, want %+v", got, want)
	}
}

// A resume rebuilds the grant the run had live: a top-level run its
// definition's (nil: agentRunPolicy reads it), a sub-run its definition's
// narrowed again to the recorded parent's. A record without the field means
// the parent had none, and a sub-run with no record fails closed.
func TestResumedWorkspace_NarrowsAgainAndFailsClosedWithoutARecord(t *testing.T) {
	def := config.AgentDef{Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Egress: []string{"pypi", "crm"}}}
	top := store.Run{ID: "r1"}
	sub := store.Run{ID: "r2", ParentRunID: "r1"}
	for _, tc := range []struct {
		name string
		run  store.Run
		rec  *spawnRecord
		want *tools.WorkspacePolicyValue
	}{
		{"top-level run reads its definition", top, nil, nil},
		{"sub-run narrowed to a read-only parent in containers", sub,
			&spawnRecord{Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Engine: "container"}},
			&tools.WorkspacePolicyValue{Read: true, Engine: "container"}},
		{"sub-run of a parent holding everything", sub,
			&spawnRecord{Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Egress: []string{"crm", "pypi"}}},
			&tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Egress: []string{"pypi", "crm"}}},
		// RFC GW P4: the routes are narrowed again too, never widened by the
		// definition to routes the parent did not hold.
		{"sub-run of a parent holding one route", sub,
			&spawnRecord{Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Open: true, Egress: []string{"pypi", "npm"}}},
			&tools.WorkspacePolicyValue{Read: true, Open: true, Egress: []string{"pypi"}}},
		{"sub-run of a parent holding no route", sub,
			&spawnRecord{Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Open: true}},
			&tools.WorkspacePolicyValue{Read: true, Open: true}},
		{"sub-run whose parent had none", sub, &spawnRecord{}, &tools.WorkspacePolicyValue{}},
		{"sub-run with no record fails closed", sub, nil, &tools.WorkspacePolicyValue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumedWorkspace(tc.run, tc.rec, def); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The spawn record carries the parent's grant through JSON: what is written
// reads back as the same grant, and "none" is not written at all.
func TestSpawnRecord_WorkspaceRoundTripsAndNoneIsNotWritten(t *testing.T) {
	for _, in := range []tools.WorkspacePolicyValue{{}, {Read: true}, {Read: true, Write: true, Exec: true, Open: true, Engine: "container"},
		{Read: true, Open: true, Egress: []string{"pypi", "npm"}}} {
		raw, err := json.Marshal(spawnRecord{Workspace: in.Grant()})
		if err != nil {
			t.Fatal(err)
		}
		if !in.Grants() && strings.Contains(string(raw), "workspace") {
			t.Errorf("a parent with none wrote %s", raw)
		}
		var back spawnRecord
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if got := tools.WorkspaceFor(back.Workspace); !reflect.DeepEqual(got, in) {
			t.Errorf("round trip of %+v gave %+v (%s)", in, got, raw)
		}
	}
}

// An agent registered over MCP (register_agent) keeps its workspace, and an
// invalid block is refused.
func TestRegisterAgent_WorkspaceRoundTrip(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "dyn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: &stubProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for name, declared := range map[string]*config.AgentWorkspace{
		"dyn-all":  {Ops: []string{"read", "write", "exec"}, Open: true, Egress: []string{"pypi", "npm"}, Engine: "container"},
		"dyn-read": {Ops: []string{"read"}},
		"dyn-none": nil,
	} {
		if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: name, SystemPrompt: "code", Tools: []string{"Read"}, Provider: "stub", Model: "stub-model",
			Workspace: declared, TTLSeconds: 600,
		}); err != nil {
			t.Fatalf("RegisterAgent %s: %v", name, err)
		}
		def, ok := lookup.Agent(ctx, st, cfg, "", name)
		if !ok {
			t.Fatalf("%s not found after RegisterAgent", name)
		}
		if !reflect.DeepEqual(def.Workspace, declared) {
			t.Errorf("%s: Workspace = %+v, want %+v", name, def.Workspace, declared)
		}
	}

	for _, bad := range []*config.AgentWorkspace{{Ops: []string{"run"}}, {Ops: []string{"read"}, Engine: "vm"},
		{Ops: []string{"read"}, Open: true, Egress: []string{"Bad_Name"}}} {
		if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: "dyn-bad", SystemPrompt: "code", Tools: []string{"Read"}, Provider: "stub", Model: "stub-model",
			Workspace: bad,
		}); err == nil || !strings.Contains(err.Error(), "workspace") {
			t.Errorf("RegisterAgent with workspace %+v: err = %v, want a workspace refusal", bad, err)
		}
	}
	// The JSON a transport decodes is strict: a key a later phase adds is
	// refused before the request is built.
	var req connector.RegisterAgentRequest
	if err := json.Unmarshal([]byte(`{"name":"x","workspace":{"ops":["read"],"connect":true}}`), &req); err == nil {
		t.Error("a register_agent body with workspace.connect decoded")
	}
}
