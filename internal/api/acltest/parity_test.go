// Package acltest holds the transport parity test of the access-control
// management surface (RFC EZ §2): the same (principal, op, input) driven
// through POST/GET on the HTTP routes, the gRPC GroupDef / AclDef /
// AclBinding / Authz RPCs and the MCP meta-tools of the same names, each
// behind its real authentication, over one real server, must get one answer.
//
// It is a package of its own because it needs all three transports, and the
// transports' own test packages cannot import each other.
package acltest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	lcgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Test fixtures, not secrets: these bearers exist only inside this test's
// in-memory server.
const (
	pepper   = "pep-acl-parity"
	legacy   = "legacy-acl-parity"
	operator = "lct_acme_ops"
	member   = "lct_acme_alice"
	delegate = "lct_acme_delegate"
	readless = "lct_acme_readless" // a tenant operator denied reading groups and lists
)

type noProviders struct{}

func (noProviders) Get(string) (providers.Provider, error) { return nil, io.EOF }

type env struct {
	t    *testing.T
	ts   *httptest.Server
	grpc loomcyclepb.LoomcycleClient
}

func newEnv(t *testing.T, mode authz.Mode) *env {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "none", Model: "none"},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.OperatorTokenPepper = pepper
	cfg.Env.AuthToken = legacy
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "acl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, tok := range []struct {
		bearer, subject string
		scopes          []string
	}{
		{operator, "ops", []string{auth.ScopeTenant}},
		{member, "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}},
		{delegate, "delegate", []string{auth.ScopeTenant}},
		{readless, "readless", []string{auth.ScopeTenant}},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: "def_" + tok.subject, Name: "acme-" + tok.subject, TenantID: "acme", Subject: tok.subject,
			TokenHash: auth.HashToken(pepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv := lchttp.New(cfg, noProviders{}, []tools.Tool{}, concurrency.New(4, 4, 2*time.Second), st)
	srv.SetACLResolver(aclres.New(st, mode, 0))
	srv.SetMCPHTTPHandler(lcmcp.NewHTTPHandler(lcmcp.Config{Connector: srv, Runner: srv, Store: st, Logf: func(string, ...any) {}}))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	adapter := lcgrpc.New(lcgrpc.Config{
		Store: st, CancelReg: cancel.NewRegistry(), Connector: srv, Runner: srv,
		Limits: srv.LimitsTracker(), AuthToken: cfg.Env.AuthToken,
		PrincipalResolver: srv.ResolvePrincipal, AuthConfigured: srv.AuthConfigured,
		ACLMode: srv.ACLResolver().Mode(),
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
	return &env{t: t, ts: ts, grpc: loomcyclepb.NewLoomcycleClient(conn)}
}

func (e *env) do(method, path, bearer, body string, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
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

// outcome is one call's answer in terms every transport shares.
type outcome struct {
	allowed bool
	// code is the refusal: the body's code, or "insufficient_scope" for a
	// caller the transport's own scope gate turned away.
	code string
	// body is the success payload, decoded, for comparing answers.
	body any
	raw  string
}

func decoded(raw []byte) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

func codeOf(raw []byte) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Code
}

// call is one twin op: the family, the twin's input, and the HTTP route the
// same op is on.
type call struct {
	family, input      string
	method, path, body string
	grpc               func(loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error)
}

func (e *env) viaHTTP(bearer string, c call) outcome {
	resp, raw := e.do(c.method, c.path, bearer, c.body, nil)
	if resp.StatusCode < 300 {
		return outcome{allowed: true, body: decoded(raw), raw: string(raw)}
	}
	return outcome{code: codeOf(raw), raw: string(raw)}
}

func (e *env) viaGRPC(bearer string, c call) outcome {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := c.grpc(e.grpc)(ctx, &loomcyclepb.SubstrateRequest{InputJson: []byte(c.input)})
	if err != nil {
		st := status.Convert(err)
		o := outcome{raw: st.Code().String() + ": " + st.Message()}
		switch {
		case strings.Contains(st.Message(), "insufficient scope"):
			o.code = "insufficient_scope"
		case strings.HasPrefix(st.Message(), "acl_denied"):
			o.code = "acl_denied"
		}
		return o
	}
	if resp.GetIsError() {
		return outcome{code: codeOf(resp.GetOutputJson()), raw: string(resp.GetOutputJson())}
	}
	return outcome{allowed: true, body: decoded(resp.GetOutputJson()), raw: string(resp.GetOutputJson())}
}

func (e *env) viaMCP(bearer string, c call) outcome {
	e.t.Helper()
	h := map[string]string{"Accept": "application/json, text/event-stream"}
	resp, raw := e.do(http.MethodPost, "/v1/_mcp", bearer,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, h)
	if resp.StatusCode != http.StatusOK {
		return outcome{code: codeOf(raw), raw: string(raw)}
	}
	h["Mcp-Session-Id"] = resp.Header.Get("Mcp-Session-Id")
	resp, raw = e.do(http.MethodPost, "/v1/_mcp", bearer,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+c.family+`","arguments":`+c.input+`}}`, h)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("MCP tools/call = %d: %s", resp.StatusCode, raw)
	}
	var reply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		e.t.Fatalf("MCP reply is not JSON-RPC: %s", raw)
	}
	if reply.Error != nil {
		o := outcome{raw: reply.Error.Message}
		if reply.Error.Code == -32001 { // the per-tool gate: the tool is not this principal's
			o.code = "insufficient_scope"
		}
		return o
	}
	if len(reply.Result.Content) != 1 {
		e.t.Fatalf("MCP tools/call result = %s", raw)
	}
	text := []byte(reply.Result.Content[0].Text)
	if reply.Result.IsError {
		return outcome{code: codeOf(text), raw: string(text)}
	}
	return outcome{allowed: true, body: decoded(text), raw: string(text)}
}

func groupDef(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.GroupDef
}
func aclDef(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.AclDef
}
func aclBinding(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.AclBinding
}
func authzRPC(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.Authz
}

// setup binds the delegate (a tenant operator) a list denying it authoring
// lists and bindings, over HTTP as the operator.
func (e *env) setup() {
	e.t.Helper()
	deny := `[{"effect":"deny","actions":["author"],"resources":["def:acl/*"]}]`
	noRead := `[{"effect":"deny","actions":["read"],"resources":["principal:group:*","def:acl/*"]}]`
	for _, s := range []struct{ path, body string }{
		{"/v1/_acldef", `{"op":"create","name":"no-acl-authoring","rules":` + deny + `}`},
		{"/v1/_aclbinding", `{"acl":"no-acl-authoring","subject_kind":"user","subject_id":"delegate"}`},
		{"/v1/_acldef", `{"op":"create","name":"no-read","rules":` + noRead + `}`},
		{"/v1/_aclbinding", `{"acl":"no-read","subject_kind":"user","subject_id":"readless"}`},
	} {
		if resp, raw := e.do(http.MethodPost, s.path, operator, s.body, nil); resp.StatusCode != http.StatusCreated {
			e.t.Fatalf("setup %s: %d %s", s.path, resp.StatusCode, raw)
		}
	}
}

func TestACLManagement_OneAnswerOnEveryTransport(t *testing.T) {
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	ask := `"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}`
	cases := []struct {
		name    string
		bearer  string
		call    call
		allowed bool
		code    string // the refusal every transport must give
		compare bool   // the success payloads must be equal
	}{
		{"a member is refused group management", member,
			call{"groupdef", `{"op":"create","name":"support"}`, "POST", "/v1/_groups", `{"name":"support"}`, groupDef},
			false, "insufficient_scope", false},
		{"a member is refused list management", member,
			call{"acldef", `{"op":"list"}`, "POST", "/v1/_acldef", `{"op":"list"}`, aclDef},
			false, "insufficient_scope", false},
		{"a member is refused binding management", member,
			call{"aclbinding", `{"op":"list"}`, "GET", "/v1/_aclbinding", "", aclBinding},
			false, "insufficient_scope", false},
		{"an operator manages groups", operator,
			call{"groupdef", `{"op":"create","name":"support"}`, "POST", "/v1/_groups", `{"name":"support"}`, groupDef},
			true, "", false},
		{"an operator lists the lists", operator,
			call{"acldef", `{"op":"names"}`, "GET", "/v1/_acldef/names", "", aclDef},
			true, "", true},
		{"a delegate denied authoring is refused a list create", delegate,
			call{"acldef", `{"op":"create","name":"wider","rules":` + rules + `}`, "POST", "/v1/_acldef", `{"op":"create","name":"wider","rules":` + rules + `}`, aclDef},
			false, "acl_denied", false},
		{"a delegate denied authoring is refused a binding", delegate,
			call{"aclbinding", `{"op":"create","acl":"x","subject_kind":"user","subject_id":"bob"}`, "POST", "/v1/_aclbinding", `{"acl":"x","subject_kind":"user","subject_id":"bob"}`, aclBinding},
			false, "acl_denied", false},
		{"a delegate denied authoring still reads lists", delegate,
			call{"acldef", `{"op":"get","name":"no-acl-authoring"}`, "POST", "/v1/_acldef", `{"op":"get","name":"no-acl-authoring"}`, aclDef},
			true, "", true},
		{"a member asks about itself", member,
			call{"authz", `{"op":"evaluation",` + ask + `}`, "POST", "/v1/_authz/evaluation", `{` + ask + `}`, authzRPC},
			true, "", true},
		{"a member explains its own decision", member,
			call{"authz", `{"op":"explain",` + ask + `}`, "POST", "/v1/_authz/explain", `{` + ask + `}`, authzRPC},
			true, "", true},
		{"a member may not ask about another subject", member,
			call{"authz", `{"op":"evaluation","subject":{"id":"ops"},` + ask + `}`, "POST", "/v1/_authz/evaluation", `{"subject":{"id":"ops"},` + ask + `}`, authzRPC},
			false, "authz_subject", false},
		// Write without read: every transport asks the family's read first
		// (the RPC and the route), then the op's own verb, as acldef does.
		{"manage without read is refused a group write", readless,
			call{"groupdef", `{"op":"create","name":"by-readless"}`, "POST", "/v1/_groups", `{"name":"by-readless"}`, groupDef},
			false, "acl_denied", false},
		{"author without read is refused a binding", readless,
			call{"aclbinding", `{"op":"create","acl":"no-acl-authoring","subject_kind":"user","subject_id":"bob"}`, "POST", "/v1/_aclbinding", `{"acl":"no-acl-authoring","subject_kind":"user","subject_id":"bob"}`, aclBinding},
			false, "acl_denied", false},
		{"an operator asks about a member", operator,
			call{"authz", `{"op":"evaluation","subject":{"id":"alice"},` + ask + `}`, "POST", "/v1/_authz/evaluation", `{"subject":{"id":"alice"},` + ask + `}`, authzRPC},
			true, "", true},
	}
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnv(t, mode)
			e.setup()
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					got := map[string]outcome{
						"http": e.viaHTTP(c.bearer, c.call),
						"grpc": e.viaGRPC(c.bearer, c.call),
						"mcp":  e.viaMCP(c.bearer, c.call),
					}
					for transport, o := range got {
						if o.allowed != c.allowed || (!c.allowed && o.code != c.code) {
							t.Errorf("%s: allowed=%v code=%q (%s), want allowed=%v code=%q", transport, o.allowed, o.code, o.raw, c.allowed, c.code)
						}
					}
					if c.compare && c.allowed {
						if !reflect.DeepEqual(got["http"].body, got["grpc"].body) || !reflect.DeepEqual(got["http"].body, got["mcp"].body) {
							t.Errorf("the transports answered differently:\n http %s\n grpc %s\n mcp  %s", got["http"].raw, got["grpc"].raw, got["mcp"].raw)
						}
					}
				})
			}
		})
	}
}
