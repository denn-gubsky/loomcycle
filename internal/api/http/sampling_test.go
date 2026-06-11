package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// (recordingProvider — captures the last providers.Request — lives in
// llm_gateway_test.go.)

func f64(v float64) *float64 { return &v }

// TestHandleRuns_PerRunSamplingOverridesAgent: a /v1/runs `sampling` block wins
// PER FIELD over the agent's own sampling (temperature overridden; top_p
// inherited from the agent). The resolved value reaches the provider.
func TestHandleRuns_PerRunSamplingOverridesAgent(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {
				Model:        "stub-model",
				SystemPrompt: "hi",
				Sampling:     &config.Sampling{Temperature: f64(0.2), TopP: f64(0.9)},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &recordingProvider{}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "sampling.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Per-run override sets ONLY temperature → wins; top_p inherits the agent's.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","sampling":{"temperature":0.95},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	req := prov.last
	if req == nil {
		t.Fatal("provider received no request")
	}
	if req.Temperature == nil || *req.Temperature != 0.95 {
		t.Errorf("temperature = %v, want 0.95 (per-run override wins)", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9 (inherited from the agent — per-run left it unset)", req.TopP)
	}
}

// TestHandleRuns_AgentSamplingWhenNoOverride: with no per-run sampling, the
// agent's own sampling reaches the provider unchanged.
func TestHandleRuns_AgentSamplingWhenNoOverride(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi", Sampling: &config.Sampling{Temperature: f64(0.1)}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &recordingProvider{}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "sampling2.db"))
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	req := prov.last
	if req == nil {
		t.Fatal("provider received no request")
	}
	if req.Temperature == nil || *req.Temperature != 0.1 {
		t.Errorf("temperature = %v, want 0.1 (agent default reaches the wire)", req.Temperature)
	}
}
