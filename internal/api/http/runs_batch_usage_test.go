package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// twoCallProvider answers each run in two model calls: the first asks for a
// tool, the second ends the turn. It decides by the conversation, not by a
// shared call counter, so concurrent batch children each get both calls.
// Usage rides EventDone, as every real driver reports it (the loop
// synthesises EventUsage from it).
type twoCallProvider struct{}

func (twoCallProvider) ID() string                    { return "stub" }
func (twoCallProvider) Probe(_ context.Context) error { return nil }
func (twoCallProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (twoCallProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (twoCallProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 3)
	if len(req.Messages) <= 1 {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read", Input: json.RawMessage(`{}`)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use",
			Usage: &providers.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50, CacheCreationTokens: 5}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "done"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
			Usage: &providers.Usage{InputTokens: 200, OutputTokens: 20, CacheReadTokens: 70}}
	}
	close(ch)
	return ch, nil
}

func newTwoCallServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"r": {Model: "stub-model", Tools: []string{"Read"}, SystemPrompt: "be brief"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.AuthToken = "" // open mode
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(cfg, &stubResolver{p: twoCallProvider{}}, []tools.Tool{namedStubTool{"Read"}}, concurrency.New(4, 4, 2*time.Second), st)
}

// The run's cumulative figures under twoCallProvider: both calls summed.
var twoCallRunUsage = providers.Usage{InputTokens: 300, OutputTokens: 30, CacheReadTokens: 120, CacheCreationTokens: 5}

// TestSpawnRunBatch_UsageIsTheRunsCumulativeUsage pins RFC GK D5: a batch
// child's usage is the whole run's, the figures get_run reports, not the
// last model call's. Each child makes two model calls, so the last call's
// figures differ from the run's.
func TestSpawnRunBatch_UsageIsTheRunsCumulativeUsage(t *testing.T) {
	s := newTwoCallServer(t)
	ctx := context.Background()
	res, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
		{Agent: "r", Segments: oneUserSeg("a")},
		{Agent: "r", Segments: oneUserSeg("b")},
	}})
	if err != nil {
		t.Fatalf("SpawnRunBatch: %v", err)
	}
	var batchSum, runSum providers.Usage
	for i, r := range res.Results {
		if r.Status != "completed" || r.Usage == nil {
			t.Fatalf("child %d: status %q usage %v err %q", i, r.Status, r.Usage, r.Error)
		}
		run, err := s.GetRunByRunID(ctx, r.RunID)
		if err != nil || run.Usage == nil {
			t.Fatalf("child %d: GetRunByRunID: %v (usage %v)", i, err, run.Usage)
		}
		addTokenCounts(&batchSum, r.Usage)
		addTokenCounts(&runSum, run.Usage)
	}
	want := twoCallRunUsage
	addTokenCounts(&want, &twoCallRunUsage)
	if runSum != want {
		t.Fatalf("get_run sum = %+v, want %+v (fixture changed?)", runSum, want)
	}
	if batchSum != runSum {
		t.Errorf("summed batch usage = %+v, summed get_run usage = %+v", batchSum, runSum)
	}
}

// TestSpawnRun_UsageIsTheRunsCumulativeUsage: the single blocking spawn shares
// the fold, and had the same defect.
func TestSpawnRun_UsageIsTheRunsCumulativeUsage(t *testing.T) {
	s := newTwoCallServer(t)
	r, err := s.SpawnRun(context.Background(), connector.SpawnRunRequest{Agent: "r", Segments: oneUserSeg("a")})
	if err != nil || r.Usage == nil {
		t.Fatalf("SpawnRun: %v (usage %v, status %q, err %q)", err, r.Usage, r.Status, r.Error)
	}
	var got providers.Usage
	addTokenCounts(&got, r.Usage)
	if got != twoCallRunUsage {
		t.Errorf("spawn usage = %+v, want the run's cumulative %+v", got, twoCallRunUsage)
	}
}

// addTokenCounts sums only the token counts the comparisons are about.
func addTokenCounts(dst *providers.Usage, u *providers.Usage) {
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.CacheReadTokens += u.CacheReadTokens
	dst.CacheCreationTokens += u.CacheCreationTokens
}
