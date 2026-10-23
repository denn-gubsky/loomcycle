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

// gitRemoteProbe records the Git network grant on ctx at each call, which is
// what the Git tool reads.
type gitRemoteProbe struct {
	mu   sync.Mutex
	seen []tools.GitRemotePolicyValue
}

func (p *gitRemoteProbe) Name() string                 { return "probe" }
func (p *gitRemoteProbe) Description() string          { return "" }
func (p *gitRemoteProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *gitRemoteProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.PolicyOf(ctx).GitRemote)
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// RFC FB, the crossing: a top-level run holds its definition's git_remote,
// and the sub-agent it spawns may perform an operation only when both its own
// definition and the parent run allow it, so a child listing push under a
// fetch-only parent cannot push. The parent's grant is recorded on the
// child's run for a resume.
func TestRunOnce_GitRemoteNarrowsFromParentToSubAgent(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent", "probe"}, SystemPrompt: "parent",
				GitRemote: []string{"fetch"}},
			// Lists push, which the parent does not allow.
			"child": {Model: "stub-model", Tools: []string{"probe"}, SystemPrompt: "child",
				GitRemote: []string{"fetch", "push"}},
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
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "gitremote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	probe := &gitRemoteProbe{}
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
	if got, want := probe.seen[0], (tools.GitRemotePolicyValue{Fetch: true}); got != want {
		t.Errorf("parent's git_remote = %+v, want %+v", got, want)
	}
	if got, want := probe.seen[1], (tools.GitRemotePolicyValue{Fetch: true}); got != want {
		t.Errorf("child's git_remote = %+v, want %+v (its own [fetch push] narrowed to the parent's)", got, want)
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
	if !reflect.DeepEqual(rec.Spawn.GitRemote, []string{"fetch"}) {
		t.Errorf("recorded parent git_remote = %v, want [fetch]", rec.Spawn.GitRemote)
	}
}

// A resume rebuilds the grant the run had live: a top-level run its
// definition's (nil: agentRunPolicy reads it), a sub-run its definition's
// narrowed again to the recorded parent's. A record without the field means
// the parent had no network, and a sub-run with no record fails closed.
func TestResumedGitRemote(t *testing.T) {
	def := config.AgentDef{GitRemote: []string{"fetch", "push"}}
	top := store.Run{ID: "r1"}
	sub := store.Run{ID: "r2", ParentRunID: "r1"}
	v := func(fetch, push bool) *tools.GitRemotePolicyValue {
		return &tools.GitRemotePolicyValue{Fetch: fetch, Push: push}
	}
	for _, tc := range []struct {
		name string
		run  store.Run
		rec  *spawnRecord
		want *tools.GitRemotePolicyValue
	}{
		{"top-level run reads its definition", top, nil, nil},
		{"sub-run narrowed to a fetch-only parent", sub, &spawnRecord{GitRemote: []string{"fetch"}}, v(true, false)},
		{"sub-run of a parent that could push", sub, &spawnRecord{GitRemote: []string{"fetch", "push"}}, v(true, true)},
		{"sub-run whose parent had no network", sub, &spawnRecord{}, v(false, false)},
		{"sub-run with no record fails closed", sub, nil, v(false, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumedGitRemote(tc.run, tc.rec, def); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The spawn record carries the parent's grant through JSON: what is written
// reads back as the same grant, and "none" is not written at all.
func TestSpawnRecord_GitRemoteRoundTrip(t *testing.T) {
	for _, in := range []tools.GitRemotePolicyValue{{}, {Fetch: true}, {Fetch: true, Push: true}} {
		raw, err := json.Marshal(spawnRecord{GitRemote: in.Ops()})
		if err != nil {
			t.Fatal(err)
		}
		if in == (tools.GitRemotePolicyValue{}) && strings.Contains(string(raw), "git_remote") {
			t.Errorf("a parent with no network wrote %s", raw)
		}
		var back spawnRecord
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if got := tools.GitRemoteFor(back.GitRemote); got != in {
			t.Errorf("round trip of %+v gave %+v (%s)", in, got, raw)
		}
	}
}

// An agent registered over MCP (register_agent) keeps its git_remote, and an
// invalid list is refused.
func TestRegisterAgent_GitRemoteRoundTrip(t *testing.T) {
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

	for name, declared := range map[string][]string{"dyn-push": {"fetch", "push"}, "dyn-fetch": {"fetch"}, "dyn-none": nil} {
		if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: name, SystemPrompt: "code", Tools: []string{"Git"}, Provider: "stub", Model: "stub-model",
			GitRemote: declared, TTLSeconds: 600,
		}); err != nil {
			t.Fatalf("RegisterAgent %s: %v", name, err)
		}
		def, ok := lookup.Agent(ctx, st, cfg, "", name)
		if !ok {
			t.Fatalf("%s not found after RegisterAgent", name)
		}
		if got, want := strings.Join(def.GitRemote, ","), strings.Join(declared, ","); got != want {
			t.Errorf("%s: GitRemote = %q, want %q", name, got, want)
		}
	}

	for _, bad := range [][]string{{"push"}, {"fetch", "pull"}} {
		if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: "dyn-bad", SystemPrompt: "code", Tools: []string{"Git"}, Provider: "stub", Model: "stub-model",
			GitRemote: bad,
		}); err == nil || !strings.Contains(err.Error(), "git_remote") {
			t.Errorf("RegisterAgent with git_remote %v: err = %v, want a git_remote refusal", bad, err)
		}
	}
}
