package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestSpawnRunStreaming_UsageIsTheRunsCumulativeUsage pins RFC GK D5 on the
// streaming spawn path: the result's usage is the run's total (the loop's
// closing EventDone), not the last model call's EventUsage.
func TestSpawnRunStreaming_UsageIsTheRunsCumulativeUsage(t *testing.T) {
	fr := &fakeRunner{agentID: "a_1", runID: "r_1", sessionID: "s_1", events: []providers.Event{
		{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50}},
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 200, OutputTokens: 20, CacheReadTokens: 70}},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 300, OutputTokens: 30, CacheReadTokens: 120}},
	}}
	sess := NewSession()
	sess.MarkInitialized()
	sess.SetRunEventsEnabled(true)
	env := &handlerEnv{connector: &mockConnector{}, runner: fr, session: sess, notify: func(string, any) {}}
	res, err := spawnRunStreaming(context.Background(), env, connector.SpawnRunRequest{Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Usage; u == nil || u.InputTokens != 300 || u.OutputTokens != 30 || u.CacheReadTokens != 120 {
		t.Errorf("usage = %+v, want the run's 300/30/120", res.Usage)
	}
}
