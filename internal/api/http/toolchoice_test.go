package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func toolChoiceServer(t *testing.T) (*Server, *httptest.Server, *recordingProvider, *storesqlite.Store) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {
				Model: "stub-model", SystemPrompt: "hi",
				ToolChoice: &config.ToolChoice{Mode: "none", Until: "always"},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &recordingProvider{}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "toolchoice.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, prov, st
}

// A per-run tool_choice REPLACES the agent's whole — the agent's until:always
// does not ride along onto the run's `required` — reaches the provider, and is
// persisted with the run so resume keeps it.
func TestHandleRuns_PerRunToolChoiceReplacesTheAgentsAndIsPersisted(t *testing.T) {
	_, ts, prov, st := toolChoiceServer(t)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","tool_choice":{"mode":"required"},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if prov.last == nil || prov.last.ToolChoice.Mode != "required" {
		t.Fatalf("provider saw tool_choice %+v, want the run's `required`", prov.last)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.ToolChoice == nil || rec.ToolChoice.Mode != "required" || rec.ToolChoice.Until != "" {
		t.Errorf("persisted tool_choice = %+v, want exactly the run's choice", rec.ToolChoice)
	}
}

// A choice that could never let the run finish is a 400 at intake on every
// entry path, not a run that spins to its iteration cap.
func TestRunIntake_RefusesAnInvalidToolChoice(t *testing.T) {
	srv, ts, prov, _ := toolChoiceServer(t)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","tool_choice":{"mode":"required","until":"always"},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "never give a final answer") {
		t.Errorf("POST /v1/runs = %d %s, want a 400 naming why", resp.StatusCode, body)
	}
	if prov.last != nil {
		t.Error("the provider was called for a refused run")
	}

	err = srv.RunOnce(context.Background(), runner.RunInput{
		Agent:      "agent",
		ToolChoice: &config.ToolChoice{Mode: "tool"},
	}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("RunOnce = %v, want ErrInvalidArgument", err)
	}
}

// A resumed run re-enters the loop with a fresh policy, so the resume path must
// know what the run already spent: first_call is spent by any completed model
// call, until_called only by a call that satisfies it, always never.
func TestToolChoiceSpent_ReadsTheRunsOwnEvents(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	ctx := context.Background()
	run := seedTenantRun(t, srv.store, "acme", "u1", "a_spent")
	first := &config.ToolChoice{Mode: "tool", Name: "WebSearch"}
	until := &config.ToolChoice{Mode: "tool", Name: "WebSearch", Until: "until_called"}

	if toolChoiceSpent(ctx, srv.store, run.ID, first) {
		t.Error("a run with no model call has not spent first_call")
	}
	appendEv := func(typ string, payload string) {
		if err := srv.store.AppendEvent(ctx, run.ID, typ, []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	appendEv("usage", `{"type":"usage","usage":{}}`)
	appendEv("tool_call", `{"type":"tool_call","tool_use":{"id":"1","name":"Echo","input":{}}}`)
	if !toolChoiceSpent(ctx, srv.store, run.ID, first) {
		t.Error("a completed model call spends first_call")
	}
	if toolChoiceSpent(ctx, srv.store, run.ID, until) {
		t.Error("a call to a DIFFERENT tool does not spend until_called")
	}
	appendEv("tool_call", `{"type":"tool_call","tool_use":{"id":"2","name":"WebSearch","input":{}}}`)
	if !toolChoiceSpent(ctx, srv.store, run.ID, until) {
		t.Error("the requested call spends until_called")
	}
	if toolChoiceSpent(ctx, srv.store, run.ID, &config.ToolChoice{Mode: "none", Until: "always"}) {
		t.Error("always is never spent")
	}
}
