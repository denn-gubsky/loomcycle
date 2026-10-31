// Package subjectmergetest holds the transport parity test of the ontology
// subject merge (RFC FR P2): the same merge driven through POST
// /v1/_ontology/subjects/merge, the gRPC MergeSubjects RPC and the MCP
// `ontology` tool, each behind its real authentication over one real server
// wired as boot wires it (SQL Memory, a channel bus), must give one answer and
// leave the store in one state.
//
// It is a package of its own because it needs all three transports, and the
// transports' own test packages cannot import each other.
package subjectmergetest

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
	"strings"
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
	pepper   = "pep-subject-merge"
	legacy   = "legacy-subject-merge"
	operator = "lct_acme_ops"
	member   = "lct_acme_alice"
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
	// The seeded subjects.
	dup, dupRoot, surv, survRoot, report string
}

// newEnv builds one real server reachable three ways and seeds a tenant with
// two subjects for one person and a report citing the duplicate.
func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "none", Model: "none"},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.OperatorTokenPepper = pepper
	cfg.Env.AuthToken = legacy
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	for _, tok := range []struct {
		bearer, subject string
		scopes          []string
	}{
		{operator, "ops", []string{auth.ScopeTenant}},
		{member, "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: "def_" + tok.subject, Name: "acme-" + tok.subject, TenantID: tenant, Subject: tok.subject,
			TokenHash: auth.HashToken(pepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv := lchttp.New(cfg, noProviders{}, nil, concurrency.New(4, 4, 2*time.Second), st)
	srv.SetSqlMem(mgr)
	srv.SetChannelBus(channels.NewBus())
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
	subject := func(title, slug, key string) (string, string) {
		out := e.docCall(fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":"/facts/%s","type":"person","subject":%q,"natural_key":%q}`,
			title, slug, title, key))
		return out["document_id"].(string), out["root_chunk_id"].(string)
	}
	e.dup, e.dupRoot = subject("Ada King", "ada-king", "person:ada-king")
	e.surv, e.survRoot = subject("Ada Lovelace", "ada-lovelace", "person:ada-lovelace")
	e.docCall(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Wrote the notes","body":"She wrote the notes."}`, e.dup))
	e.report = e.docCall(`{"op":"create_document","scope":"tenant","title":"Report","path":"/kb/report"}`)["document_id"].(string)
	cite := e.docCall(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Summary","body":"Ada King wrote the notes."}`, e.report))["id"].(string)
	e.docCall(fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, cite, e.dupRoot))
	e.docCall(fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"same_as"}`, e.dupRoot, e.survRoot))
}

// outcome is one merge's answer in terms every transport shares.
type outcome struct {
	ok bool
	// result is the success payload as JSON, the shared result shape.
	result map[string]any
	// code is the refusal: the merge code, or "insufficient_scope" for a caller
	// the transport's own scope gate turned away.
	code string
	raw  string
}

type surface struct {
	name  string
	merge func(e *env, bearer, dup, surv string) outcome
}

var surfaces = []surface{
	{"http", (*env).mergeHTTP},
	{"grpc", (*env).mergeGRPC},
	{"mcp", (*env).mergeMCP},
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

func (e *env) mergeHTTP(bearer, dup, surv string) outcome {
	body, _ := json.Marshal(map[string]string{"duplicate": dup, "survivor": surv})
	resp, raw := e.do(http.MethodPost, "/v1/_ontology/subjects/merge", bearer, string(body), nil)
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

func (e *env) mergeGRPC(bearer, dup, surv string) outcome {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.MergeSubjects(ctx, &loomcyclepb.MergeSubjectsRequest{Duplicate: dup, Survivor: surv})
	if err == nil {
		// Rendered in the shared result's JSON names, so the three compare as one.
		ref := func(r *loomcyclepb.SubjectRef) map[string]any {
			return map[string]any{"document_id": r.GetDocumentId(), "root_chunk_id": r.GetRootChunkId(), "natural_key": r.GetNaturalKey()}
		}
		m := map[string]any{
			"tenant": resp.GetTenant(), "duplicate": ref(resp.GetDuplicate()), "survivor": ref(resp.GetSurvivor()),
			"facts_moved": float64(resp.GetFactsMoved()), "statements_repointed": float64(resp.GetStatementsRepointed()),
			"statements_retired": float64(resp.GetStatementsRetired()), "statements_left": float64(resp.GetStatementsLeft()),
		}
		if resp.GetNotAnnounced() != "" {
			m["not_announced"] = resp.GetNotAnnounced()
		}
		return outcome{ok: true, result: m, raw: resp.String()}
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

func (e *env) mergeMCP(bearer, dup, surv string) outcome {
	args, _ := json.Marshal(map[string]string{"op": "merge_subjects", "duplicate": dup, "survivor": surv})
	reply := e.mcpRPC(bearer, e.mcpSession(bearer), "tools/call", `{"name":"ontology","arguments":`+string(args)+`}`)
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

// state is what a merge left in the store, in terms that do not depend on the
// environment's random ids.
func (e *env) state() map[string]any {
	e.t.Helper()
	root := e.docCall(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, e.dupRoot))
	fields, _ := root["fields"].(map[string]any)
	out := map[string]any{
		"dup_status":            root["status"],
		"merged_into_survivor":  fields["merged_into"] == e.surv,
		"survivor_holds_a_fact": false,
	}
	survDoc := e.docCall(fmt.Sprintf(`{"op":"query_chunks","scope":"tenant","document_id":%q}`, e.surv))
	if chunks, _ := survDoc["chunks"].([]any); len(chunks) > 1 {
		out["survivor_holds_a_fact"] = true
	}
	var msgs []map[string]any
	all, err := e.st.SnapshotReadChannelMessages(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, m := range all {
		if m.Channel != "memory/subjects" {
			continue
		}
		var ev map[string]any
		_ = json.Unmarshal(m.Payload, &ev)
		msgs = append(msgs, map[string]any{
			"op": ev["op"], "tenant": m.TenantID, "scope": string(m.Scope),
			"names_duplicate": ev["document_id"] == e.dup && ev["root_chunk_id"] == e.dupRoot,
			"names_survivor":  ev["surviving_document_id"] == e.surv && ev["surviving_key"] == "person:ada-lovelace",
			"key":             ev["subject_natural_key"],
		})
	}
	out["events"] = msgs
	return out
}

// normalized drops the environment's random ids from a result, keeping what
// they say: that the result names the seeded subjects.
func (e *env) normalized(r map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		out[k] = v
	}
	for side, want := range map[string][2]string{"duplicate": {e.dup, e.dupRoot}, "survivor": {e.surv, e.survRoot}} {
		ref, _ := r[side].(map[string]any)
		out[side] = map[string]any{
			"natural_key": ref["natural_key"],
			"is_seeded":   ref["document_id"] == want[0] && ref["root_chunk_id"] == want[1],
		}
	}
	return out
}

// TestMergeSubjects_SameResultOnEveryTransport (RFC FR §10 P2): the same merge,
// driven once per transport against identical tenants, answers the same and
// leaves the same store — the duplicate retired and pointing at the survivor,
// its fact moved, and one `merged` message on the tenant's memory/subjects
// naming both. The HTTP leg is the real route on a server wired as boot wires
// it, read back from the store.
func TestMergeSubjects_SameResultOnEveryTransport(t *testing.T) {
	var firstResult, firstState map[string]any
	for _, s := range surfaces {
		e := newEnv(t)
		o := s.merge(e, operator, "person:ada-king", e.surv)
		if !o.ok {
			t.Fatalf("%s: merge refused: %s %s", s.name, o.code, o.raw)
		}
		result, state := e.normalized(o.result), e.state()
		if firstResult == nil {
			firstResult, firstState = result, state
			want := map[string]any{
				"dup_status": "merged", "merged_into_survivor": true, "survivor_holds_a_fact": true,
				"events": []map[string]any{{"op": "merged", "tenant": tenant, "scope": "tenant", "names_duplicate": true, "names_survivor": true, "key": "person:ada-king"}},
			}
			if !reflect.DeepEqual(state, want) {
				t.Errorf("%s: state = %v, want %v", s.name, state, want)
			}
			if result["tenant"] != tenant || result["facts_moved"] != float64(1) || result["statements_repointed"] != float64(1) ||
				result["statements_retired"] != float64(1) || result["not_announced"] != nil {
				t.Errorf("%s: result = %v", s.name, result)
			}
			continue
		}
		if !reflect.DeepEqual(result, firstResult) {
			t.Errorf("%s answered %v, http answered %v", s.name, result, firstResult)
		}
		if !reflect.DeepEqual(state, firstState) {
			t.Errorf("%s left %v, http left %v", s.name, state, firstState)
		}
	}
}

// TestMergeSubjects_SameRefusalOnEveryTransport: each refusal carries the same
// code on every transport, and none changes anything.
func TestMergeSubjects_SameRefusalOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	cases := []struct{ name, dup, surv, code string }{
		{"same subject", e.dup, "person:ada-king", "same_subject"},
		{"not a subject", e.report, e.surv, "not_a_subject"},
		{"not in the scope", "person:nobody", e.surv, "subject_not_in_scope"},
	}
	for _, s := range surfaces {
		for _, c := range cases {
			if o := s.merge(e, operator, c.dup, c.surv); o.ok || o.code != c.code {
				t.Errorf("%s %s: ok=%v code %q, want %q (%s)", s.name, c.name, o.ok, o.code, c.code, o.raw)
			}
		}
	}
	if st := e.state(); st["dup_status"] == "merged" || len(st["events"].([]map[string]any)) != 0 {
		t.Fatalf("a refused merge changed the store: %v", st)
	}
	if o := surfaces[0].merge(e, operator, e.dup, e.surv); !o.ok {
		t.Fatalf("merge: %s %s", o.code, o.raw)
	}
	for _, s := range surfaces {
		if o := s.merge(e, operator, e.dup, e.surv); o.ok || o.code != "already_merged" {
			t.Errorf("%s re-merge: ok=%v code %q, want already_merged (%s)", s.name, o.ok, o.code, o.raw)
		}
	}
}

// TestMergeSubjects_MembersAreRefusedOnEveryTransport (RFC FR §9): a member
// token of the tenant — which reaches the other ontology routes on the member
// path — is refused the merge on all three transports, and nothing changes.
func TestMergeSubjects_MembersAreRefusedOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	for _, s := range surfaces {
		if o := s.merge(e, member, e.dup, e.surv); o.ok || o.code != "insufficient_scope" {
			t.Errorf("%s: a member's merge: ok=%v code %q (%s), want refused at the scope gate", s.name, o.ok, o.code, o.raw)
		}
	}
	if st := e.state(); st["dup_status"] == "merged" || len(st["events"].([]map[string]any)) != 0 {
		t.Fatalf("a member's merge changed the store: %v", st)
	}
}
