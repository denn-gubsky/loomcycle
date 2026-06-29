package http

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// capCfg builds a minimal config with a small MaxRequestBytes so the cap can be
// exercised without posting megabytes.
func capCfg(limit int64) *config.Config {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"a": {Model: "stub-model", SystemPrompt: "x"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.MaxRequestBytes = limit
	return cfg
}

func capServer(t *testing.T, limit int64) *httptest.Server {
	t.Helper()
	prov := &scriptedProvider{scripts: [][]providers.Event{{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}}
	srv := New(capCfg(limit), &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

// TestRequestCap_OverLimitReturns413: a /v1/runs body larger than the configured
// cap is rejected with 413 (not silently processed).
func TestRequestCap_OverLimitReturns413(t *testing.T) {
	ts := capServer(t, 512)
	// A body comfortably over 512 bytes: pad the text block.
	big := strings.Repeat("A", 4096)
	payload := fmt.Sprintf(`{"agent":"a","segments":[{"role":"user","content":[{"type":"trusted-text","text":%q}]}]}`, big)

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413; body: %s", resp.StatusCode, body)
	}
}

// TestRequestCap_UnderLimitAccepted: a /v1/runs body under the cap is NOT
// rejected by the byte cap — it reaches the handler and runs (200).
func TestRequestCap_UnderLimitAccepted(t *testing.T) {
	ts := capServer(t, 16<<20)
	payload := `{"agent":"a","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
}
