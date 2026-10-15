package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run whose turn call waits for a saturated provider's lane lends its global
// run slot: a run bound for an idle provider starts and finishes meanwhile,
// where it used to wait behind runs that were not computing. The waiting run
// takes a slot back before its call computes, and every run completes.
func TestRunSlot_ARunWaitingForABusyLaneDoesNotKeepAnIdleProvidersRunOut(t *testing.T) {
	busy := &pausableProvider{release: make(chan struct{}), finalText: "slow"}
	idle := &pausableProvider{release: make(chan struct{}), finalText: "quick"}
	close(idle.release)
	cfg := makeBaseConfig()
	cfg.Agents["slow"] = config.AgentDef{Provider: "B", Model: "m", Tools: []string{}}
	cfg.Agents["quick"] = config.AgentDef{Provider: "F", Model: "m", Tools: []string{}}
	srv, sched := lanedServer(t, cfg, map[string]int{"B": 1}, map[string]providers.Provider{"B": busy, "F": idle})
	srv.sem = concurrency.New(2, 8, 0) // two runs at a time
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	run := func(agent, id string) chan string {
		done := make(chan string, 1)
		go func() { done <- postLaneRun(t, ts, laneRunBody(agent, id)) }()
		return done
	}
	slow1 := run("slow", "slow-1")
	waitForLane(t, sched, "B", 1, 0) // computing on B
	slow2 := run("slow", "slow-2")
	waitForLane(t, sched, "B", 1, 1) // waiting for B's only slot

	select {
	case out := <-run("quick", "quick-3"):
		if out == "" {
			t.Fatal("the quick run failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a run bound for an idle provider did not finish while another run only waited for a busy lane — the waiting run kept its global slot")
	}

	close(busy.release)
	for _, c := range []chan string{slow1, slow2} {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatal("a slow run did not finish once the busy provider freed")
		}
	}
	assertLaneIdle(t, sched, "B")
	if st := srv.sem.Stats(); st.Active != 0 || st.Queued != 0 || st.Lent != 0 {
		t.Errorf("runtime after all runs: %+v, want nothing active, queued or lent", st)
	}
}

// slotSeeingProvider records, per model call, whether the call's ctx carries a
// run slot the lane could lend.
type slotSeeingProvider struct {
	recordingScriptedProvider
	mu   sync.Mutex
	seen []bool
}

func (p *slotSeeingProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.seen = append(p.seen, concurrency.RunSlotFrom(ctx) != nil)
	p.mu.Unlock()
	return p.recordingScriptedProvider.Call(ctx, req)
}

// The run's slot rides only its own loop's calls: a sub-agent's calls run
// under a ctx that carries none, so a child — which takes no admission slot —
// can never lend its parent's (several children would lend and reclaim one
// slot at once).
func TestRunSlot_ASubAgentsCallsCarryNoSlot(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the parent"},
		"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are the child"},
	}
	spawn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	prov := &slotSeeingProvider{recordingScriptedProvider: recordingScriptedProvider{scripts: [][]providers.Event{spawn, endTurn(), endTurn()}, defaultS: endTurn()}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "slot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	if out := postLaneRun(t, ts, laneRunBody("parent", "slot-parent")); out == "" {
		t.Fatal("the run failed")
	}
	prov.mu.Lock()
	seen := append([]bool(nil), prov.seen...)
	prov.mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("model calls = %d, want the parent's two and the child's one", len(seen))
	}
	if !seen[0] || !seen[2] {
		t.Errorf("the parent's calls carried no slot (%v) — its turn calls could not lend", seen)
	}
	if seen[1] {
		t.Error("the child's call carried a slot — its parent's, which is not the child's to lend")
	}
}
