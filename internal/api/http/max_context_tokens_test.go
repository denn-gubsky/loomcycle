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
// llm_gateway_test.go; f64 lives in sampling_test.go, same package.)

// newMaxCtxTestServer builds a minimal /v1/runs server backed by a
// recordingProvider so a test can inspect the providers.Request the loop
// assembled — the seam where the resolved MaxContextTokens lands (loop.go
// sets Request.MaxContextTokens = opts.MaxContextTokens).
func newMaxCtxTestServer(t *testing.T, agentMaxCtx int) (*httptest.Server, *recordingProvider) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {
				Model:            "stub-model",
				SystemPrompt:     "hi",
				MaxContextTokens: agentMaxCtx,
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &recordingProvider{}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "maxctx.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, prov
}

func postRunMaxCtx(t *testing.T, ts *httptest.Server, body string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestHandleRuns_PerRunMaxContextTokensOverridesAgent: a /v1/runs
// `max_context_tokens` wins over the agent's own max_context_tokens; the
// resolved window reaches the provider request. Regression for RFC CJ phase 2.
func TestHandleRuns_PerRunMaxContextTokensOverridesAgent(t *testing.T) {
	ts, prov := newMaxCtxTestServer(t, 8192)
	postRunMaxCtx(t, ts, `{"agent":"agent","max_context_tokens":16384,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)

	if prov.last == nil {
		t.Fatal("provider received no request")
	}
	if prov.last.MaxContextTokens != 16384 {
		t.Errorf("max_context_tokens = %d, want 16384 (per-run override wins over agent's 8192)", prov.last.MaxContextTokens)
	}
}

// TestHandleRuns_AgentMaxContextTokensWhenNoOverride: with no per-run value,
// the agent's own max_context_tokens reaches the provider unchanged.
func TestHandleRuns_AgentMaxContextTokensWhenNoOverride(t *testing.T) {
	ts, prov := newMaxCtxTestServer(t, 8192)
	postRunMaxCtx(t, ts, `{"agent":"agent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)

	if prov.last == nil {
		t.Fatal("provider received no request")
	}
	if prov.last.MaxContextTokens != 8192 {
		t.Errorf("max_context_tokens = %d, want 8192 (inherited from the agent — no per-run override)", prov.last.MaxContextTokens)
	}
}

// TestHandleRuns_PerRunZeroMaxContextTokensInheritsAgent: an explicit 0 per-run
// value is "unset" (a window is never meaningfully 0), so it inherits the
// agent's — proving the merge treats 0 as absence, not a floor.
func TestHandleRuns_PerRunZeroMaxContextTokensInheritsAgent(t *testing.T) {
	ts, prov := newMaxCtxTestServer(t, 8192)
	postRunMaxCtx(t, ts, `{"agent":"agent","max_context_tokens":0,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)

	if prov.last == nil {
		t.Fatal("provider received no request")
	}
	if prov.last.MaxContextTokens != 8192 {
		t.Errorf("max_context_tokens = %d, want 8192 (per-run 0 = unset → inherit agent's)", prov.last.MaxContextTokens)
	}
}
