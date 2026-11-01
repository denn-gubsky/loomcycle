// Package statementreviewtest holds the transport parity tests of the
// statement review surface (RFC FS P0): the review queue, bulk rulings and
// their undo, each driven through its HTTP route, its gRPC RPC and the MCP
// `review` tool, behind each transport's real authentication, over one real
// server wired as boot wires it (SQL Memory, a channel bus, the access-control
// resolver). The three must give one answer and leave the store in one state.
//
// It is a package of its own because it needs all three transports, and the
// transports' own test packages cannot import each other.
package statementreviewtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
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
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Test fixtures, not secrets: these bearers exist only inside this test's
// in-memory server.
const (
	pepper   = "pep-statement-review"
	legacy   = "legacy-statement-review"
	operator = "lct_acme_ops"
	reviewer = "lct_acme_rita"
	member   = "lct_acme_alice"
	isolated = "lct_acme_iris"
	tenant   = "acme"
)

type noProviders struct{}

func (noProviders) Get(string) (providers.Provider, error) { return nil, io.EOF }

type env struct {
	t    *testing.T
	st   store.Store
	doc  *builtin.Document
	ts   *httptest.Server
	grpc loomcyclepb.LoomcycleClient
	// The seeded proposals: two from /board, one from /kb.
	cites, repeals, elaborates string
}

// newEnv builds one real server reachable three ways, with a reviewer member
// whose access-control rules grant `review` under /board, and seeds the queue.
func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "none", Model: "none"},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.OperatorTokenPepper = pepper
	cfg.Env.AuthToken = legacy
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	for _, tok := range []struct {
		bearer, subject string
		scopes          []string
	}{
		{operator, "ops", []string{auth.ScopeTenant}},
		{reviewer, "rita", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{member, "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{isolated, "iris", []string{auth.ScopeUser}},
	} {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: "def_" + tok.subject, Name: "acme-" + tok.subject, TenantID: tenant, Subject: tok.subject,
			TokenHash: auth.HashToken(pepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: "board-review",
		Rules: json.RawMessage(`[{"effect":"allow","actions":["review"],"resources":["document:tenant:/board/**"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: tenant, ACLName: "board-review", SubjectKind: "user", SubjectID: "rita"}); err != nil {
		t.Fatal(err)
	}
	srv := lchttp.New(cfg, noProviders{}, nil, concurrency.New(4, 4, 2*time.Second), st)
	srv.SetSqlMem(mgr)
	srv.SetChannelBus(channels.NewBus())
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	srv.SetMCPHTTPHandler(lcmcp.NewHTTPHandler(lcmcp.Config{Connector: srv, Runner: srv, Store: st, Logger: slog.New(slog.DiscardHandler)}))
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

	e := &env{t: t, st: st, doc: &builtin.Document{Store: st, SqlMem: mgr}, ts: ts, grpc: loomcyclepb.NewLoomcycleClient(conn)}
	e.seed()
	return e
}

// docCtx is the tenant's Document as an operator's off-run write sees it.
func (e *env) docCtx() context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: tenant})
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		p.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	})
}

func (e *env) docCall(body string) map[string]any {
	e.t.Helper()
	res, err := e.doc.Execute(e.docCtx(), json.RawMessage(body))
	if err != nil || res.IsError {
		e.t.Fatalf("%s: %v %s", body, err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

func (e *env) seed() {
	chunk := func(path string) string {
		d := e.docCall(fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q}`, path, path))["document_id"].(string)
		return e.docCall(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"part","body":"text"}`, d))["id"].(string)
	}
	one, two, notes := chunk("/board/reg/one"), chunk("/board/reg/two"), chunk("/kb/notes/n")
	propose := func(from, to, kind string) string {
		return e.docCall(fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":%q}`, from, to, kind))["id"].(string)
	}
	e.cites, e.repeals, e.elaborates = propose(one, two, "cites"), propose(one, two, "repeals"), propose(notes, two, "elaborates")
}

// outcome is one call's answer in terms every transport shares.
type outcome struct {
	ok     bool
	result map[string]any
	// code is the refusal: the review code, or "insufficient_scope" for a
	// caller the transport's own gate turned away.
	code string
	raw  string
}

type surface struct {
	name   string
	queue  func(e *env, bearer string, summarize bool) outcome
	rule   func(e *env, bearer string, req map[string]any) outcome
	revert func(e *env, bearer, rulingID string) outcome
}

var surfaces = []surface{
	{"http", (*env).queueHTTP, (*env).ruleHTTP, (*env).revertHTTP},
	{"grpc", (*env).queueGRPC, (*env).ruleGRPC, (*env).revertGRPC},
	{"mcp", (*env).queueMCP, (*env).ruleMCP, (*env).revertMCP},
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

func httpOutcome(resp *http.Response, raw []byte) outcome {
	if resp.StatusCode == http.StatusOK {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return outcome{ok: true, result: m, raw: string(raw)}
	}
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient") {
		return outcome{code: "insufficient_scope", raw: string(raw)}
	}
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	return outcome{code: b.Code, raw: string(raw)}
}

func (e *env) queueHTTP(bearer string, summarize bool) outcome {
	return httpOutcome(e.do(http.MethodGet, fmt.Sprintf("/v1/_statements/review?summarize=%v", summarize), bearer, "", nil))
}

func (e *env) ruleHTTP(bearer string, req map[string]any) outcome {
	body, _ := json.Marshal(req)
	return httpOutcome(e.do(http.MethodPost, "/v1/_statements/rule", bearer, string(body), nil))
}

func (e *env) revertHTTP(bearer, rulingID string) outcome {
	body, _ := json.Marshal(map[string]string{"ruling_id": rulingID})
	return httpOutcome(e.do(http.MethodPost, "/v1/_statements/revert", bearer, string(body), nil))
}

func grpcOutcome(err error, m map[string]any) outcome {
	if err == nil {
		raw, _ := json.Marshal(m)
		// Round-trip through JSON so numbers compare as the other transports'.
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return outcome{ok: true, result: out, raw: string(raw)}
	}
	st := status.Convert(err)
	o := outcome{raw: st.Message()}
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

func authed(bearer string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
}

func items(in []*loomcyclepb.RulingItem) []map[string]any {
	out := []map[string]any{}
	for _, it := range in {
		m := map[string]any{"id": it.GetId()}
		for k, v := range map[string]string{"status": it.GetStatus(), "error": it.GetError(), "message": it.GetMessage()} {
			if v != "" {
				m[k] = v
			}
		}
		out = append(out, m)
	}
	return out
}

func (e *env) queueGRPC(bearer string, summarize bool) outcome {
	resp, err := e.grpc.ListReviewQueue(authed(bearer), &loomcyclepb.ListReviewQueueRequest{Summarize: summarize})
	if err != nil {
		return grpcOutcome(err, nil)
	}
	stmts := []json.RawMessage{}
	for _, s := range resp.GetStatementsJson() {
		stmts = append(stmts, s)
	}
	m := map[string]any{"tenant": resp.GetTenant(), "scope": resp.GetScope(), "statements": stmts}
	if summarize {
		var groups []map[string]any
		for _, g := range resp.GetSummary() {
			group := map[string]any{"kind": g.GetKind(), "method": g.GetMethod(), "from_path_prefix": g.GetFromPathPrefix(),
				"judge_verdict": g.GetJudgeVerdict(), "count": g.GetCount()}
			if g.GetFilter() != nil {
				group["filter"] = filterMap(g.GetFilter())
			}
			groups = append(groups, group)
		}
		m["summary"], m["total"] = groups, resp.GetTotal()
	}
	return grpcOutcome(nil, m)
}

func (e *env) ruleGRPC(bearer string, req map[string]any) outcome {
	in := &loomcyclepb.RuleStatementsRequest{}
	in.Action, _ = req["action"].(string)
	in.Reason, _ = req["reason"].(string)
	if ids, ok := req["ids"].([]string); ok {
		in.Ids = ids
	}
	if f, ok := req["filter"].(map[string]any); ok {
		in.HasFilter = true
		in.Filter = &loomcyclepb.ReviewQueueFilter{}
		in.Filter.FromUnderPath, _ = f["from_under_path"].(string)
	}
	if d, ok := req["dry_run"].(bool); ok {
		in.DryRun = &d
	}
	if n, ok := req["expect_count"].(float64); ok {
		c := int32(n)
		in.ExpectCount = &c
	}
	resp, err := e.grpc.RuleStatements(authed(bearer), in)
	if err != nil {
		return grpcOutcome(err, nil)
	}
	breakdown := []map[string]any{}
	for _, b := range resp.GetBreakdown() {
		breakdown = append(breakdown, map[string]any{"kind": b.GetKind(), "method": b.GetMethod(), "count": b.GetCount()})
	}
	m := map[string]any{"tenant": resp.GetTenant(), "scope": resp.GetScope(), "action": resp.GetAction(), "dry_run": resp.GetDryRun(),
		"matched": resp.GetMatched(), "selected": resp.GetSelected(), "skipped": resp.GetSkipped(), "to_rule": resp.GetToRule(),
		"ruled": resp.GetRuled(), "failed": resp.GetFailed(), "breakdown": breakdown}
	if resp.GetRulingId() != "" {
		m["ruling_id"] = resp.GetRulingId()
	}
	if len(resp.GetItems()) > 0 {
		m["items"] = items(resp.GetItems())
	}
	if len(resp.GetSampleJson()) > 0 {
		var sample []json.RawMessage
		for _, s := range resp.GetSampleJson() {
			sample = append(sample, s)
		}
		m["sample"] = sample
	}
	return grpcOutcome(nil, m)
}

func (e *env) revertGRPC(bearer, rulingID string) outcome {
	resp, err := e.grpc.RevertRuling(authed(bearer), &loomcyclepb.RevertRulingRequest{RulingId: rulingID})
	if err != nil {
		return grpcOutcome(err, nil)
	}
	return grpcOutcome(nil, map[string]any{"tenant": resp.GetTenant(), "scope": resp.GetScope(), "ruling_id": resp.GetRulingId(),
		"action": resp.GetAction(), "reverted": resp.GetReverted(), "changed": resp.GetChanged(), "skipped": resp.GetSkipped(),
		"items": items(resp.GetItems()), "revert_id": resp.GetRevertId()})
}

// filterMap is a proto filter in the JSON shape the other transports return
// (connector.ReviewQueueFilter: every field omitted when unset).
func filterMap(f *loomcyclepb.ReviewQueueFilter) map[string]any {
	m := map[string]any{}
	for k, v := range map[string]string{"status": f.GetStatus(), "kind": f.GetKind(), "method": f.GetMethod(), "actor": f.GetActor(),
		"document_id": f.GetDocumentId(), "under_path": f.GetUnderPath(), "from_document_id": f.GetFromDocumentId(),
		"to_document_id": f.GetToDocumentId(), "from_under_path": f.GetFromUnderPath(), "to_under_path": f.GetToUnderPath(),
		"judge_verdict": f.GetJudgeVerdict(), "method_exact": f.GetMethodExact(), "kind_exact": f.GetKindExact(),
		"from_path_prefix": f.GetFromPathPrefix()} {
		if v != "" {
			m[k] = v
		}
	}
	if len(f.GetKinds()) > 0 {
		m["kinds"] = f.GetKinds()
	}
	if f.MinConfidence != nil {
		m["min_confidence"] = f.GetMinConfidence()
	}
	if f.CreatedBefore != nil {
		m["created_before"] = f.GetCreatedBefore()
	}
	if f.CreatedAfter != nil {
		m["created_after"] = f.GetCreatedAfter()
	}
	if f.HasJudgeVerdict != nil {
		m["has_judge_verdict"] = f.GetHasJudgeVerdict()
	}
	if f.GetFromUnfiled() {
		m["from_unfiled"] = true
	}
	return m
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *env) mcpRPC(bearer, sessionID, method, params string) rpcReply {
	e.t.Helper()
	h := map[string]string{"Accept": "application/json, text/event-stream", "Mcp-Session-Id": sessionID}
	resp, raw := e.do(http.MethodPost, "/v1/_mcp", bearer, `{"jsonrpc":"2.0","id":2,"method":"`+method+`","params":`+params+`}`, h)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s = %d: %s", method, resp.StatusCode, raw)
	}
	var reply rpcReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		e.t.Fatalf("%s reply is not JSON-RPC: %s", method, raw)
	}
	return reply
}

func (e *env) mcpSession(bearer string) string {
	e.t.Helper()
	resp, raw := e.do(http.MethodPost, "/v1/_mcp", bearer,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		map[string]string{"Accept": "application/json, text/event-stream"})
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("initialize = %d: %s", resp.StatusCode, raw)
	}
	return resp.Header.Get("Mcp-Session-Id")
}

// reviewMCP calls the review tool in a fresh session and reads its answer as an
// outcome: a refusal's code is the {code, error} body's.
func (e *env) reviewMCP(bearer string, arguments map[string]any) outcome {
	args, _ := json.Marshal(arguments)
	reply := e.mcpRPC(bearer, e.mcpSession(bearer), "tools/call", `{"name":"review","arguments":`+string(args)+`}`)
	if reply.Error != nil {
		o := outcome{raw: reply.Error.Message}
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
	var m map[string]any
	_ = json.Unmarshal([]byte(text), &m)
	if !res.IsError {
		return outcome{ok: true, result: m, raw: text}
	}
	code, _ := m["code"].(string)
	return outcome{code: code, raw: text}
}

func (e *env) queueMCP(bearer string, summarize bool) outcome {
	return e.reviewMCP(bearer, map[string]any{"op": "queue", "summarize": summarize})
}

func (e *env) ruleMCP(bearer string, req map[string]any) outcome {
	args := map[string]any{"op": "rule"}
	for k, v := range req {
		args[k] = v
	}
	return e.reviewMCP(bearer, args)
}

func (e *env) revertMCP(bearer, rulingID string) outcome {
	return e.reviewMCP(bearer, map[string]any{"op": "revert", "ruling_id": rulingID})
}

// normalized drops what differs by environment (statement and ruling ids, a
// sample's timestamps) and keeps what each says, in the seeded names.
func (e *env) normalized(r map[string]any) map[string]any {
	names := map[string]string{e.cites: "cites", e.repeals: "repeals", e.elaborates: "elaborates"}
	out := map[string]any{}
	for k, v := range r {
		switch k {
		case "ruling_id", "revert_id":
			out[k] = v != ""
		case "items":
			var its []string
			for _, it := range v.([]any) {
				m := it.(map[string]any)
				its = append(its, fmt.Sprint(names[m["id"].(string)], "|", m["status"], "|", m["error"]))
			}
			sort.Strings(its)
			out[k] = its
		case "sample", "statements":
			var ids []string
			for _, s := range v.([]any) {
				ids = append(ids, names[s.(map[string]any)["id"].(string)])
			}
			sort.Strings(ids)
			out[k] = ids
		default:
			out[k] = v
		}
	}
	return out
}

// state is each seeded statement's status and who ruled it, the ruling named
// by its kind rather than its id.
func (e *env) state() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for name, id := range map[string]string{"cites": e.cites, "repeals": e.repeals, "elaborates": e.elaborates} {
		s := e.docCall(fmt.Sprintf(`{"op":"get_edge","scope":"tenant","id":%q}`, id))["statement"].(map[string]any)
		by, _ := s["ruled_by"].(string)
		if kind, _, ok := strings.Cut(by, ":"); ok {
			by = kind
		}
		out[name] = fmt.Sprint(s["status"], "|", by)
	}
	return out
}

// TestStatementReview_SameRulingAndRevertOnEveryTransport: one bulk ruling,
// driven as dry run, ruling and revert once per transport against identical
// tenants, answers the same at each step and leaves the same store.
func TestStatementReview_SameRulingAndRevertOnEveryTransport(t *testing.T) {
	var first []any
	for _, s := range surfaces {
		e := newEnv(t)
		req := map[string]any{"action": "accept", "filter": map[string]any{"from_under_path": "/board"}}
		dry := s.rule(e, operator, req)
		if !dry.ok {
			t.Fatalf("%s: dry run refused: %s %s", s.name, dry.code, dry.raw)
		}
		req["dry_run"], req["expect_count"] = false, dry.result["matched"]
		real := s.rule(e, operator, req)
		if !real.ok {
			t.Fatalf("%s: ruling refused: %s %s", s.name, real.code, real.raw)
		}
		ruled := e.state()
		rev := s.revert(e, operator, real.result["ruling_id"].(string))
		if !rev.ok {
			t.Fatalf("%s: revert refused: %s %s", s.name, rev.code, rev.raw)
		}
		steps := []any{e.normalized(dry.result), e.normalized(real.result), ruled, e.normalized(rev.result), e.state()}
		if first == nil {
			first = steps
			want := map[string]string{"cites": "asserted|bulk", "repeals": "asserted|bulk", "elaborates": "proposed|"}
			if !reflect.DeepEqual(ruled, want) {
				t.Errorf("%s: after the ruling %v, want %v", s.name, ruled, want)
			}
			if r := real.result; r["matched"] != float64(2) || r["ruled"] != float64(2) || r["tenant"] != tenant {
				t.Errorf("%s: ruling = %v", s.name, r)
			}
			if after := e.state(); after["cites"] != "proposed|" || after["repeals"] != "proposed|" {
				t.Errorf("%s: after the revert %v", s.name, after)
			}
			continue
		}
		for i, step := range []string{"dry run", "ruling", "store after the ruling", "revert", "store after the revert"} {
			if !reflect.DeepEqual(steps[i], first[i]) {
				t.Errorf("%s %s = %v, http %v", s.name, step, steps[i], first[i])
			}
		}
	}
}

// TestStatementReview_ReviewerRulesOnlyUnderItsGrantOnEveryTransport: a member
// whose rules grant `review` under /board rules there on every transport, and
// the /kb proposal is skipped, counted and never named.
func TestStatementReview_ReviewerRulesOnlyUnderItsGrantOnEveryTransport(t *testing.T) {
	for _, s := range surfaces {
		e := newEnv(t)
		req := map[string]any{"action": "reject", "reason": "wrong act", "filter": map[string]any{}}
		dry := s.rule(e, reviewer, req)
		if !dry.ok {
			t.Fatalf("%s: dry run refused: %s %s", s.name, dry.code, dry.raw)
		}
		req["dry_run"], req["expect_count"] = false, dry.result["matched"]
		o := s.rule(e, reviewer, req)
		if !o.ok || o.result["ruled"] != float64(2) || o.result["skipped"] != float64(1) || strings.Contains(o.raw, e.elaborates) {
			t.Errorf("%s: reviewer ruling = %v %s", s.name, o.code, o.raw)
		}
		want := map[string]string{"cites": "rejected|bulk", "repeals": "rejected|bulk", "elaborates": "proposed|"}
		if got := e.state(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: store = %v, want %v", s.name, got, want)
		}
	}
}

// TestStatementReview_SameRefusalsOnEveryTransport: a member without `review`
// is refused (forbidden) and an isolated user is turned away at the gate, on
// every transport; a stale expect_count and a reject without a reason carry the
// same code everywhere; none changes anything.
func TestStatementReview_SameRefusalsOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	before := e.state()
	for _, s := range surfaces {
		for _, c := range []struct {
			name, bearer string
			req          map[string]any
			code         string
		}{
			{"a member without review", member, map[string]any{"action": "accept", "filter": map[string]any{}}, "forbidden"},
			{"an isolated user", isolated, map[string]any{"action": "accept", "filter": map[string]any{}}, "insufficient_scope"},
			{"a stale count", operator, map[string]any{"action": "accept", "filter": map[string]any{}, "dry_run": false, "expect_count": float64(99)}, "queue_changed"},
			{"a reject without a reason", operator, map[string]any{"action": "reject", "filter": map[string]any{}}, "invalid_request"},
		} {
			if o := s.rule(e, c.bearer, c.req); o.ok || o.code != c.code {
				t.Errorf("%s %s: ok=%v code %q, want %q (%s)", s.name, c.name, o.ok, o.code, c.code, o.raw)
			}
		}
		if o := s.revert(e, member, "anything"); o.ok || o.code != "forbidden" {
			t.Errorf("%s: a member's revert: ok=%v code %q (%s)", s.name, o.ok, o.code, o.raw)
		}
		if o := s.revert(e, operator, "no-such-ruling"); o.ok || o.code != "ruling_not_found" {
			t.Errorf("%s: an unknown ruling: ok=%v code %q (%s)", s.name, o.ok, o.code, o.raw)
		}
		if o := s.queue(e, isolated, false); o.ok || o.code != "insufficient_scope" {
			t.Errorf("%s: an isolated user's queue: ok=%v code %q (%s)", s.name, o.ok, o.code, o.raw)
		}
	}
	if after := e.state(); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused call changed the store: %v, was %v", after, before)
	}
}

// TestStatementReview_SameQueueOnEveryTransport: the queue and its summary read
// the same on every transport, for an operator and for a member.
func TestStatementReview_SameQueueOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	for _, bearer := range []string{operator, member} {
		var first [2]map[string]any
		for _, s := range surfaces {
			rows, summary := s.queue(e, bearer, false), s.queue(e, bearer, true)
			if !rows.ok || !summary.ok {
				t.Fatalf("%s: queue refused: %s %s / %s %s", s.name, rows.code, rows.raw, summary.code, summary.raw)
			}
			got := [2]map[string]any{e.normalized(rows.result), e.normalized(summary.result)}
			if first[0] == nil {
				first = got
				if fmt.Sprint(got[0]["statements"]) != "[cites elaborates repeals]" || got[1]["total"] != float64(3) {
					t.Errorf("%s: queue = %v / %v", s.name, got[0], got[1])
				}
				continue
			}
			if !reflect.DeepEqual(got, first) {
				t.Errorf("%s queue = %v, http %v", s.name, got, first)
			}
		}
	}
}
