package http

import (
	"context"
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
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestNew_ContextToolAdvertisesAgentTool is the F45 regression: the server
// appends the Agent tool to its own tool set in New(), so the Context tool's
// catalog (what `Context op=tools` lists) must be re-pointed to the COMPLETE
// set at serve time — otherwise Agent is silently omitted. Fail-before: without
// the New()-side wiring the Context tool's catalog stays as passed (no Agent).
func TestNew_ContextToolAdvertisesAgentTool(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	ctxTool := &builtin.Context{Cfg: cfg}
	srv := New(cfg, &stubResolver{p: &scriptedProvider{}}, []tools.Tool{ctxTool}, concurrency.New(4, 4, time.Second), nil)
	_ = srv

	var foundAgent, foundContext bool
	for _, tl := range ctxTool.Tools {
		switch tl.Name() {
		case "Agent":
			foundAgent = true
		case "Context":
			foundContext = true
		}
	}
	if !foundAgent {
		t.Errorf("Context tool catalog missing Agent (F45); has %d tools", len(ctxTool.Tools))
	}
	if !foundContext {
		t.Errorf("Context tool catalog should still include the tools it was built with (Context); has %d", len(ctxTool.Tools))
	}
}

// echoCfg builds a minimal config with one scripted agent "echo".
func echoCfg() *config.Config {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"echo": {Model: "stub-model", AllowedTools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	return cfg
}

// TestHandleRuns_PromptSugarExpandsToUserSegment is the F47 regression for the
// `prompt` convenience field: a caller that sends {"prompt":"..."} with no
// `segments` must have it expanded into a trusted-text user segment that
// actually reaches the model (recorded on the transcript as user_input).
// Fail-before: without the sugar, `prompt` is dropped → no user segment.
func TestHandleRuns_PromptSugarExpandsToUserSegment(t *testing.T) {
	cfg := echoCfg()
	prov := &scriptedProvider{scripts: [][]providers.Event{
		{
			{Type: providers.EventText, Text: "ok"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "f47.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"echo","prompt":"hello-sugar-XYZ"}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session_id in stream:\n%s", body)
	}

	transcript, err := st.GetTranscript(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var sawUserInput bool
	for _, ev := range transcript {
		if ev.Type == "user_input" {
			sawUserInput = true
			payload := string(ev.Payload)
			if !strings.Contains(payload, "hello-sugar-XYZ") {
				t.Errorf("user_input missing the prompt text (sugar didn't expand): %s", payload)
			}
			if !strings.Contains(payload, "trusted-text") {
				t.Errorf("prompt sugar should produce a trusted-text block: %s", payload)
			}
		}
	}
	if !sawUserInput {
		t.Errorf("no user_input event recorded — the run carried no input")
	}
}

// TestHandleRuns_RejectsEmptyInput is the F47 regression for the empty guard: a
// run with neither `segments` nor `prompt` must return a clear 400 rather than
// dispatching an empty messages array to the provider. Fail-before: without the
// guard the empty run reaches the (lenient mock) provider and returns 200.
func TestHandleRuns_RejectsEmptyInput(t *testing.T) {
	cfg := echoCfg()
	srv := New(cfg, &stubResolver{p: &scriptedProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"echo"}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no input") {
		t.Errorf("400 body should explain the empty-input cause; got: %s", body)
	}
}
