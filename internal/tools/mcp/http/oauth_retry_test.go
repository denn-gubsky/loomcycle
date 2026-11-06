package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// invalidTokenChallenge is what a resource server answers a token it no
// longer accepts with (RFC 6750 §3.1).
const invalidTokenChallenge = `Bearer realm="docs", error="invalid_token", error_description="The access token expired"`

// oauthRig is an invented MCP server, protected by the fake authorization
// server, and a connected tenant OAuth credential "docs" for it. The server
// refuses the tokens the test revokes, and can bind its sessions to the
// token that opened them.
type oauthRig struct {
	f      *oauthtest.Server
	engine *credential.Engine
	mgr    *credential.OAuthManager
	id     credential.Identity

	mu sync.Mutex
	// revoked tokens are answered 401 with challenge.
	revoked    map[string]bool
	revokeAll  bool // every token is refused
	challenge  string
	tokenBound bool // sessions are tied to the token that opened them
	calls      []rigCall
}

type rigCall struct {
	method string
	token  string
}

func newOAuthRig(t *testing.T) *oauthRig {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	rig := &oauthRig{f: oauthtest.New(t), engine: credential.NewEngine(st, sealer),
		id: credential.Identity{TenantID: "t", Scope: "tenant", Name: "docs"}, revoked: map[string]bool{},
		challenge: invalidTokenChallenge}
	rig.f.Set(func(f *oauthtest.Server) {
		f.Interval = 1
		f.ResourceHandler = http.HandlerFunc(rig.serve)
	})
	rig.mgr = credential.NewOAuthManager(rig.engine, rig.f.Client(), "replica-test", nil)
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig.mgr.SetBackground(bg)
	if _, err := rig.engine.PutOAuth(context.Background(), rig.id, credential.OAuthConfig{Resource: rig.f.Resource(),
		Client: credential.OAuthClientConfig{Registration: credential.OAuthRegistrationDynamic, TokenEndpointAuth: credential.OAuthAuthNone}}, nil); err != nil {
		t.Fatal(err)
	}
	rig.connect(t)
	return rig
}

// connect connects (again) the credential and waits for the grant.
func (rig *oauthRig) connect(t *testing.T) {
	t.Helper()
	if _, err := rig.mgr.ConnectStart(context.Background(), rig.id); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if m, _ := rig.mgr.ConnectStatus(context.Background(), rig.id); m.OAuth.Status.State == credential.OAuthConnected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}
}

// sessionFor is the session the server opens for tok: derived from it, so
// a session can be checked against the token without holding the token.
func sessionFor(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return "sess-" + hex.EncodeToString(sum[:8])
}

func (rig *oauthRig) serve(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &probe)
	rig.mu.Lock()
	rig.calls = append(rig.calls, rigCall{method: probe.Method, token: tok})
	refused := rig.revoked[tok] || rig.revokeAll
	challenge, bound := rig.challenge, rig.tokenBound
	rig.mu.Unlock()
	if refused {
		if challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		oauthtest.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}
	if bound && probe.Method != "initialize" && r.Header.Get(sessionHeader) != sessionFor(tok) {
		oauthtest.WriteJSON(w, http.StatusNotFound, map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": -32001, "message": "session not found"}})
		return
	}
	if probe.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch probe.Method {
	case "initialize":
		if bound {
			w.Header().Set(sessionHeader, sessionFor(tok))
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{
			"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{},
			"serverInfo": map[string]any{"name": "docs", "version": "1"}}})
	case "tools/list":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{
			"tools": []map[string]any{{"name": "lookup", "inputSchema": map[string]any{"type": "object"}}}}})
	default:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}}}})
	}
}

// revoke makes the server refuse tok from now on.
func (rig *oauthRig) revoke(tok string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	rig.revoked[tok] = true
}

func (rig *oauthRig) set(fn func(rig *oauthRig)) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	fn(rig)
}

// count is how many requests of method the server saw.
func (rig *oauthRig) count(method string) int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	n := 0
	for _, c := range rig.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (rig *oauthRig) methods() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	out := make([]string, 0, len(rig.calls))
	for _, c := range rig.calls {
		out = append(out, c.method)
	}
	return out
}

// resolver is the $oauth: pass cmd/loomcycle wires, for the run identity on
// the request's ctx.
func (rig *oauthRig) resolver(ctx context.Context, s, target string) (string, []string, error) {
	ri := tools.RunIdentity(ctx)
	return rig.mgr.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, s, target, nil)
}

// client is a client of the rig's server whose Authorization header names
// header.
func (rig *oauthRig) client(t *testing.T, header string) *Client {
	t.Helper()
	c, err := New(Config{URL: rig.f.Resource(), Headers: map[string]string{"Authorization": header},
		HTTPClient: rig.f.Client(), CredSubstitute: rig.resolver})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rigCtx() context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a", UserID: "alice"})
}

// initialized is a client of the rig's server, past the handshake on the
// credential's first token.
func (rig *oauthRig) initialized(t *testing.T) *Client {
	t.Helper()
	c := rig.client(t, "Bearer $oauth:docs")
	if _, err := mcp.Initialize(rigCtx(), c, "loomcycle", "test"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMCPHTTPClient_OAuthInvalidTokenRefreshesOnceAndRetries(t *testing.T) {
	rig := newOAuthRig(t)
	c := rig.initialized(t)
	first := rig.f.LastAccess()
	tokensBefore := rig.f.Count("token")
	rig.revoke(first)

	res, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("CallTool: %v", err)
	}
	if n := rig.f.Count("token") - tokensBefore; n != 1 {
		t.Errorf("%d token requests, want exactly one refresh", n)
	}
	if n := rig.count("tools/call"); n != 2 {
		t.Errorf("the server saw %d tools/call, want the refused one and one retry", n)
	}
	if rig.f.LastAccess() == first {
		t.Error("no new access token was issued")
	}
}

func TestMCPHTTPClient_OAuthConcurrentInvalidTokensShareOneRefresh(t *testing.T) {
	rig := newOAuthRig(t)
	c := rig.initialized(t)
	tokensBefore := rig.f.Count("token")
	rig.f.Set(func(f *oauthtest.Server) { f.TokenDelay = 50 * time.Millisecond })
	rig.revoke(rig.f.LastAccess())

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`))
			if err == nil && res.IsError {
				err = errors.New("tool error")
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("CallTool: %v", err)
		}
	}
	if got := rig.f.Count("token") - tokensBefore; got != 1 {
		t.Errorf("%d token requests for %d concurrent refusals, want one", got, n)
	}
}

func TestMCPHTTPClient_OAuthSecondInvalidTokenNeedsReauthWithoutThirdAttempt(t *testing.T) {
	logs := &bytes.Buffer{}
	var logMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{w: logs, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rig := newOAuthRig(t)
	c := rig.initialized(t)
	tokensBefore := rig.f.Count("token")
	rig.set(func(rig *oauthRig) { rig.revokeAll = true })
	rig.revoke(rig.f.LastAccess())

	_, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a call refused twice succeeded")
	}
	if !errors.Is(err, credential.ErrOAuthReauthRequired) || !errors.Is(err, tools.ErrRunCredentialUnavailable) ||
		!strings.Contains(err.Error(), "oauth_connect_start") {
		t.Errorf("err = %v, want the reconnect refusal", err)
	}
	if n := rig.count("tools/call"); n != 2 {
		t.Errorf("the server saw %d tools/call, want two (no third attempt)", n)
	}
	if n := rig.f.Count("token") - tokensBefore; n != 1 {
		t.Errorf("%d token requests, want one refresh", n)
	}
	m, gerr := rig.engine.Get(context.Background(), rig.id)
	if gerr != nil || m.OAuth == nil || m.OAuth.Status.State != credential.OAuthNeedsReauth {
		t.Errorf("credential status = %+v (%v), want needs_reauth", m.OAuth, gerr)
	}
	// A later call does not refresh again: the grant waits for its owner.
	if _, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`)); !errors.Is(err, tools.ErrRunCredentialUnavailable) {
		t.Errorf("a call after needs_reauth: %v", err)
	}
	if n := rig.f.Count("token") - tokensBefore; n != 1 {
		t.Errorf("%d token requests after needs_reauth, want still one", n)
	}

	// Never a token in the error or the log.
	logMu.Lock()
	logged := logs.String()
	logMu.Unlock()
	for _, s := range rig.f.Secrets() {
		if strings.Contains(err.Error(), s) {
			t.Error("the error quotes a token")
		}
		if strings.Contains(logged, s) {
			t.Error("the log quotes a token")
		}
	}
}

func TestMCPHTTPClient_OAuth401WithoutInvalidTokenDoesNotRefresh(t *testing.T) {
	for _, challenge := range []string{"", `Bearer realm="docs"`, `Bearer error="insufficient_scope"`, `Basic error="invalid_token"`} {
		rig := newOAuthRig(t)
		c := rig.initialized(t)
		tokensBefore := rig.f.Count("token")
		rig.set(func(rig *oauthRig) { rig.challenge = challenge })
		rig.revoke(rig.f.LastAccess())
		_, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`))
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("challenge %q: err = %v, want the 401 as today", challenge, err)
		}
		if n := rig.f.Count("token") - tokensBefore; n != 0 {
			t.Errorf("challenge %q: %d token requests, want none", challenge, n)
		}
		if n := rig.count("tools/call"); n != 1 {
			t.Errorf("challenge %q: %d tools/call, want one", challenge, n)
		}
	}
}

func TestMCPHTTPClient_InvalidTokenOnNonOAuthServerIsNotRetried(t *testing.T) {
	rig := newOAuthRig(t)
	tokensBefore := rig.f.Count("token")
	c := rig.client(t, "Bearer a-static-test-value")
	rig.revoke("a-static-test-value")
	_, err := c.Call(rigCtx(), "initialize", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want the 401 as today", err)
	}
	if n := rig.count("initialize"); n != 1 {
		t.Errorf("%d initialize requests, want one", n)
	}
	if n := rig.f.Count("token") - tokensBefore; n != 0 {
		t.Errorf("%d token requests, want none", n)
	}
}

func TestMCPHTTPClient_OAuthRefreshReinitializesATokenBoundSession(t *testing.T) {
	rig := newOAuthRig(t)
	rig.set(func(rig *oauthRig) { rig.tokenBound = true })
	c := rig.initialized(t)
	rig.revoke(rig.f.LastAccess())

	res, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("CallTool: %v (requests %v)", err, rig.methods())
	}
	if n := rig.count("initialize"); n != 2 {
		t.Errorf("%d initialize requests, want the handshake and one re-initialize (requests %v)", n, rig.methods())
	}
	if !c.Healthy() {
		t.Error("the client was marked dead")
	}
	// The new session serves later calls.
	if _, err := mcp.CallTool(rigCtx(), c, "lookup", json.RawMessage(`{}`)); err != nil {
		t.Errorf("a later call: %v", err)
	}
}

func TestMCPHTTPClient_OAuthServerUsableAgainAfterReconnect(t *testing.T) {
	rig := newOAuthRig(t)
	pool := mcp.NewPool(func(string, string) (mcp.Caller, error) {
		return rig.client(t, "Bearer $oauth:docs"), nil
	}, nil, nil)
	t.Cleanup(pool.Close)
	ctx := rigCtx()
	tool := mcp.NewTool(pool, "docs", mcp.ToolDescriptor{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)})

	// Every token is refused: the first handshake fails, the credential
	// needs its owner, and the tool result says so.
	rig.set(func(rig *oauthRig) { rig.revokeAll = true })
	rig.revoke(rig.f.LastAccess())
	res, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil || !res.IsError || res.Error == nil || res.Error.Retryable || !strings.Contains(res.Text, "oauth_connect_start") {
		t.Fatalf("result %+v err %v, want a non-retryable reconnect refusal", res, err)
	}
	for _, s := range rig.f.Secrets() {
		if strings.Contains(res.Text, s) {
			t.Error("the tool result quotes a token")
		}
	}
	if m, _ := rig.engine.Get(context.Background(), rig.id); m.OAuth.Status.State != credential.OAuthNeedsReauth {
		t.Fatalf("state %q, want needs_reauth", m.OAuth.Status.State)
	}

	// The owner reconnects and the server accepts tokens again: the next
	// call reaches it, with no restart.
	rig.set(func(rig *oauthRig) { rig.revokeAll = false })
	rig.connect(t)
	res, err = tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("after reconnect: %+v %v", res, err)
	}
}

// lockedWriter serialises writes to w.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
