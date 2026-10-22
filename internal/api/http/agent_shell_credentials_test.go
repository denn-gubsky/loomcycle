package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
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

// shellCredsProbe records the shell-credential narrowing on ctx at each call,
// which is what Bash and the Bashbox fallback read.
type shellCredsProbe struct {
	mu   sync.Mutex
	seen []tools.ShellCredsPolicyValue
}

func (p *shellCredsProbe) Name() string                 { return "probe" }
func (p *shellCredsProbe) Description() string          { return "" }
func (p *shellCredsProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *shellCredsProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.PolicyOf(ctx).ShellCreds)
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

func strs(list ...string) *config.CredentialNames { c := config.CredentialNames(list); return &c }

// RFC EW, the crossing: a top-level run holds its definition's
// shell_credentials, and the sub-agent it spawns holds only the names both its
// own definition and the parent run allow, so a child listing a credential the
// parent gave up does not get it. The parent's narrowing is recorded on the
// child's run for a resume.
func TestRunOnce_ShellCredentialsNarrowFromParentToSubAgent(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent", "probe"}, SystemPrompt: "parent",
				ShellCredentials: strs("GITHUB_TOKEN", "NPM_TOKEN")},
			// Lists DEPLOY_KEY, which the parent does not allow.
			"child": {Model: "stub-model", Tools: []string{"probe"}, SystemPrompt: "child",
				ShellCredentials: strs("DEPLOY_KEY", "NPM_TOKEN")},
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
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "shellcreds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	probe := &shellCredsProbe{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{probe}, concurrency.New(4, 4, time.Second), st)
	if err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "parent", TenantID: "acme", UserID: "bob",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "start"}}}},
	}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(probe.seen) != 2 {
		t.Fatalf("probe calls = %d, want 2 (parent and child)", len(probe.seen))
	}
	if got, want := probe.seen[0], (tools.ShellCredsPolicyValue{Set: true, Names: []string{"GITHUB_TOKEN", "NPM_TOKEN"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("parent's shell credentials = %+v, want %+v", got, want)
	}
	if got, want := probe.seen[1], (tools.ShellCredsPolicyValue{Set: true, Names: []string{"NPM_TOKEN"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("child's shell credentials = %+v, want %+v (its own [DEPLOY_KEY NPM_TOKEN] narrowed to the parent's)", got, want)
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
	if rec.Spawn.ShellCredentials == nil || !reflect.DeepEqual([]string(*rec.Spawn.ShellCredentials), []string{"GITHUB_TOKEN", "NPM_TOKEN"}) {
		t.Errorf("recorded parent shell credentials = %v, want [GITHUB_TOKEN NPM_TOKEN]", rec.Spawn.ShellCredentials)
	}
}

// A resume rebuilds the narrowing the run had live: a top-level run its
// definition's (nil: agentRunPolicy reads it), a sub-run its definition's
// narrowed again to the recorded parent's. A record without the field, or no
// record, narrows to the definition alone: no run could narrow before the
// field existed.
func TestResumedShellCreds(t *testing.T) {
	def := config.AgentDef{ShellCredentials: strs("A", "B")}
	top := store.Run{ID: "r1"}
	sub := store.Run{ID: "r2", ParentRunID: "r1"}
	set := func(n ...string) *tools.ShellCredsPolicyValue {
		return &tools.ShellCredsPolicyValue{Set: true, Names: append([]string{}, n...)}
	}
	for _, tc := range []struct {
		name string
		run  store.Run
		rec  *spawnRecord
		want *tools.ShellCredsPolicyValue
	}{
		{"top-level run reads its definition", top, nil, nil},
		{"sub-run narrowed to its recorded parent", sub, &spawnRecord{ShellCredentials: strs("B", "C")}, set("B")},
		{"sub-run whose parent allowed none", sub, &spawnRecord{ShellCredentials: strs()}, set()},
		{"sub-run recorded before the field", sub, &spawnRecord{}, set("A", "B")},
		{"sub-run with no record", sub, nil, set("A", "B")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumedShellCreds(tc.run, tc.rec, def); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The spawn record keeps "allowed none" apart from "narrowed nothing": an
// empty list survives the JSON round trip as an empty list.
func TestSpawnRecord_ShellCredentialsKeepsEmptyApartFromAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   tools.ShellCredsPolicyValue
	}{
		{"narrowed nothing", tools.ShellCredsPolicyValue{}},
		{"allowed none", tools.ShellCredsPolicyValue{Set: true}},
		{"allowed one", tools.ShellCredsPolicyValue{Set: true, Names: []string{"A"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(spawnRecord{ShellCredentials: shellCredsRecordOf(tc.in)})
			if err != nil {
				t.Fatal(err)
			}
			var back spawnRecord
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			got := tools.ShellCredsFor(back.ShellCredentials)
			if got.Set != tc.in.Set || len(got.Names) != len(tc.in.Names) {
				t.Errorf("round trip of %+v gave %+v (%s)", tc.in, got, raw)
			}
		})
	}
}

// An agent registered over MCP (register_agent) keeps its shell_credentials,
// `[]` included: dropped on the way to storage, it would come back holding
// every credential the operator lists. A malformed name is refused.
func TestRegisterAgent_ShellCredentialsRoundTrip(t *testing.T) {
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

	for name, declared := range map[string]*config.CredentialNames{"dyn-none": strs(), "dyn-one": strs("GITHUB_TOKEN"), "dyn-absent": nil} {
		if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: name, SystemPrompt: "code", Tools: []string{"Bash"}, Provider: "stub", Model: "stub-model",
			ShellCredentials: declared, TTLSeconds: 600,
		}); err != nil {
			t.Fatalf("RegisterAgent %s: %v", name, err)
		}
		def, ok := lookup.Agent(ctx, st, cfg, "", name)
		if !ok {
			t.Fatalf("%s not found after RegisterAgent", name)
		}
		// strs() is a pointer to a NIL list, the shape JSON once wrote as
		// null (absent): it must come back as none, an empty list.
		got, want := def.ShellCredentials, declared
		if (got == nil) != (want == nil) || (got != nil && !reflect.DeepEqual([]string(*got), append([]string{}, (*want)...))) {
			t.Errorf("%s: ShellCredentials = %v, want %v", name, got, want)
		}
	}

	if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
		Name: "dyn-bad", SystemPrompt: "code", Tools: []string{"Bash"}, Provider: "stub", Model: "stub-model",
		ShellCredentials: strs("git:GH_TOKEN"),
	}); err == nil {
		t.Error("RegisterAgent accepted a malformed shell credential name")
	}
}
