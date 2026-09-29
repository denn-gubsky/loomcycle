package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A sub-run's run-state events name the run that spawned it, so a subscriber
// can place a live run in the tree without reading the row back. Before, the
// stream carried only parent_agent_id, which every run of an agent reuses.
// Read off the user-agents SSE stream, which serialises the connector event —
// the same one the gRPC and MCP streams map from.
func TestStreamUserAgents_SubRunEventsCarryTheParentRunID(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "parent"},
		"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "child"},
	}
	done := func(text string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventText, Text: text},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_spawn", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"hi"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
		done("child answer"),
	}, defaultS: done("parent done")}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runstate_parent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)

	reader, cancel, _ := openStream(t, srv, "u1", "")
	defer cancel()
	if ev, _, ok := readSSEFrame(t, reader); !ok || ev != "stream_open" {
		t.Fatalf("first frame = %q, want stream_open", ev)
	}
	waitForSubscriber(t, bus)

	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"a_rs_parent","user_id":"u1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "parent done") {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}

	// running + completed for each run; the parent finishes last.
	var parentEvents, childEvents []connector.RunStateEvent
	for len(parentEvents) < 2 || len(childEvents) < 2 {
		ev, data, ok := readSSEFrame(t, reader)
		if !ok {
			t.Fatalf("stream ended after parent %d / child %d events", len(parentEvents), len(childEvents))
		}
		if ev != "run_state" {
			continue
		}
		var e connector.RunStateEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		switch e.Agent {
		case "parent":
			parentEvents = append(parentEvents, e)
			if strings.Contains(data, `"parent_run_id"`) {
				t.Errorf("top-level run's %s event carries parent_run_id: %s", e.Status, data)
			}
		case "child":
			childEvents = append(childEvents, e)
		}
	}
	parentRunID := parentEvents[0].RunID
	for _, e := range childEvents {
		if e.ParentRunID != parentRunID {
			t.Errorf("child's %s event parent_run_id = %q, want the parent's run %q", e.Status, e.ParentRunID, parentRunID)
		}
	}
}

// A sub-run re-dispatched after a pause keeps announcing its parent: resume
// rebuilds the run's run-state identity from the row, and must take the parent
// run with it.
func TestResumePausedRuns_ResumedChildEventsCarryItsParentRunID(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "resume"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "resumed"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ := makeServer(t, prov, cfg)
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	sub := bus.Subscribe("alice")
	defer sub.Close()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_rs_resumed", ParentAgentID: "a_parent", ParentRunID: "r_parent", UserID: "alice", Model: "stub-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go on"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	for {
		select {
		case ev := <-sub.C:
			if ev.RunID != run.ID {
				continue
			}
			if ev.ParentRunID != "r_parent" {
				t.Errorf("resumed child's %s event parent_run_id = %q, want r_parent", ev.Status, ev.ParentRunID)
			}
			if ev.Status != "running" {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("resumed run published no terminal event")
		}
	}
}
