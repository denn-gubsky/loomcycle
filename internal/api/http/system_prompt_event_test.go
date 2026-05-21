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
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// v0.9.x — system_prompt transcript event tests. Pin the contract
// the Web UI relies on:
//
//   - POST /v1/runs persists a `system_prompt` event when the agent
//     has a SystemPrompt, alongside the existing `user_input`.
//   - Agents WITHOUT a SystemPrompt emit NO system_prompt event
//     (no empty payload pollution; the omitempty contract).
//
// Continuation (handleMessages) and sub-agent spawn paths are
// covered by the in-process behavioural contract (same helper
// emitSystemPromptEvent) — extending the broader test suite would
// require wiring a real sub-agent path which is much larger and
// out of scope for this change.

func TestSystemPromptEvent_PersistedOnRunsWhenAgentHasPrompt(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {
				Model:        "stub-model",
				SystemPrompt: "You are a careful researcher. Output JSON only.",
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 1}},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session_id in SSE stream:\n%s", body)
	}

	transcript, err := st.GetTranscript(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// Find the system_prompt event.
	var sysIdx, userIdx = -1, -1
	for i, ev := range transcript {
		switch ev.Type {
		case "system_prompt":
			sysIdx = i
		case "user_input":
			userIdx = i
		}
	}
	if sysIdx < 0 {
		t.Fatalf("no system_prompt event in transcript; got types: %v", typeList(transcript))
	}
	if userIdx < 0 {
		t.Fatalf("no user_input event in transcript; got types: %v", typeList(transcript))
	}
	// user_input must come before system_prompt because the
	// existing RunOnce emits user_input first then system_prompt
	// (mirrors the existing emission ordering in server.go).
	if userIdx > sysIdx {
		t.Errorf("expected user_input before system_prompt; got %d > %d", userIdx, sysIdx)
	}

	// Payload assertions.
	var payload map[string]any
	if err := json.Unmarshal(transcript[sysIdx].Payload, &payload); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if payload["system_prompt"] != "You are a careful researcher. Output JSON only." {
		t.Errorf("system_prompt mismatch: %v", payload["system_prompt"])
	}
	// agent_def_id is empty for yaml-only agents (no DB row).
	if v, ok := payload["agent_def_id"]; ok {
		t.Errorf("agent_def_id should be omitted for yaml-only agent, got %v", v)
	}
	// No skill_def_ids when the agent has no Skills.
	if v, ok := payload["skill_def_ids"]; ok {
		t.Errorf("skill_def_ids should be omitted when agent has no Skills, got %v", v)
	}
}

func TestSystemPromptEvent_NotEmittedWhenAgentHasNoPrompt(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			// SystemPrompt explicitly empty.
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 1}},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	sessionID := extractSessionID(string(body))

	transcript, err := st.GetTranscript(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range transcript {
		if ev.Type == "system_prompt" {
			t.Errorf("agent with empty SystemPrompt should not emit system_prompt event; got %s", ev.Payload)
		}
	}
}

// Verify the helper handles nil store + empty runID gracefully —
// this is the test-without-a-store path RunOnce uses.
func TestEmitSystemPromptEvent_NoOpOnEmptyInputs(t *testing.T) {
	// nil store.
	srv := &Server{}
	srv.emitSystemPromptEvent(context.Background(), "run_test", "you are X", "def_1", runPromptProvenance{})
	// no panic = pass.

	// store set but empty runID.
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv2 := &Server{store: st}
	srv2.emitSystemPromptEvent(context.Background(), "", "you are X", "def_1", runPromptProvenance{})

	// store set + runID set, but no system prompt.
	srv2.emitSystemPromptEvent(context.Background(), "run_test", "", "def_1", runPromptProvenance{})
}

// Helper: extract event types into a flat slice for error messages.
func typeList(events []store.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}
