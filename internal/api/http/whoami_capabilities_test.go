package http

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestWhoami_CapabilitiesReflectConfig verifies the RFC AU capability
// advertisement on GET /v1/_me: the UI needs mcp_allow_dynamic_stdio to gate the
// stdio import path (host RCE), and http_host_allowlist_configured to warn about
// http imports — and the block must NEVER leak the allowlist CONTENTS.
func TestWhoami_CapabilitiesReflectConfig(t *testing.T) {
	newServer := func(stdio bool, allowlist []string) *Server {
		cfg := &config.Config{
			Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
			Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 1000},
		}
		cfg.Env.AuthToken = "" // open mode → the no-principal whoami branch
		cfg.Env.MCPAllowDynamicStdio = stdio
		cfg.Env.HTTPHostAllowlist = allowlist
		return New(cfg, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), nil)
	}

	get := func(t *testing.T, srv *Server) (map[string]any, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/_me", nil))
		if rec.Code != 200 {
			t.Fatalf("GET /v1/_me = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
		caps, ok := body["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("no capabilities object in /v1/_me; body=%s", rec.Body.String())
		}
		return caps, rec.Body.String()
	}

	t.Run("stdio off, no allowlist", func(t *testing.T) {
		caps, _ := get(t, newServer(false, nil))
		if caps["mcp_allow_dynamic_stdio"] != false {
			t.Errorf("mcp_allow_dynamic_stdio = %v, want false", caps["mcp_allow_dynamic_stdio"])
		}
		if caps["http_host_allowlist_configured"] != false {
			t.Errorf("http_host_allowlist_configured = %v, want false", caps["http_host_allowlist_configured"])
		}
	})

	t.Run("stdio on, allowlist set — reflects flags but never leaks contents", func(t *testing.T) {
		const secretHost = "internal-mcp.corp.example"
		caps, raw := get(t, newServer(true, []string{secretHost}))
		if caps["mcp_allow_dynamic_stdio"] != true {
			t.Errorf("mcp_allow_dynamic_stdio = %v, want true", caps["mcp_allow_dynamic_stdio"])
		}
		if caps["http_host_allowlist_configured"] != true {
			t.Errorf("http_host_allowlist_configured = %v, want true", caps["http_host_allowlist_configured"])
		}
		// The allowlist host must never appear in the response — booleans only.
		if strings.Contains(raw, secretHost) {
			t.Errorf("/v1/_me leaked an allowlist hostname %q; body=%s", secretHost, raw)
		}
	})
}
