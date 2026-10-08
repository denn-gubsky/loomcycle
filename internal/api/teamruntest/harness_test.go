// Package teamruntest holds the tests of who may start, read and stop a team
// walk through the team-definition tool OUTSIDE a run, driven through the
// three real surfaces at once: POST /v1/_teamdef, the gRPC TeamDef RPC and the
// MCP `teamdef` tool, each behind its real authentication (a bearer resolved
// by the server, the route and RPC scope gates, the MCP per-tool gate) over
// one real HTTP server.
//
// It is a package of its own because it needs all three transports, and the
// transports' own test packages cannot import each other.
package teamruntest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	lcgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Test fixtures, not secrets: the bearers below exist only inside these tests'
// in-memory servers.
const (
	pepper      = "pep-teamrun"
	legacyToken = "legacy-teamrun-test"
	tenant      = "acme"
	teamName    = "solo"
	leadPrompt  = "you are the lead"
)

// soloTeam is one agent state and a terminal: the smallest walk with a member,
// so a walk that was admitted is one the model was called for.
const soloTeam = `{"entry":"work","states":[` +
	`{"state":"work","handler":{"kind":"agent","agent":"worker"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"work","to":"done","on":"success"}]}`

// countingProvider answers every member call and counts them: a member run
// that reached the model is a walk that was let through. A call for the "lead"
// agent is answered from leadScript instead, and recorded.
type countingProvider struct {
	mu         sync.Mutex
	calls      int
	leadScript [][]providers.Event
	leadCalls  []providers.Request
}

func (p *countingProvider) ID() string                  { return "stub" }
func (p *countingProvider) Probe(context.Context) error { return nil }
func (p *countingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *countingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *countingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, leadPrompt) {
		i := len(p.leadCalls)
		p.leadCalls = append(p.leadCalls, req)
		if i >= len(p.leadScript) {
			p.mu.Unlock()
			return nil, errors.New("the lead's script is exhausted")
		}
		events = p.leadScript[i]
	} else {
		p.calls++
		events = says("done")
	}
	p.mu.Unlock()
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *countingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func says(text string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func callsTeamDef(id, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "TeamDef", Input: json.RawMessage(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// toolResults are the tool results the lead was shown, in order, read off the
// requests it made.
func (p *countingProvider) toolResults() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.leadCalls) == 0 {
		return nil
	}
	var out []string
	for _, m := range p.leadCalls[len(p.leadCalls)-1].Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

type oneProvider struct{ p providers.Provider }

func (r oneProvider) Get(string) (providers.Provider, error) { return r.p, nil }

type env struct {
	t    *testing.T
	st   store.Store
	ts   *httptest.Server
	prov *countingProvider
	grpc loomcyclepb.LoomcycleClient
}

// newEnv builds one real server reachable three ways, with the team "solo"
// active in tenant acme and in the two tenants a caller with no minted token
// works in. Everything a call crosses is the production code: the auth
// middleware and the scope tables, the MCP handler mounted at /v1/_mcp, the
// gRPC interceptors, the TeamDef tool and its admission. Only the model is a
// double.
func newEnv(t *testing.T, open bool) *env {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"worker": {Model: "stub-model", SystemPrompt: "work"},
			// An agent granted the tool, for the test of a call made inside a run.
			"lead": {Model: "stub-model", Tools: []string{"TeamDef"}, SystemPrompt: leadPrompt},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 2000},
	}
	cfg.Env.OperatorTokenPepper = pepper
	if !open {
		cfg.Env.AuthToken = legacyToken
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "teamrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	prov := &countingProvider{}
	team := &builtin.TeamDef{Store: st}
	srv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{team}, concurrency.New(4, 4, 2*time.Second), st)
	srv.SetTeamDefTool(team)
	srv.SetMCPHTTPHandler(lcmcp.NewHTTPHandler(lcmcp.Config{
		Connector: srv, Runner: srv, Store: st, Logf: func(string, ...any) {},
	}))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	adapter := lcgrpc.New(lcgrpc.Config{
		Store: st, CancelReg: cancel.NewRegistry(), Connector: srv, Runner: srv,
		Limits: srv.LimitsTracker(), AuthToken: cfg.Env.AuthToken,
		PrincipalResolver: srv.ResolvePrincipal, AuthConfigured: srv.AuthConfigured,
	})
	gs := googlegrpc.NewServer(
		googlegrpc.UnaryInterceptor(adapter.UnaryAuthInterceptor()),
		googlegrpc.StreamInterceptor(adapter.StreamAuthInterceptor()),
	)
	loomcyclepb.RegisterLoomcycleServer(gs, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	e := &env{t: t, st: st, ts: ts, prov: prov, grpc: loomcyclepb.NewLoomcycleClient(conn)}
	e.seedTeam(tenant)
	e.seedTeam("")        // the tenant an unauthenticated caller works in
	e.seedTeam("default") // and the single shared token's
	return e
}

// seedTeam writes and activates the team in one tenant, straight to the store:
// authoring is exercised separately, by the tests that are about it.
func (e *env) seedTeam(tenantID string) {
	e.t.Helper()
	def, err := teamgraph.Parse([]byte(soloTeam))
	if err != nil {
		e.t.Fatal(err)
	}
	row, err := e.st.TeamDefCreate(context.Background(), store.TeamDefRow{
		DefID: "tdf_" + tenantID + "_" + teamName, Name: teamName, TenantID: tenantID,
		Definition: json.RawMessage(soloTeam), ContentSHA256: teamgraph.Sign(teamName, def),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.TeamDefSetActive(context.Background(), tenantID, teamName, row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		e.t.Fatal(err)
	}
}

// mint stores a token for subject in tenant acme holding scopes, as the token
// substrate would, and returns the bearer a caller presents.
func (e *env) mint(subject string, scopes ...string) string {
	e.t.Helper()
	bearer := "lct_" + tenant + "_" + subject
	_, err := e.st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_" + subject, Name: subject, TenantID: tenant, Subject: subject,
		TokenHash:     auth.HashToken(pepper, bearer),
		AllowedScopes: scopes,
	})
	if err != nil {
		e.t.Fatalf("mint: %v", err)
	}
	return bearer
}

// walkRuns is how many walks of the team have a run row, in any tenant.
func (e *env) walkRuns() int {
	e.t.Helper()
	_, err := e.st.GetRunByAgentID(context.Background(), "team:"+teamName)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return 0
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return 1
}

func (e *env) do(path, bearer, body string, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, raw
}

// outcome is one call's result in terms every surface shares.
type outcome struct {
	// forbidden: the surface's own scope refusal — HTTP 403, gRPC
	// PermissionDenied, JSON-RPC -32001. Anything the tool itself answered,
	// success or failure, is not this.
	forbidden bool
	// atRoute: the refusal came from the route / RPC gate (or the MCP session
	// was not opened) rather than from the per-op check.
	atRoute bool
	// toolError: the tool was reached and answered with a failure.
	toolError bool
	// text is the tool's payload, or the refusal's message.
	text string
	// scopeHeader is HTTP's WWW-Authenticate on a refusal.
	scopeHeader string
}

// surface is one way in. call sends the same TeamDef input each way.
type surface struct {
	name string
	call func(e *env, bearer, input string) outcome
}

var surfaces = []surface{
	{"http", (*env).teamHTTP},
	{"grpc", (*env).teamGRPC},
	{"mcp", (*env).teamMCP},
}

func (e *env) teamHTTP(bearer, input string) outcome {
	resp, raw := e.do("/v1/_teamdef", bearer, input, nil)
	o := outcome{text: string(raw), scopeHeader: resp.Header.Get("WWW-Authenticate")}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		o.forbidden = true
		// The route gate answers in plain text; the per-op check in JSON.
		o.atRoute = !json.Valid(raw)
	case http.StatusUnprocessableEntity:
		o.toolError = true
	default:
		e.t.Fatalf("POST /v1/_teamdef %s = %d: %s", input, resp.StatusCode, raw)
	}
	return o
}

func (e *env) teamGRPC(bearer, input string) outcome {
	ctx := context.Background()
	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
	}
	resp, err := e.grpc.TeamDef(ctx, &loomcyclepb.SubstrateRequest{InputJson: []byte(input)})
	if err != nil {
		st := status.Convert(err)
		if st.Code() != codes.PermissionDenied {
			e.t.Fatalf("TeamDef RPC %s: %v", input, err)
		}
		return outcome{forbidden: true, text: st.Message(),
			atRoute: strings.Contains(st.Message(), auth.ScopeTenant)}
	}
	return outcome{toolError: resp.GetIsError(), text: string(resp.GetOutputJson())}
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *env) mcpFrame(bearer, sessionID, frame string) (*http.Response, []byte) {
	h := map[string]string{"Accept": "application/json, text/event-stream"}
	if sessionID != "" {
		h["Mcp-Session-Id"] = sessionID
	}
	return e.do("/v1/_mcp", bearer, frame, h)
}

// mcpRPC opens a session at the real /v1/_mcp as bearer and sends one request.
// A refusal at the route is returned as ok=false.
func (e *env) mcpRPC(bearer, method, params string) (reply rpcReply, ok bool) {
	e.t.Helper()
	resp, raw := e.mcpFrame(bearer, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != http.StatusOK {
		return rpcReply{}, false
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		e.t.Fatalf("initialize gave no session: %s", raw)
	}
	resp, raw = e.mcpFrame(bearer, sessionID, `{"jsonrpc":"2.0","id":2,"method":"`+method+`","params":`+params+`}`)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s = %d: %s", method, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		e.t.Fatalf("%s reply is not JSON-RPC: %s", method, raw)
	}
	return reply, true
}

func (e *env) teamMCP(bearer, input string) outcome {
	reply, ok := e.mcpRPC(bearer, "tools/call", `{"name":"teamdef","arguments":`+input+`}`)
	if !ok {
		return outcome{forbidden: true, atRoute: true}
	}
	if reply.Error != nil {
		if reply.Error.Code != -32001 {
			e.t.Fatalf("teamdef %s: JSON-RPC error %d %s", input, reply.Error.Code, reply.Error.Message)
		}
		return outcome{forbidden: true, text: reply.Error.Message}
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(reply.Result, &res); err != nil || len(res.Content) == 0 {
		e.t.Fatalf("tools/call result = %s", reply.Result)
	}
	return outcome{toolError: res.IsError, text: res.Content[0].Text}
}

// mcpLists reports whether bearer's MCP session is offered the teamdef tool.
func (e *env) mcpLists(bearer string) bool {
	e.t.Helper()
	reply, ok := e.mcpRPC(bearer, "tools/list", `{}`)
	if !ok {
		e.t.Fatal("the MCP session was refused")
	}
	return strings.Contains(string(reply.Result), `"name":"teamdef"`)
}
