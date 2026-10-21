package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The def tools grant operator authority only to a call with no run id on its
// ctx (RFC ES #15): a run started with an admin bearer carries that principal
// into its model's tool calls, and the model is not the operator. The rule
// holds only while every run's tool calls carry a run id, whichever surface
// started the run, and while the operator planes carry none.

// runCtxProbeTool records, for each call, the run id on its ctx and whether an
// admin principal rode in with it.
type runCtxProbeTool struct {
	mu   sync.Mutex
	runs []string
	adm  []bool
}

func (p *runCtxProbeTool) Name() string        { return "Probe" }
func (p *runCtxProbeTool) Description() string { return "" }
func (p *runCtxProbeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (p *runCtxProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	pr, ok := auth.PrincipalFromContext(ctx)
	p.mu.Lock()
	p.runs = append(p.runs, tools.RunID(ctx))
	p.adm = append(p.adm, ok && auth.HasScope(pr.Scopes, auth.ScopeAdmin))
	p.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// RunOnce is the start every surface but POST /v1/runs shares (MCP spawn_run,
// gRPC, schedules, webhooks); a sub-agent is the other way a model reaches a
// tool. Both carry the admin principal, and both carry a run id.
func TestRunOnce_AnAdminStartedRunsToolCallsCarryARunID(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Probe", "Agent"}, SystemPrompt: "parent"},
		"child":  {Model: "stub-model", Tools: []string{"Probe"}, SystemPrompt: "child"},
	}
	call := func(id, tool, input string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: tool, Input: json.RawMessage(input)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	prov := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
		scripts: [][]providers.Event{
			call("tu_parent_probe", "Probe", `{}`),
			call("tu_spawn", "Agent", `{"name":"child","prompt":"p"}`),
			call("tu_child_probe", "Probe", `{}`),
		},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "inrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	probe := &runCtxProbeTool{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{probe}, concurrency.New(8, 8, 5*time.Second), st)

	admin := auth.WithPrincipal(t.Context(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	if err := srv.RunOnce(admin, runner.RunInput{Agent: "parent", Segments: seg}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.runs) != 2 {
		t.Fatalf("Probe was called %d times, want 2 (the parent, then the child)", len(probe.runs))
	}
	if !probe.adm[0] {
		t.Error("premise: the parent's tool call no longer carries the admin principal that started the run")
	}
	for i, who := range []string{"parent", "child"} {
		if probe.runs[i] == "" {
			t.Errorf("the %s's tool call carries no run id, so it would hold operator authority", who)
		}
	}
	if probe.runs[0] == probe.runs[1] {
		t.Errorf("parent and child tool calls carry the same run id %q", probe.runs[0])
	}
}

// The HTTP operator plane is off-run: an admin's own call there keeps
// operator authority.
func TestSubstrateAdminCtx_AnAdminsOwnCallCarriesNoRunID(t *testing.T) {
	admin := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	if id := tools.RunID(substrateAdminCtx(admin)); id != "" {
		t.Errorf("the HTTP operator plane's ctx carries run id %q", id)
	}
}
