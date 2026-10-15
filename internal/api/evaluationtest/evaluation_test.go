// Package evaluationtest drives the Evaluation tool outside a run through the
// three real surfaces at once — POST /v1/_evaluation, the Evaluation RPC and
// the MCP `evaluation` tool — each behind its real authentication: a bearer
// resolved by the server, the route and RPC scope gates, the MCP per-tool gate.
//
// It is a package of its own because it needs all three transports, and the
// transports' own test packages cannot import each other.
package evaluationtest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Test fixtures, not secrets: the bearers below exist only inside these tests'
// in-memory servers.
const (
	pepper      = "pep-evaluation"
	legacyToken = "legacy-evaluation-test"
)

// The callers. Each is a token class the grant tells apart.
const (
	callerAdmin      = "admin"      // the legacy token: substrate:admin
	callerTenant     = "tenant"     // substrate:tenant in acme
	callerIsolated   = "isolated"   // substrate:user in acme (subject ivy)
	callerMember     = "member"     // runs:create + runs:read in acme, not isolated
	callerNoRunsRead = "no-runs"    // a member token holding only channel scopes
	callerRestricted = "restricted" // substrate:tenant held to the run plane
)

var callers = []string{callerAdmin, callerTenant, callerIsolated, callerMember, callerNoRunsRead, callerRestricted}

type noProviders struct{}

func (noProviders) Get(string) (providers.Provider, error) {
	return nil, errMissing
}

type missingErr struct{}

func (missingErr) Error() string { return "no chat provider in this test" }

var errMissing = missingErr{}

// fixture is what one env's store holds. Run ids are minted by the store, so
// they differ between envs; every outcome text is normalised against them.
type fixture struct {
	runIvy, runBob, runGlobex string
}

type env struct {
	t       *testing.T
	st      store.Store
	ts      *httptest.Server
	grpc    loomcyclepb.LoomcycleClient
	bearers map[string]string
	fx      fixture
}

// newEnv builds one real server reachable three ways, with acme and globex
// data in it. Everything a call crosses is production code: the auth
// middleware and the scope tables, the MCP handler at /v1/_mcp, the gRPC
// interceptors and the Evaluation tool.
func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "none", Model: "none"},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.OperatorTokenPepper = pepper
	cfg.Env.AuthToken = legacyToken
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "evaluation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := lchttp.New(cfg, noProviders{}, []tools.Tool{&builtin.Evaluation{Store: st}}, concurrency.New(4, 4, 2*time.Second), st)
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

	e := &env{t: t, st: st, ts: ts, grpc: loomcyclepb.NewLoomcycleClient(conn), bearers: map[string]string{callerAdmin: legacyToken}}
	e.bearers[callerTenant] = e.mint("acme", "olga", "", auth.ScopeTenant)
	e.bearers[callerIsolated] = e.mint("acme", "ivy", "", auth.ScopeUser)
	e.bearers[callerMember] = e.mint("acme", "mel", "", auth.ScopeRunsCreate, auth.ScopeRunsRead)
	e.bearers[callerNoRunsRead] = e.mint("acme", "nora", "", auth.ScopeChannelRead, auth.ScopeChannelPublish)
	e.bearers[callerRestricted] = e.mint("acme", "rex", `{"agents":["worker"]}`, auth.ScopeTenant)
	e.seed()
	return e
}

// mint stores a token for (tenant, subject) as the token substrate would and
// returns the bearer a caller presents.
func (e *env) mint(tenant, subject, restrictions string, scopes ...string) string {
	e.t.Helper()
	bearer := "lct_" + tenant + "_" + subject
	if _, err := e.st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "tok_" + subject, Name: tenant + "-" + subject, TenantID: tenant, Subject: subject,
		TokenHash: auth.HashToken(pepper, bearer), AllowedScopes: scopes, RestrictionsJSON: restrictions,
	}); err != nil {
		e.t.Fatalf("mint: %v", err)
	}
	return bearer
}

// seed puts one def and two runs in acme (ivy's and bob's) and one of each in
// globex, and scores bob's run and globex's run once each.
func (e *env) seed() {
	e.t.Helper()
	ctx := context.Background()
	for _, d := range []struct{ id, tenant string }{{"def_acme", "acme"}, {"def_globex", "globex"}} {
		if _, err := e.st.AgentDefCreate(ctx, store.AgentDefRow{
			DefID: d.id, Name: "worker", Version: 1, Definition: json.RawMessage(`{"model":"none"}`), TenantID: d.tenant,
		}); err != nil {
			e.t.Fatalf("AgentDefCreate %s: %v", d.id, err)
		}
	}
	run := func(tenant, user, def string) string {
		sess, err := e.st.CreateSession(ctx, tenant, "worker", user)
		if err != nil {
			e.t.Fatal(err)
		}
		r, err := e.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_worker_" + user, UserID: user, TenantID: tenant, AgentDefID: def})
		if err != nil {
			e.t.Fatal(err)
		}
		return r.ID
	}
	e.fx = fixture{
		runIvy:    run("acme", "ivy", "def_acme"),
		runBob:    run("acme", "bob", "def_acme"),
		runGlobex: run("globex", "gus", "def_globex"),
	}
	for _, ev := range []struct{ id, run, def string }{
		{"eval_acme", e.fx.runBob, "def_acme"},
		{"eval_globex", e.fx.runGlobex, "def_globex"},
	} {
		if _, err := e.st.EvaluationSubmit(ctx, store.EvaluationRow{
			EvalID: ev.id, RunID: ev.run, DefID: ev.def, Score: 0.9, Rationale: "seeded", EmitterRole: "external",
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// op is one call, written against the fixture so it can be rebuilt per env.
type op struct {
	name string
	body func(fixture) string
}

// The reads come first so that the submits, which add rows, cannot change
// what a read returns; every env runs the same calls in the same order.
var ops = []op{
	{"get own tenant", func(fixture) string { return `{"op":"get","eval_id":"eval_acme"}` }},
	{"get other tenant", func(fixture) string { return `{"op":"get","eval_id":"eval_globex"}` }},
	{"get missing", func(fixture) string { return `{"op":"get","eval_id":"eval_nope"}` }},
	{"list_for_run", func(f fixture) string { return `{"op":"list_for_run","run_id":"` + f.runBob + `"}` }},
	{"list_for_def", func(fixture) string { return `{"op":"list_for_def","def_id":"def_acme"}` }},
	{"aggregate", func(fixture) string { return `{"op":"aggregate","def_id":"def_acme"}` }},
	{"submit own run", func(f fixture) string { return `{"op":"submit","run_id":"` + f.runIvy + `","score":0.5}` }},
	{"submit another run", func(f fixture) string { return `{"op":"submit","run_id":"` + f.runBob + `","score":0.5}` }},
	{"submit other tenant", func(f fixture) string { return `{"op":"submit","run_id":"` + f.runGlobex + `","score":0.5}` }},
	{"submit missing", func(fixture) string { return `{"op":"submit","run_id":"r_nope","score":0.5}` }},
}

// outcome is a call's result in the terms every surface shares.
type outcome struct {
	// kind: "ok" (the tool answered), "refused" (the tool refused) or
	// "denied" (a scope gate turned the caller away before the tool).
	kind string
	// text is the tool's refusal text, normalised; "" otherwise.
	text string
	// rows is how many evaluations a list returned, -1 when not a list.
	rows int
	raw  string
}

func (o outcome) String() string {
	if o.kind == "refused" {
		return "refused: " + o.text
	}
	if o.kind == "ok" && o.rows >= 0 {
		return "ok (" + jsonInt(o.rows) + " rows)"
	}
	return o.kind
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// normalise replaces the env's run ids so the same refusal reads the same on
// every env.
func (e *env) normalise(text string) string {
	return strings.NewReplacer(e.fx.runIvy, "<run-ivy>", e.fx.runBob, "<run-bob>", e.fx.runGlobex, "<run-globex>").Replace(text)
}

func (e *env) okOutcome(payload string) outcome {
	o := outcome{kind: "ok", rows: -1, raw: payload}
	var body struct {
		Evaluations *[]json.RawMessage `json:"evaluations"`
	}
	if json.Unmarshal([]byte(payload), &body) == nil && body.Evaluations != nil {
		o.rows = len(*body.Evaluations)
	}
	return o
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

func (e *env) viaHTTP(bearer, call string) outcome {
	resp, raw := e.do(http.MethodPost, "/v1/_evaluation", bearer, call, nil)
	switch resp.StatusCode {
	case http.StatusOK:
		return e.okOutcome(string(raw))
	case http.StatusUnprocessableEntity:
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Code != "tool_refused" {
			e.t.Fatalf("422 body = %s", raw)
		}
		return outcome{kind: "refused", text: e.normalise(body.Error), rows: -1, raw: string(raw)}
	case http.StatusForbidden:
		return outcome{kind: "denied", rows: -1, raw: string(raw)}
	}
	e.t.Fatalf("POST /v1/_evaluation = %d: %s", resp.StatusCode, raw)
	return outcome{}
}

func (e *env) viaGRPC(bearer, call string) outcome {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.Evaluation(ctx, &loomcyclepb.SubstrateRequest{InputJson: []byte(call)})
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			return outcome{kind: "denied", rows: -1, raw: err.Error()}
		}
		e.t.Fatalf("Evaluation RPC: %v", err)
	}
	if resp.GetIsError() {
		return outcome{kind: "refused", text: e.normalise(string(resp.GetOutputJson())), rows: -1, raw: string(resp.GetOutputJson())}
	}
	return e.okOutcome(string(resp.GetOutputJson()))
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
	return e.do(http.MethodPost, "/v1/_mcp", bearer, frame, h)
}

func (e *env) viaMCP(bearer, call string) outcome {
	resp, raw := e.mcpFrame(bearer, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode == http.StatusForbidden {
		return outcome{kind: "denied", rows: -1, raw: string(raw)}
	}
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("initialize = %d: %s", resp.StatusCode, raw)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	resp, raw = e.mcpFrame(bearer, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"evaluation","arguments":`+call+`}}`)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("tools/call = %d: %s", resp.StatusCode, raw)
	}
	var reply rpcReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		e.t.Fatalf("tools/call reply is not JSON-RPC: %s", raw)
	}
	if reply.Error != nil {
		if reply.Error.Code == -32001 {
			return outcome{kind: "denied", rows: -1, raw: reply.Error.Message}
		}
		e.t.Fatalf("tools/call error: %+v", reply.Error)
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
	if res.IsError {
		return outcome{kind: "refused", text: e.normalise(res.Content[0].Text), rows: -1, raw: res.Content[0].Text}
	}
	return e.okOutcome(res.Content[0].Text)
}

type surface struct {
	name string
	call func(e *env, bearer, call string) outcome
}

var surfaces = []surface{
	{"http", (*env).viaHTTP},
	{"grpc", (*env).viaGRPC},
	{"mcp", (*env).viaMCP},
}

// matrix runs every caller × op through one surface, on a fresh env so the
// submits one surface makes cannot show up in another's reads.
func matrix(t *testing.T, s surface) map[string]map[string]outcome {
	t.Helper()
	e := newEnv(t)
	out := map[string]map[string]outcome{}
	for _, c := range callers {
		out[c] = map[string]outcome{}
		for _, o := range ops {
			out[c][o.name] = s.call(e, e.bearers[c], o.body(e.fx))
		}
	}
	return out
}

// TestEvaluation_EveryTransportGivesEveryCallerTheSameAnswer is the drift
// guard across transports: the same token asking the same thing gets the same
// answer over HTTP, gRPC and MCP — the same ops allowed, the same refusal
// text, the same rows. It fails when one transport's gate or grant moves
// without the others (MCP once gave a tenant operator submit_self while HTTP
// and gRPC would have given it everything).
func TestEvaluation_EveryTransportGivesEveryCallerTheSameAnswer(t *testing.T) {
	results := map[string]map[string]map[string]outcome{}
	for _, s := range surfaces {
		results[s.name] = matrix(t, s)
	}
	base := surfaces[0].name
	for _, s := range surfaces[1:] {
		for _, c := range callers {
			for _, o := range ops {
				want, got := results[base][c][o.name], results[s.name][c][o.name]
				if want.kind != got.kind || want.text != got.text || want.rows != got.rows {
					t.Errorf("%s / %s: %s = %v, %s = %v\n  %s raw: %s\n  %s raw: %s",
						c, o.name, base, want, s.name, got, base, want.raw, s.name, got.raw)
				}
			}
		}
	}
}

// TestEvaluation_TheGrantFollowsTheToken pins what each token class may do, on
// every transport: a tenant operator every op inside its own tenant, admin
// every op in every tenant, an isolated user or a member submit_self only,
// and a token without runs:read or held to the run plane nothing at all.
func TestEvaluation_TheGrantFollowsTheToken(t *testing.T) {
	// "ok", "denied", or a refusal that must contain the text after "refused:".
	const (
		ok       = "ok"
		denied   = "denied"
		notFound = "refused:not found"
		noRead   = "refused:requires evaluation_scopes containing \"read_any\""
		noSubmit = "refused:emitter_role=\"unrelated\" not permitted"
	)
	every := map[string]string{
		"get own tenant": ok, "get missing": notFound, "list_for_run": ok, "list_for_def": ok, "aggregate": ok,
		"submit own run": ok, "submit another run": ok, "submit missing": notFound,
	}
	tenantOnly := map[string]string{"get other tenant": notFound, "submit other tenant": notFound}
	crossTenant := map[string]string{"get other tenant": ok, "submit other tenant": ok}
	selfOnly := map[string]string{
		"get own tenant": noRead, "get other tenant": noRead, "get missing": noRead,
		"list_for_run": noRead, "list_for_def": noRead, "aggregate": noRead,
		"submit missing": notFound, "submit other tenant": notFound,
	}
	nothing := map[string]string{}
	for _, o := range ops {
		nothing[o.name] = denied
	}
	merge := func(ms ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	want := map[string]map[string]string{
		callerAdmin:  merge(every, crossTenant),
		callerTenant: merge(every, tenantOnly),
		// An isolated user sees only its own runs: bob's run reads as missing.
		// Its own run is visible, but submit_self is about an AGENT scoring its
		// own run, and a token is not the run's agent, so it is refused.
		callerIsolated:   merge(selfOnly, map[string]string{"submit own run": noSubmit, "submit another run": notFound}),
		callerMember:     merge(selfOnly, map[string]string{"submit own run": noSubmit, "submit another run": noSubmit}),
		callerNoRunsRead: nothing,
		callerRestricted: nothing,
	}
	for _, s := range surfaces {
		got := matrix(t, s)
		for _, c := range callers {
			for _, o := range ops {
				w, g := want[c][o.name], got[c][o.name]
				var match bool
				switch {
				case w == ok || w == denied:
					match = g.kind == w
				case strings.HasPrefix(w, "refused:"):
					match = g.kind == "refused" && strings.Contains(g.text, strings.TrimPrefix(w, "refused:"))
				default:
					t.Fatalf("no expectation for %s / %s", c, o.name)
				}
				if !match {
					t.Errorf("%s: %s / %s = %v, want %s (raw %s)", s.name, c, o.name, g, w, g.raw)
				}
			}
		}
	}
}

// TestEvaluation_AnotherTenantsIDReadsLikeAMissingOne: a tenant operator
// asking for another tenant's evaluation, or scoring another tenant's run,
// gets exactly the refusal an id that does not exist gets, so a guessed id
// says nothing about the other tenant.
func TestEvaluation_AnotherTenantsIDReadsLikeAMissingOne(t *testing.T) {
	for _, s := range surfaces {
		e := newEnv(t)
		bearer := e.bearers[callerTenant]
		theirs := s.call(e, bearer, `{"op":"get","eval_id":"eval_globex"}`)
		none := s.call(e, bearer, `{"op":"get","eval_id":"eval_nope"}`)
		if theirs.kind != "refused" || strings.ReplaceAll(theirs.text, "eval_globex", "<id>") != strings.ReplaceAll(none.text, "eval_nope", "<id>") {
			t.Errorf("%s: get of another tenant's evaluation = %v, want the missing-id refusal %v", s.name, theirs, none)
		}
		theirs = s.call(e, bearer, `{"op":"submit","run_id":"`+e.fx.runGlobex+`","score":0.1}`)
		none = s.call(e, bearer, `{"op":"submit","run_id":"r_nope","score":0.1}`)
		if theirs.kind != "refused" || strings.ReplaceAll(theirs.text, "<run-globex>", "<id>") != strings.ReplaceAll(none.text, "r_nope", "<id>") {
			t.Errorf("%s: submit against another tenant's run = %v, want the missing-run refusal %v", s.name, theirs, none)
		}
		rows, err := e.st.EvaluationListForRun(context.Background(), e.fx.runGlobex, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("%s: globex's run holds %d evaluations, want its own 1", s.name, len(rows))
		}
	}
}
