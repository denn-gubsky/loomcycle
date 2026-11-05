package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
)

// toolSeeingProvider records the tool names each call advertised, and answers
// with a plain end_turn.
type toolSeeingProvider struct {
	mu    sync.Mutex
	tools [][]string
}

func (p *toolSeeingProvider) ID() string                  { return "stub" }
func (p *toolSeeingProvider) Probe(context.Context) error { return nil }
func (p *toolSeeingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *toolSeeingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *toolSeeingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	var names []string
	for _, ts := range req.Tools {
		names = append(names, ts.Name)
	}
	p.mu.Lock()
	p.tools = append(p.tools, names)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "done"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

func (p *toolSeeingProvider) firstCallTools() ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tools) == 0 {
		return nil, false
	}
	return p.tools[0], true
}

// THE CROSSING: a top-level run, started over HTTP with user_credentials, on a
// cold pool, against a dynamic MCP server that answers tools/list only to the
// run's named credential. The run must be offered that server's tools at its
// first model call. Before the fix its discovery ran before the credentials
// reached the run, the server refused the handshake, and the run started with
// no tools — until a sub-agent warmed the pool.
func TestRunOnce_TopLevelRunDiscoversWithItsOwnCredentials(t *testing.T) {
	for _, tc := range []struct {
		name      string
		creds     string // the user_credentials field, or "" for none
		wantTools bool
	}{
		{"with the credential", `"user_credentials":{"jobs":"t"}`, true},
		{"without it", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := mcptest.NewServer(t, mcptest.WithRequiredAuthorization("Bearer t"), mcptest.WithToolName("getAgentContext"))
			headers := map[string]string{"Authorization": "Bearer ${run.credentials.jobs}"}
			pool := loommcp.NewPool(func(_, _ string) (loommcp.Caller, error) {
				return mcphttp.New(mcphttp.Config{URL: up.URL, Headers: headers})
			}, nil, func(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID })
			t.Cleanup(pool.Close)
			reg := loommcp.NewDynamicRegistry()
			reg.Set(loommcp.DynamicMCPServerSpec{Name: "jobs", Transport: "http", URL: up.URL, Headers: headers})
			reader := activeDefReaderFunc(func(_ context.Context, _, name string) (json.RawMessage, bool) {
				return json.RawMessage(`{"transport":"http"}`), name == "jobs" // no discovered_tools
			})

			cfg := &config.Config{
				Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
				Agents: map[string]config.AgentDef{
					"drafter": {Model: "stub-model", Tools: []string{"mcp__jobs__*"}, SystemPrompt: "adapt"},
				},
				Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2},
			}
			prov := &toolSeeingProvider{}
			srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(2, 2, time.Second), nil)
			srv.SetDynamicToolEnumerator(func(ctx context.Context, tenant string, want map[string]bool) []tools.Tool {
				return loommcp.DynamicToolsForRun(ctx, pool, reg, reader, tenant, want, 5*time.Second, nil)
			})

			body := `{"agent":"drafter","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]`
			if tc.creds != "" {
				body += "," + tc.creds
			}
			body += "}"
			rec := doRunRequest(t, srv, body)
			if rec.code != http.StatusOK {
				t.Fatalf("POST /v1/runs = %d: %s", rec.code, rec.body)
			}

			got, ok := prov.firstCallTools()
			if !ok {
				t.Fatalf("the provider was never called: %s", rec.body)
			}
			has := false
			for _, n := range got {
				if n == "mcp__jobs__getAgentContext" {
					has = true
				}
			}
			if has != tc.wantTools {
				t.Errorf("first call advertised %v; has mcp__jobs__getAgentContext = %v, want %v", got, has, tc.wantTools)
			}
			auth, sent := up.AuthorizationSeen("initialize")
			if !sent {
				t.Fatal("the run-start discovery never reached the server")
			}
			if tc.wantTools && auth != "Bearer t" {
				t.Errorf("initialize carried Authorization %q, want the run's credential", auth)
			}
			if !tc.wantTools && auth != "" {
				t.Errorf("initialize carried Authorization %q, want none", auth)
			}
		})
	}
}

type activeDefReaderFunc func(ctx context.Context, tenant, name string) (json.RawMessage, bool)

func (f activeDefReaderFunc) ActiveMCPServerDef(ctx context.Context, tenant, name string) (json.RawMessage, bool) {
	return f(ctx, tenant, name)
}

type runResult struct {
	code int
	body string
}

func doRunRequest(t *testing.T, srv *Server, body string) runResult {
	t.Helper()
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return runResult{resp.StatusCode, string(b)}
}
