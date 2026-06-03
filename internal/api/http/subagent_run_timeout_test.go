package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runMetaCapturingProvider records the ctx-carried RunMeta.RunTimeoutSeconds on
// every Call (in call order) and drives a scripted event sequence. Mirrors
// bearerCapturingProvider — the seam for asserting what a child run inherits in
// its ctx at provider-call time.
type runMetaCapturingProvider struct {
	calls   int
	scripts [][]providers.Event

	mu             sync.Mutex
	capturedBudget []int // RunMeta.RunTimeoutSeconds per Call invocation
}

func (p *runMetaCapturingProvider) ID() string                    { return "runmeta-capturing" }
func (p *runMetaCapturingProvider) Probe(_ context.Context) error { return nil }
func (p *runMetaCapturingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *runMetaCapturingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *runMetaCapturingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	meta, _ := providers.RunMetaFromContext(ctx)
	p.mu.Lock()
	p.capturedBudget = append(p.capturedBudget, meta.RunTimeoutSeconds)
	idx := p.calls
	p.calls++
	p.mu.Unlock()

	var events []providers.Event
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

// TestSubAgent_InheritsPerAgentRunTimeout is the regression for the code-review
// finding on PR #359: runSubAgent (the 4th loop.Run site) omitted
// RunTimeoutSeconds, so a code-js sub-agent's per-agent run_timeout_seconds was
// silently dropped to the global default — exactly the fan-out orchestrator
// case the override exists to serve. The child here declares
// run_timeout_seconds: 1800; its provider Call must see that budget on RunMeta.
//
// Fail-before: with the RunTimeoutSeconds line removed from runSubAgent's
// RunOptions, the child Call captures 0 (global default) and this test fails.
func TestSubAgent_InheritsPerAgentRunTimeout(t *testing.T) {
	const childBudget = 1800

	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", AllowedTools: []string{"Agent"}, SystemPrompt: "you are the parent"},
		// No RunTimeoutSeconds on parent → its own Call sees 0 (the
		// per-run knob is absent on the top-level test request too).
		"child": {Model: "stub-model", AllowedTools: []string{}, SystemPrompt: "you are the child", RunTimeoutSeconds: childBudget},
	}

	prov := &runMetaCapturingProvider{
		scripts: [][]providers.Event{
			{ // 1) parent spawns the child
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
					ID: "tu1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go"}`),
				}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			{ // 2) child
				{Type: providers.EventText, Text: "child done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}},
			},
			{ // 3) parent wraps up
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent_runtimeout.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		slurp, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, slurp)
	}
	_, _ = io.ReadAll(resp.Body) // drain so the parent + child runs finish

	prov.mu.Lock()
	captured := append([]int(nil), prov.capturedBudget...)
	prov.mu.Unlock()

	// call order: parent(1) → child(2) → parent(3). The child Call is the
	// one that must carry the per-agent budget.
	if len(captured) < 2 {
		t.Fatalf("expected at least 2 provider calls (parent + child), got %d", len(captured))
	}
	if captured[0] != 0 {
		t.Errorf("parent Call should see no per-agent budget (0); got %d", captured[0])
	}
	if captured[1] != childBudget {
		t.Errorf("child Call must inherit its per-agent run_timeout_seconds (%d); got %d — runSubAgent dropped RunTimeoutSeconds", childBudget, captured[1])
	}
}
