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
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// cryptoProbe records the Crypto grant on ctx at each call — what the Crypto
// tool will read.
type cryptoProbe struct {
	mu   sync.Mutex
	seen [][]string
}

func (p *cryptoProbe) Name() string                 { return "probe" }
func (p *cryptoProbe) Description() string          { return "" }
func (p *cryptoProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *cryptoProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.PolicyOf(ctx).Crypto.Scopes)
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// The crossing: a top-level run holds its own definition's crypto_scopes, and
// the sub-agent it spawns holds only what BOTH its own definition and the
// parent run hold — a child declaring a wider key scope does not get it. The
// parent's grant is also recorded on the child's run so a resume can narrow
// again without the parent's ctx.
func TestRunOnce_CryptoScopesNarrowFromParentToSubAgent(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent", "probe"}, SystemPrompt: "parent",
				CryptoScopes: []string{"agent", "user"}},
			// Declares tenant, which the parent does not hold.
			"child": {Model: "stub-model", Tools: []string{"probe"}, SystemPrompt: "child",
				CryptoScopes: []string{"tenant", "user"}},
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
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "crypto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	probe := &cryptoProbe{}
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
	if got, want := probe.seen[0], []string{"agent", "user"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parent's crypto scopes = %v, want %v", got, want)
	}
	if got, want := probe.seen[1], []string{"user"}; !reflect.DeepEqual(got, want) {
		t.Errorf("child's crypto scopes = %v, want %v (its own [tenant user] narrowed to the parent's [agent user])", got, want)
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
	if got, want := rec.Spawn.CryptoScopes, []string{"agent", "user"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded parent crypto scopes = %v, want %v", got, want)
	}
}

// A resume rebuilds the same grant the run had live: a top-level run its own
// definition's, a sub-run its definition's narrowed to the recorded parent
// grant, and a sub-run with no record nothing at all (fail closed — its own
// definition's is exactly the widening the narrowing prevents).
func TestResumedCryptoPolicy(t *testing.T) {
	def := config.AgentDef{CryptoScopes: []string{"tenant", "user"}}
	top := store.Run{ID: "r1"}
	sub := store.Run{ID: "r2", ParentRunID: "r1"}
	for _, tc := range []struct {
		name string
		run  store.Run
		rec  *spawnRecord
		want []string
	}{
		{"top-level run", top, nil, []string{"tenant", "user"}},
		{"sub-run narrowed to its recorded parent", sub, &spawnRecord{CryptoScopes: []string{"user", "run"}}, []string{"user"}},
		{"sub-run whose parent held none", sub, &spawnRecord{}, nil},
		{"sub-run with no record", sub, nil, nil},
		{"sub-run marked only by parent agent", store.Run{ID: "r3", ParentAgentID: "a1"}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumedCryptoPolicy(tc.run, tc.rec, def).Scopes; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
