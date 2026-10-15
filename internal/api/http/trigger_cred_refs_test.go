package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// refsAtTenantProvider records, per model call, whether the run identity on
// the call's ctx says its credential references resolve at tenant level.
type refsAtTenantProvider struct {
	calls   atomic.Int32
	scripts [][]providers.Event

	mu   sync.Mutex
	seen []bool
}

func (p *refsAtTenantProvider) ID() string                    { return "refs-at-tenant" }
func (p *refsAtTenantProvider) Probe(_ context.Context) error { return nil }
func (p *refsAtTenantProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *refsAtTenantProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *refsAtTenantProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.RunIdentity(ctx).CredentialRefsAtTenant)
	p.mu.Unlock()
	idx := int(p.calls.Add(1)) - 1
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

// A run a trigger started carries "its credential references are its
// definition's" into its own calls and into every sub-agent it spawns: the
// child sends the same references, so they must resolve the same way. A run
// its user started carries nothing of the kind.
func TestRunOnce_ATriggersCredentialRuleReachesTheRunAndItsSubAgents(t *testing.T) {
	for _, atTenant := range []bool{true, false} {
		cfg := makeBaseConfig()
		cfg.Agents = map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the parent"},
			"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are the child"},
		}
		prov := &refsAtTenantProvider{scripts: [][]providers.Event{
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go"}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			{
				{Type: providers.EventText, Text: "child done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}},
			},
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		}}
		st, err := storesqlite.Open(filepath.Join(t.TempDir(), "trigger_refs.db"))
		if err != nil {
			t.Fatal(err)
		}
		srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
		err = srv.RunOnce(context.Background(), runner.RunInput{
			Agent:                  "parent",
			UserCredentials:        map[string]string{"gh": "$cred:tok"},
			CredentialRefsAtTenant: atTenant,
			Segments:               []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "start"}}}},
		}, runner.RunCallbacks{})
		_ = st.Close()
		if err != nil {
			t.Fatalf("atTenant=%v: RunOnce: %v", atTenant, err)
		}
		prov.mu.Lock()
		seen := append([]bool(nil), prov.seen...)
		prov.mu.Unlock()
		if len(seen) < 3 {
			t.Fatalf("atTenant=%v: %d model calls, want the parent's two and the child's", atTenant, len(seen))
		}
		for i, v := range seen {
			if v != atTenant {
				t.Errorf("atTenant=%v: call #%d saw %v", atTenant, i, v)
			}
		}
	}
}

// A trigger run that queued, and is started again after a restart, keeps the
// rule: its sealed credentials come back with it, and would otherwise resolve
// for the run's user.
func TestQueuedRun_ATriggersCredentialRuleSurvivesARestart(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	s.SetQueueSealer(testSealer(t, 1))
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds the slot")})
	holder.ids(t)

	queued := startRunOnce(s, context.Background(), runner.RunInput{
		Agent: "r", TenantID: "acme", UserID: "alice", Segments: oneUserSeg("a webhook's run"),
		UserCredentials: map[string]string{"gh": "$cred:tok"}, CredentialRefsAtTenant: true,
	})
	_, runID := queued.ids(t)

	q := store.QueuedRun{Input: queuedRowInput(t, s, runID)}
	q.Run, _ = s.store.GetRun(context.Background(), runID)
	in, why := s.restartInput(q)
	if why != "" {
		t.Fatalf("restartInput: %s", why)
	}
	if !in.CredentialRefsAtTenant || in.UserCredentials["gh"] != "$cred:tok" {
		t.Errorf("restored: refsAtTenant=%v credentials=%v, want the trigger's rule with its reference", in.CredentialRefsAtTenant, in.UserCredentials)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	_ = holder.wait(t)
	_ = queued.wait(t)
}
