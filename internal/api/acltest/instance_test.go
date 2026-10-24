package acltest

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Test fixture, not a secret: a tenant operator bound to the deny below.
const fenced = "lct_acme_fenced"

// newInstanceEnv is newEnv with the Document, Path and CredentialDef tools
// wired, so their off-run surfaces (POST /v1/_document, /v1/_path,
// /v1/_credentialdef, the gRPC RPCs and the MCP meta-tools) reach the real
// tools.
func newInstanceEnv(t *testing.T, mode authz.Mode) *env {
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
	for _, tok := range []struct{ bearer, subject string }{{operator, "ops"}, {fenced, "fenced"}} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: "def_" + tok.subject, Name: "acme-" + tok.subject, TenantID: "acme", Subject: tok.subject,
			TokenHash: auth.HashToken(pepper, tok.bearer), AllowedScopes: []string{auth.ScopeTenant},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	tl := []tools.Tool{
		&builtin.Document{Store: st, SqlMem: mgr},
		&builtin.Path{Store: st},
		&builtin.CredentialDef{Engine: credential.NewEngine(st, sealer)},
	}
	srv := lchttp.New(cfg, noProviders{}, tl, concurrency.New(4, 4, 2*time.Second), st)
	srv.SetSqlMem(mgr)
	srv.SetACLResolver(aclres.New(st, mode, 0))
	srv.SetMCPHTTPHandler(lcmcp.NewHTTPHandler(lcmcp.Config{Connector: srv, Runner: srv, Store: st, Logger: slog.New(slog.DiscardHandler)}))
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

func documentRPC(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.Document
}
func pathRPC(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.Path
}
func credentialRPC(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.CredentialDef
}

// seedInstances files /kb/secret/plan, /kb/secret/hidden and
// /kb/public/notes in fenced's own user scope (the scope every transport
// keys on the caller's subject), a tenant credential S, and then, as the
// operator, binds fenced a list denying it writes under /kb/secret, reads of
// /kb/secret/hidden and managing S.
func (e *env) seedInstances() (secretChunk, publicChunk string) {
	e.t.Helper()
	as := fenced
	post := func(path, body string, want int) map[string]any {
		resp, raw := e.do(http.MethodPost, path, as, body, nil)
		if resp.StatusCode != want {
			e.t.Fatalf("seed %s %s: %d %s", path, body, resp.StatusCode, raw)
		}
		m, _ := decoded(raw).(map[string]any)
		return m
	}
	chunk := func(docPath, title string) string {
		doc := post("/v1/_document", `{"op":"create_document","scope":"user","title":"`+title+`","path":"`+docPath+`"}`, http.StatusOK)
		c := post("/v1/_document", `{"op":"create_chunk","scope":"user","document_id":"`+doc["document_id"].(string)+`","title":"p","body":"b"}`, http.StatusOK)
		return c["id"].(string)
	}
	secretChunk = chunk("/kb/secret/plan", "Plan")
	publicChunk = chunk("/kb/public/notes", "Notes")
	chunk("/kb/secret/hidden", "Hidden")
	post("/v1/_credentialdef", `{"op":"create","scope":"tenant","name":"S","value":"v1"}`, http.StatusOK)
	rules := `[{"effect":"deny","actions":["write","delete"],"resources":["document:user:/kb/secret/**"]},` +
		`{"effect":"deny","actions":["read"],"resources":["document:user:/kb/secret/hidden"]},` +
		`{"effect":"deny","actions":["manage"],"resources":["credential:tenant:S"]}]`
	as = operator
	post("/v1/_acldef", `{"op":"create","name":"fence","rules":`+rules+`}`, http.StatusCreated)
	post("/v1/_aclbinding", `{"acl":"fence","subject_kind":"user","subject_id":"fenced"}`, http.StatusCreated)
	return secretChunk, publicChunk
}

const aclRefusal = "access-control rules do not allow"

// The off-run Document, Path and CredentialDef surfaces ask the caller's set
// about the instance the call resolves to, with one answer on every
// transport.
func TestInstanceACL_OffRunSurfacesAskTheInstanceOnEveryTransport(t *testing.T) {
	e := newInstanceEnv(t, authz.ModeCompat)
	secret, public := e.seedInstances()
	// add_tags is a write that every transport can repeat (update_chunk
	// would need each call's current revision).
	tag := func(chunk string) string {
		return `{"op":"add_tags","scope":"user","id":"` + chunk + `","tags":["t"]}`
	}
	update := `{"op":"update_chunk","scope":"user","id":"` + secret + `","body":"x","revision":1}`
	cases := []struct {
		name    string
		bearer  string
		call    call
		refused bool
	}{
		{"a write under the denied path", fenced, call{"document", update, "POST", "/v1/_document", update, documentRPC}, true},
		{"a tag write under the denied path", fenced, call{"document", tag(secret), "POST", "/v1/_document", tag(secret), documentRPC}, true},
		{"a write elsewhere", fenced, call{"document", tag(public), "POST", "/v1/_document", tag(public), documentRPC}, false},
		{"a rename out of the denied path", fenced, call{"path", `{"op":"mv","scope":"user","path":"/kb/secret/plan","to":"/kb/plan"}`, "POST", "/v1/_path", `{"op":"mv","scope":"user","path":"/kb/secret/plan","to":"/kb/plan"}`, pathRPC}, true},
		{"a get of a read-denied document", fenced, call{"document", `{"op":"get_document","scope":"user","path":"/kb/secret/hidden"}`, "POST", "/v1/_document", `{"op":"get_document","scope":"user","path":"/kb/secret/hidden"}`, documentRPC}, true},
		{"rotating a managed-denied credential", fenced, call{"credentialdef", `{"op":"create","scope":"tenant","name":"S","value":"v2"}`, "POST", "/v1/_credentialdef", `{"op":"create","scope":"tenant","name":"S","value":"v2"}`, credentialRPC}, true},
		{"reading that credential's metadata", fenced, call{"credentialdef", `{"op":"get","scope":"tenant","name":"S"}`, "POST", "/v1/_credentialdef", `{"op":"get","scope":"tenant","name":"S"}`, credentialRPC}, false},
	}
	transports := map[string]func(string, call) outcome{"http": e.viaHTTP, "grpc": e.viaGRPC, "mcp": e.viaMCP}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for name, via := range transports {
				o := via(c.bearer, c.call)
				got := strings.Contains(o.raw, aclRefusal)
				if got != c.refused || (!c.refused && !o.allowed) {
					t.Errorf("%s: allowed=%v refused-by-acl=%v (%s), want refused=%v", name, o.allowed, got, o.raw, c.refused)
				}
			}
		})
	}

	// A listing leaves out what the caller may not read, on every transport.
	list := call{"path", `{"op":"ls","scope":"user","path":"/kb/secret"}`, "POST", "/v1/_path", `{"op":"ls","scope":"user","path":"/kb/secret"}`, pathRPC}
	query := call{"document", `{"op":"query_documents","scope":"user"}`, "POST", "/v1/_document", `{"op":"query_documents","scope":"user"}`, documentRPC}
	for name, via := range transports {
		if o := via(fenced, list); !o.allowed || strings.Contains(o.raw, "hidden") || !strings.Contains(o.raw, "plan") {
			t.Errorf("%s: ls /kb/secret = %s, want plan without hidden", name, o.raw)
		}
		if o := via(fenced, query); !o.allowed || strings.Contains(o.raw, "Hidden") || !strings.Contains(o.raw, "Plan") {
			t.Errorf("%s: query_documents = %s, want Plan without Hidden", name, o.raw)
		}
	}
}
