package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The configured_run tool against the REAL HTTP server as its connector — the
// rules live there, so a fake would only test the argument mapping.

type draftAnswerProvider struct{ last *providers.Request }

func (p *draftAnswerProvider) ID() string                  { return "stub" }
func (p *draftAnswerProvider) Probe(context.Context) error { return nil }
func (p *draftAnswerProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *draftAnswerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *draftAnswerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	r := req
	p.last = &r
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "finished"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

type singleProvider struct{ p providers.Provider }

func (r singleProvider) Get(string) (providers.Provider, error) { return r.p, nil }

func configuredMCP(t *testing.T) (*Server, *draftAnswerProvider, store.Store) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "mcp-configured.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	prov := &draftAnswerProvider{}
	httpSrv := lchttp.New(cfg, singleProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	return New(Config{Connector: httpSrv, Logf: func(string, ...any) {}}), prov, st
}

// callTool runs one tools/call and returns the decoded result.
func callTool(t *testing.T, srv *Server, name string, args any) loommcp.CallToolResult {
	t.Helper()
	a, _ := json.Marshal(args)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, a),
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &res); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, resps[1].Result)
	}
	return res
}

func segment(text string) []map[string]any {
	return []map[string]any{{"role": "user", "content": []map[string]any{{"type": "trusted-text", "text": text}}}}
}

// create → update → start runs the edited draft to completion; delete
// discards a second draft; the refusals come back error-shaped and
// classified.
func TestConfiguredRunTool_CreateUpdateStartDelete(t *testing.T) {
	srv, prov, st := configuredMCP(t)

	res := callTool(t, srv, "configured_run", map[string]any{"op": "create", "agent": "agent", "user_id": "u1", "segments": segment("first")})
	if res.IsError {
		t.Fatalf("create: %v", res.Content)
	}
	var created connector.ConfiguredRun
	if err := json.Unmarshal([]byte(res.Content[0].Text), &created); err != nil || created.Status != "configured" || created.RunID == "" {
		t.Fatalf("created = %+v (%v)", created, err)
	}
	if prov.last != nil {
		t.Error("the provider was called for a draft")
	}

	if res := callTool(t, srv, "configured_run", map[string]any{
		"op": "update", "run_id": created.RunID, "patch": map[string]any{"prompt": "edited", "sampling": map[string]any{"temperature": 0.6}},
	}); res.IsError {
		t.Fatalf("update: %v", res.Content)
	}

	res = callTool(t, srv, "configured_run", map[string]any{"op": "start", "run_id": created.RunID})
	if res.IsError {
		t.Fatalf("start: %v", res.Content)
	}
	var result connector.SpawnRunResult
	if err := json.Unmarshal([]byte(res.Content[0].Text), &result); err != nil || result.FinalText != "finished" || result.RunID != created.RunID {
		t.Errorf("start result = %+v (%v), want this run's final text", result, err)
	}
	if prov.last == nil || prov.last.Temperature == nil || *prov.last.Temperature != 0.6 {
		t.Errorf("provider saw %+v, want the patched temperature", prov.last)
	}
	if run, _ := st.GetRun(context.Background(), created.RunID); run.Status != store.RunCompleted {
		t.Errorf("row after start = %q", run.Status)
	}

	// A second start is a business refusal, not retryable.
	res = callTool(t, srv, "configured_run", map[string]any{"op": "start", "run_id": created.RunID})
	if !res.IsError || !strings.Contains(string(res.StructuredContent), `"business"`) {
		t.Errorf("second start = isError %v %s, want a business refusal", res.IsError, res.StructuredContent)
	}

	res = callTool(t, srv, "configured_run", map[string]any{"op": "create", "agent": "agent", "segments": segment("x")})
	var second connector.ConfiguredRun
	_ = json.Unmarshal([]byte(res.Content[0].Text), &second)
	if res := callTool(t, srv, "configured_run", map[string]any{"op": "delete", "run_id": second.RunID}); res.IsError {
		t.Fatalf("delete: %v", res.Content)
	}
	if _, err := st.GetRun(context.Background(), second.RunID); err == nil {
		t.Error("the deleted draft is still there")
	}
}

// Secrets at create, an identity patch and a missing run_id are validation
// refusals the caller can fix; an unknown op too.
func TestConfiguredRunTool_RefusalsAreClassified(t *testing.T) {
	srv, _, _ := configuredMCP(t)
	for name, args := range map[string]map[string]any{
		"secret at create": {"op": "create", "agent": "agent", "segments": segment("x"), "user_bearer": "abcdefghijklmnopqrstuvwxyz"},
		"no run_id":        {"op": "start"},
		"unknown op":       {"op": "launch"},
		"no patch":         {"op": "update", "run_id": "r_1"},
	} {
		res := callTool(t, srv, "configured_run", args)
		if !res.IsError || !strings.Contains(string(res.StructuredContent), `"validation"`) {
			t.Errorf("%s = isError %v %s, want a validation refusal", name, res.IsError, res.StructuredContent)
		}
	}
	res := callTool(t, srv, "configured_run", map[string]any{"op": "create", "agent": "agent", "segments": segment("x")})
	var created connector.ConfiguredRun
	_ = json.Unmarshal([]byte(res.Content[0].Text), &created)
	res = callTool(t, srv, "configured_run", map[string]any{"op": "update", "run_id": created.RunID, "patch": map[string]any{"agent": "other"}})
	if !res.IsError || !strings.Contains(string(res.StructuredContent), `"validation"`) {
		t.Errorf("identity patch = isError %v %s, want a validation refusal", res.IsError, res.StructuredContent)
	}
}

// A blocking start holds the connection for the whole run, so the thin client
// must not header-timeout and retry it.
func TestConfiguredRunTool_IsALongRunTool(t *testing.T) {
	if !longRunTools["configured_run"] {
		t.Error("configured_run is missing from longRunTools: a header timeout would retry a start mid-run")
	}
	if !tenantConfinableTools["configured_run"] {
		t.Error("configured_run is not tenant-confinable: a tenant session could not reach its own drafts")
	}
}
