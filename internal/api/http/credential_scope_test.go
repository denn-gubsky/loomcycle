package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// scopeProbe records, for each call, whether the run on ctx may resolve a
// $cred: in its user's own scope.
type scopeProbe struct {
	mu     sync.Mutex
	noUser []bool
}

func (p *scopeProbe) Name() string                 { return "probe" }
func (p *scopeProbe) Description() string          { return "" }
func (p *scopeProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *scopeProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.noUser = append(p.noUser, tools.RunIdentity(ctx).CredentialsNoUserScope)
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// A trigger's CredentialsNoUserScope reaches the run's own tool calls and
// its sub-agent's: a child resolves $cred: no more widely than its parent.
func TestRunOnce_CredentialsNoUserScopeReachesTheRunAndItsSubAgents(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent", "probe"}, SystemPrompt: "parent"},
			"child":  {Model: "stub-model", Tools: []string{"probe"}, SystemPrompt: "child"},
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

	for _, want := range []bool{true, false} {
		prov := &scriptedProvider{scripts: [][]providers.Event{
			call("tu1", "probe", `{}`),                             // parent
			call("tu2", "Agent", `{"name":"child","prompt":"go"}`), // parent
			call("tu3", "probe", `{}`),                             // child
			done,                                                   // child
			done,                                                   // parent
		}}
		st, err := storesqlite.Open(filepath.Join(t.TempDir(), "scope.db"))
		if err != nil {
			t.Fatal(err)
		}
		probe := &scopeProbe{}
		srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{probe}, concurrency.New(4, 4, time.Second), st)
		err = srv.RunOnce(context.Background(), runner.RunInput{
			Agent: "parent", TenantID: "acme", UserID: "bob", CredentialsNoUserScope: want,
			Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "start"}}}},
		}, runner.RunCallbacks{})
		_ = st.Close()
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if len(probe.noUser) != 2 {
			t.Fatalf("probe calls = %d, want 2 (parent and child)", len(probe.noUser))
		}
		for i, got := range probe.noUser {
			if got != want {
				t.Errorf("CredentialsNoUserScope=%t: call %d saw %t", want, i, got)
			}
		}
	}
}
