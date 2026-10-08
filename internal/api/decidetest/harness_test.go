// Package decidetest holds the tests of a decision asked OUTSIDE a run, driven
// through the three real surfaces at once: POST /v1/_decide, the gRPC Decide
// RPC and the MCP `decision` tool, each behind its real authentication (a
// bearer resolved by the server, the route and RPC scope gates, the MCP
// per-tool gate) over one real HTTP server.
//
// It is a package of its own because it needs all three transports, and the
// transports' own test packages cannot import each other.
package decidetest

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"google.golang.org/genproto/googleapis/rpc/errdetails"
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
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Test fixtures, not secrets: the bearers and keys below exist only inside
// these tests' in-memory servers.
const (
	pepper      = "pep-decide"
	legacyToken = "legacy-decide-test"
	operatorKey = "operator-key-for-the-test"
	tenantKey   = "acme-own-key-for-the-test"
	keyName     = "OLLAMA_API_KEY"
)

const oneQuestion = `{"state":{"ticket":"charged twice"},"questions":{"urgent":{"type":"noul","instructions":"Does it need a reply within the hour?"}}}`

// modelDouble stands in for the decision model's endpoint. It records, per
// call, the model asked for and the Authorization header the call carried,
// which is what says WHOSE key paid. reply, when set, answers instead.
type modelDouble struct {
	mu      sync.Mutex
	auths   []string
	answers string // the "answers" object returned for every call
	reply   func(w http.ResponseWriter)
	url     string
}

func newModelDouble(t *testing.T) *modelDouble {
	t.Helper()
	d := &modelDouble{answers: `{"urgent":{"type":"noul","noul":0.938}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.auths = append(d.auths, r.Header.Get("Authorization"))
		reply, answers := d.reply, d.answers
		d.mu.Unlock()
		if reply != nil {
			reply(w)
			return
		}
		_, _ = io.WriteString(w, `{"model":"`+body.Model+`","answers":`+answers+`,"usage":{"input_tokens":1116,"output_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)
	d.url = srv.URL
	return d
}

func (d *modelDouble) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.auths)
}

// paidWith is the key the last call carried.
func (d *modelDouble) paidWith(t *testing.T) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.auths) == 0 {
		t.Fatal("the decision model was never called")
	}
	return strings.TrimPrefix(d.auths[len(d.auths)-1], "Bearer ")
}

type noProviders struct{}

func (noProviders) Get(string) (providers.Provider, error) {
	return nil, errors.New("no chat provider in this test")
}

type envOptions struct {
	// unconfigured builds a server with no decision models.
	unconfigured bool
	// gate is the deployment's operator-key restriction switch.
	gate bool
	// open builds a server with no authentication at all.
	open bool
	// priced adds a pricing entry for the decision model.
	priced bool
	// timeout bounds one model call (0 = the driver's default).
	timeout time.Duration
	// maxRequestBytes caps a request body (0 = the server's default).
	maxRequestBytes int64
}

type env struct {
	t     *testing.T
	srv   *lchttp.Server
	st    store.Store
	ts    *httptest.Server
	model *modelDouble
	creds *credential.Engine
	grpc  loomcyclepb.LoomcycleClient
}

// newEnv builds one real server reachable three ways. Everything a call
// crosses is the production code: the auth middleware and the scope tables,
// the MCP handler mounted at /v1/_mcp, the gRPC interceptors, the server's
// run-less decision path, the Decision tool and the decision driver. Only the
// model's endpoint is a double, and the credential resolver is this file's
// copy of the closure main wires (main's is not importable).
func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "none", Model: "none"},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 2000},
	}
	cfg.Env.OperatorTokenPepper = pepper
	cfg.Env.OperatorKeyRestriction = o.gate
	cfg.Env.MaxRequestBytes = o.maxRequestBytes
	if !o.open {
		cfg.Env.AuthToken = legacyToken
	}
	if o.priced {
		cfg.Pricing = config.PricingConfig{Currency: "USD", Models: map[string]config.ModelPrice{
			"ollama/nimble": {Input: 2, Output: 10},
		}}
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "decide.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	model := newModelDouble(t)
	var all []tools.Tool
	var svc *decision.Service
	if !o.unconfigured {
		drv, err := decision.New("ollama", decision.Options{
			ProviderID: "ollama", BaseURL: model.url, APIKey: operatorKey, KeyEnvName: keyName, Timeout: o.timeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		svc, err = decision.NewService("decide", []decision.ModelSpec{
			{Name: "decide", Provider: "ollama", Model: "nimble", Driver: drv},
			{Name: "deep", Provider: "ollama", Model: "clef", Driver: drv},
		})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, &builtin.Decision{Service: svc})
	}
	srv := lchttp.New(cfg, noProviders{}, all, concurrency.New(4, 4, 2*time.Second), st)
	if svc != nil {
		svc.SetOnUsage(srv.RecordRunSideCallUsage) // as main wires it
	}

	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	creds := credential.NewEngine(st, sealer)
	srv.SetCredentialResolver(func(ctx context.Context, name string) (providers.CredentialResolution, bool) {
		ri := tools.RunIdentity(ctx)
		res, found, err := creds.Resolve(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, name)
		if err != nil || !found {
			return providers.CredentialResolution{}, false
		}
		return providers.CredentialResolution{Value: res.Value, Scope: res.Scope, ScopeID: res.ScopeID}, true
	})

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

	return &env{t: t, srv: srv, st: st, ts: ts, model: model, creds: creds, grpc: loomcyclepb.NewLoomcycleClient(conn)}
}

// mint stores a token for (tenant, subject) holding scopes, as the token
// substrate would, and returns the bearer a caller presents.
func (e *env) mint(tenant, subject string, scopes ...string) string {
	e.t.Helper()
	bearer := "lct_" + tenant + "_" + subject
	_, err := e.st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_" + tenant + "_" + subject, Name: tenant + "-" + subject,
		TenantID: tenant, Subject: subject,
		TokenHash:     auth.HashToken(pepper, bearer),
		AllowedScopes: scopes,
	})
	if err != nil {
		e.t.Fatalf("mint: %v", err)
	}
	return bearer
}

// storeTenantKey stores tenant's own key for the decision provider.
func (e *env) storeTenantKey(tenant, value string) {
	e.t.Helper()
	e.storeKey(credential.Identity{TenantID: tenant, Scope: "tenant", Name: keyName}, value)
}

// storeKey stores a key for the decision provider under any scope.
func (e *env) storeKey(id credential.Identity, value string) {
	e.t.Helper()
	if _, err := e.creds.PutInline(context.Background(), id, value, nil); err != nil {
		e.t.Fatalf("store a key: %v", err)
	}
}

// setHardLimit puts a hard token budget on (tenant, user) through the real
// PUT /v1/_limits, as the operator.
func (e *env) setHardLimit(tenant, user string, hard int64) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"tenant_id": tenant, "scope": "user", "scope_id": user, "hard_limit": hard})
	resp, raw := e.do(http.MethodPut, "/v1/_limits", legacyToken, string(body), nil)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("PUT /v1/_limits = %d: %s", resp.StatusCode, raw)
	}
}

// offRunRows are the ledger's rows with no run: the calls made outside one.
func (e *env) offRunRows() []store.TokenUsageRow {
	e.t.Helper()
	rows, err := e.st.TokenUsageForRun(context.Background(), "")
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) do(method, path, bearer, body string, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
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
	// ok: the model's answers came back.
	ok bool
	// answers are each question's answer bytes, as the surface delivered them.
	answers map[string]string
	// text is the whole success payload where the surface has one (the HTTP
	// body, the MCP result text); "" for gRPC, which is typed.
	text string
	// code is the refusal: a decision code, "token_limit_exceeded", or
	// "insufficient_scope" for a caller the surface's scope gate turned away.
	code string
	// status is the surface's own status for it: an HTTP status, a gRPC code,
	// "isError" or "jsonrpc -32001" for MCP.
	status string
	// raw is the surface's raw reply, for failure messages.
	raw string
}

// surface is one way in. decide sends the same JSON call each way.
type surface struct {
	name   string
	decide func(e *env, bearer, call string) outcome
}

var surfaces = []surface{
	{"http", (*env).decideHTTP},
	{"grpc", (*env).decideGRPC},
	{"mcp", (*env).decideMCP},
}

func answersOf(payload []byte) map[string]string {
	var out struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	_ = json.Unmarshal(payload, &out)
	m := make(map[string]string, len(out.Answers))
	for k, v := range out.Answers {
		m[k] = string(v)
	}
	return m
}

func (e *env) decideHTTP(bearer, call string) outcome {
	resp, raw := e.do(http.MethodPost, "/v1/_decide", bearer, call, nil)
	o := outcome{status: resp.Status[:3], raw: string(raw)}
	if resp.StatusCode == http.StatusOK {
		o.ok, o.text, o.answers = true, string(raw), answersOf(raw)
		return o
	}
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient scope") {
		o.code = "insufficient_scope"
		return o
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &body)
	o.code = body.Code
	return o
}

func (e *env) grpcCtx(bearer string) context.Context {
	ctx := context.Background()
	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
	}
	return ctx
}

func (e *env) decideGRPC(bearer, call string) outcome {
	var in struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(call), &in); err != nil {
		e.t.Fatalf("the test's call is not JSON: %v", err)
	}
	req := &loomcyclepb.DecideRequest{Model: in.Model, StateJson: in.State, Questions: map[string]*loomcyclepb.DecisionQuestion{}}
	for name, q := range in.Questions {
		req.Questions[name] = &loomcyclepb.DecisionQuestion{Type: q.Type, Instructions: q.Instructions, CriteriaJson: q.Criteria}
	}
	resp, err := e.grpc.Decide(e.grpcCtx(bearer), req)
	if err == nil {
		o := outcome{ok: true, status: codes.OK.String(), answers: map[string]string{}, raw: resp.String()}
		for k, v := range resp.GetAnswers() {
			o.answers[k] = string(v)
		}
		return o
	}
	st := status.Convert(err)
	o := outcome{status: st.Code().String(), raw: st.Message()}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			o.code = info.GetReason()
		}
	}
	if o.code == "" && st.Code() == codes.PermissionDenied && strings.Contains(st.Message(), "insufficient scope") {
		o.code = "insufficient_scope"
	}
	return o
}

// mcpCall opens a session at the real /v1/_mcp as bearer and calls one tool.
// A refusal at the route is returned as (nil, the HTTP response).
func (e *env) mcpFrame(bearer, sessionID, frame string) (*http.Response, []byte) {
	h := map[string]string{"Accept": "application/json, text/event-stream"}
	if sessionID != "" {
		h["Mcp-Session-Id"] = sessionID
	}
	return e.do(http.MethodPost, "/v1/_mcp", bearer, frame, h)
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *env) mcpSession(bearer string) (sessionID string, refused *http.Response) {
	e.t.Helper()
	resp, raw := e.mcpFrame(bearer, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != http.StatusOK {
		return "", resp
	}
	sessionID = resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		e.t.Fatalf("initialize gave no session: %s", raw)
	}
	return sessionID, nil
}

func (e *env) mcpRPC(bearer, sessionID, method, params string) rpcReply {
	e.t.Helper()
	resp, raw := e.mcpFrame(bearer, sessionID, `{"jsonrpc":"2.0","id":2,"method":"`+method+`","params":`+params+`}`)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s = %d: %s", method, resp.StatusCode, raw)
	}
	var reply rpcReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		e.t.Fatalf("%s reply is not JSON-RPC: %s", method, raw)
	}
	return reply
}

func (e *env) decideMCP(bearer, call string) outcome {
	sessionID, refused := e.mcpSession(bearer)
	if refused != nil {
		return outcome{status: refused.Status[:3], code: "insufficient_scope"}
	}
	reply := e.mcpRPC(bearer, sessionID, "tools/call", `{"name":"decision","arguments":`+call+`}`)
	if reply.Error != nil {
		o := outcome{status: "jsonrpc " + jsonInt(reply.Error.Code), raw: reply.Error.Message}
		if reply.Error.Code == -32001 {
			o.code = "insufficient_scope"
		}
		return o
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(reply.Result, &res); err != nil || len(res.Content) != 1 {
		e.t.Fatalf("tools/call result = %s", reply.Result)
	}
	text := res.Content[0].Text
	if !res.IsError {
		return outcome{ok: true, status: "result", text: text, answers: answersOf([]byte(text)), raw: text}
	}
	o := outcome{status: "isError", raw: text}
	switch {
	case strings.Contains(text, "token_limit_exceeded"):
		o.code = "token_limit_exceeded"
	default:
		o.code = builtin.DecisionFailureCode(text)
	}
	return o
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// mcpToolListed reports whether bearer's MCP session lists the decision tool.
func (e *env) mcpToolListed(bearer string) bool {
	e.t.Helper()
	sessionID, refused := e.mcpSession(bearer)
	if refused != nil {
		e.t.Fatalf("the MCP session was refused: %s", refused.Status)
	}
	reply := e.mcpRPC(bearer, sessionID, "tools/list", `{}`)
	return bytes.Contains(reply.Result, []byte(`"name":"decision"`))
}
